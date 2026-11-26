package main

import (
	"adaptive-load/dummy"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ------------------------------------------------------------------------------ helpers
func securedHandler(t *testing.T, mutate func(*SecurityConfig)) (http.HandlerFunc, *ServerPool) {
	t.Helper()
	pool := newPolicyPool(t)
	sec := SecurityFromEnv()
	if mutate != nil {
		mutate(sec)
	}
	pool.SetSecurity(sec)
	return lbHandler(pool, NewEventBus()), pool
}

func req(method, path, body string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "localhost:8080"
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
		} else if k == "RemoteAddr" {
			r.RemoteAddr = v
		} else if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	return r
}

func serve(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// ------------------------------------------------------------------- management API security
func TestManagementAPIIsLoopbackOnlyByDefault(t *testing.T) {
	h, _ := securedHandler(t, nil)
	if w := serve(h, req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, nil)); w.Code != 200 {
		t.Fatalf("loopback client must be allowed: %d %s", w.Code, w.Body.String())
	}
	for _, remote := range []string{"192.0.2.10:4444", "203.0.113.7:1", "[2001:db8::1]:80"} {
		if w := serve(h, req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, map[string]string{"RemoteAddr": remote})); w.Code != 403 {
			t.Errorf("remote %s must be refused for mutating calls, got %d", remote, w.Code)
		}
	}
	// reads stay available (the dashboard is read-only for remote viewers if the operator exposes it)
	if w := serve(h, req("GET", "/api/status", "", map[string]string{"RemoteAddr": "192.0.2.10:4444"})); w.Code != 200 {
		t.Fatalf("GET status: %d", w.Code)
	}
	// the proxy path is not subject to the management rules
	if w := serve(h, req("GET", "/some/page", "", map[string]string{"RemoteAddr": "192.0.2.10:4444"})); w.Code == 403 {
		t.Fatal("proxied traffic from remote clients must not be blocked by the admin rules")
	}
}

func TestAdminTokenModeRequiresBearerEvenFromLoopback(t *testing.T) {
	h, _ := securedHandler(t, func(s *SecurityConfig) { s.AdminToken = "s3cret-token" })
	body := `{"strategy":"Round-Robin"}`
	if w := serve(h, req("POST", "/api/strategy", body, nil)); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d", w.Code)
	}
	for _, bad := range []string{"Bearer wrong", "s3cret-token", "Basic s3cret-token", "Bearer "} {
		if w := serve(h, req("POST", "/api/strategy", body, map[string]string{"Authorization": bad})); w.Code != 401 {
			t.Errorf("%q should be 401, got %d", bad, w.Code)
		}
	}
	if w := serve(h, req("POST", "/api/strategy", body, map[string]string{"Authorization": "Bearer s3cret-token"})); w.Code != 200 {
		t.Fatalf("valid token: %d %s", w.Code, w.Body.String())
	}
	// a valid token also allows remote administration
	if w := serve(h, req("POST", "/api/strategy", body, map[string]string{"Authorization": "Bearer s3cret-token", "RemoteAddr": "192.0.2.10:1"})); w.Code != 200 {
		t.Fatalf("remote with token: %d", w.Code)
	}
}

func TestCrossSiteAndDNSRebindingRequestsAreRefused(t *testing.T) {
	h, _ := securedHandler(t, nil)
	body := `{"kill":true}`
	// a web page on another site posting to localhost (CSRF)
	for _, origin := range []string{"https://evil.example", "http://localhost:9999", "null"} {
		if w := serve(h, req("POST", "/api/agent/kill", body, map[string]string{"Origin": origin})); w.Code != 403 {
			t.Errorf("origin %q must be refused, got %d", origin, w.Code)
		}
		if w := serve(h, req("GET", "/api/status", "", map[string]string{"Origin": origin})); w.Code != 403 {
			t.Errorf("cross-site read from %q must be refused, got %d", origin, w.Code)
		}
	}
	// DNS rebinding: attacker's name resolves to 127.0.0.1, so Host (and Origin) carry the attacker's name
	if w := serve(h, req("GET", "/api/status", "", map[string]string{"Host": "attacker.example:8080"})); w.Code != 403 {
		t.Errorf("unknown Host must be refused, got %d", w.Code)
	}
	if w := serve(h, req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, map[string]string{"Host": "attacker.example:8080", "Origin": "http://attacker.example:8080"})); w.Code != 403 {
		t.Errorf("rebinding POST must be refused, got %d", w.Code)
	}
	// legitimate origins: the Vite dev server and same-origin
	for _, origin := range []string{"http://localhost:5173", "http://localhost:8080"} {
		w := serve(h, req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, map[string]string{"Origin": origin}))
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("origin %s: %d acao=%q", origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	// never a wildcard
	w := serve(h, req("GET", "/api/status", "", map[string]string{"Origin": "http://localhost:5173"}))
	if w.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("wildcard CORS must not be used")
	}
	// preflight: allowed origin only
	if w := serve(h, req("OPTIONS", "/api/policy", "", map[string]string{"Origin": "http://localhost:5173"})); w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
	if w := serve(h, req("OPTIONS", "/api/policy", "", map[string]string{"Origin": "https://evil.example"})); w.Code != 403 {
		t.Fatalf("preflight from evil origin: %d", w.Code)
	}
}

func TestPostRequiresJSONContentType(t *testing.T) {
	h, _ := securedHandler(t, nil)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
		r := req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, map[string]string{"Content-Type": ct})
		if w := serve(h, r); w.Code != 415 {
			t.Errorf("Content-Type %q must be 415, got %d", ct, w.Code)
		}
	}
	if w := serve(h, req("POST", "/api/strategy", `{"strategy":"Round-Robin"}`, map[string]string{"Content-Type": "application/json; charset=utf-8"})); w.Code != 200 {
		t.Fatalf("json with charset: %d", w.Code)
	}
	if w := serve(h, req("PUT", "/api/strategy", `{}`, nil)); w.Code != 405 {
		t.Fatalf("PUT must be 405, got %d", w.Code)
	}
}

// ------------------------------------------------------------------ strict decoding and limits
func TestStrictBodiesRejectUnknownFieldsNaNTrailingDataAndOversize(t *testing.T) {
	h, pool := securedHandler(t, nil)
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"unknown field", "/api/strategy", `{"strategy":"Round-Robin","extra":1}`, 400},
		{"NaN literal", "/api/backends/weight", `{"name":"Backend-1","weight":NaN}`, 400},
		{"Infinity literal", "/api/backends/weight", `{"name":"Backend-1","weight":Infinity}`, 400},
		{"trailing data", "/api/strategy", `{"strategy":"Round-Robin"}{"strategy":"x"}`, 400},
		{"array not object", "/api/strategy", `["Round-Robin"]`, 400},
		{"null", "/api/strategy", `null`, 400},
		{"truncated", "/api/strategy", `{"strategy":"Round`, 400},
		{"too large", "/api/strategy", `{"strategy":"` + strings.Repeat("a", 5000) + `"}`, 413},
		{"unknown strategy name", "/api/strategy", `{"strategy":"Random-Walk"}`, 400},
		{"unknown action id", "/api/policy", `{"action":42}`, 400},
		{"policy unknown field", "/api/policy", `{"action":1,"admin":true}`, 400},
		{"kill unknown field", "/api/agent/kill", `{"kill":true,"x":1}`, 400},
		{"marker wrong kind", "/api/events/marker", `{"kind":"safety","detail":"spoof"}`, 400},
	}
	for _, c := range cases {
		if w := serve(h, req("POST", c.path, c.body, nil)); w.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.want, strings.TrimSpace(w.Body.String()))
		}
	}
	if pool.GetStrategy().Name() != "Round-Robin" {
		t.Fatal("a rejected request must not change the strategy")
	}
	// an empty body is valid only where the endpoint tolerates it (reset has no required fields)
	if w := serve(h, req("POST", "/api/agent/reset", ``, nil)); w.Code == 400 || w.Code == 413 {
		t.Fatalf("empty body for reset must pass the guard: %d", w.Code)
	}
}

func TestWeightsAndBackendIdentifiersAreValidated(t *testing.T) {
	h, pool := securedHandler(t, nil)
	b := pool.GetBackendByName("Backend-1")
	before := b.GetWeight()
	for _, w := range []string{"-5", "0", "1001", "100000", "1.5", `"3"`} {
		body := `{"name":"Backend-1","weight":` + w + `}`
		if resp := serve(h, req("POST", "/api/backends/weight", body, nil)); resp.Code != 400 {
			t.Errorf("weight %s must be rejected, got %d", w, resp.Code)
		}
	}
	if b.GetWeight() != before {
		t.Fatalf("a rejected weight changed the backend: %d -> %d", before, b.GetWeight())
	}
	if resp := serve(h, req("POST", "/api/backends/weight", `{"name":"Backend-1","weight":7}`, nil)); resp.Code != 200 || b.GetWeight() != 7 {
		t.Fatalf("valid weight: %d", resp.Code)
	}
	if resp := serve(h, req("POST", "/api/backends/weight", `{"name":"Ghost","weight":3}`, nil)); resp.Code != 404 {
		t.Fatalf("unknown backend: %d", resp.Code)
	}
	if resp := serve(h, req("POST", "/api/backends/toggle", `{"name":"Ghost"}`, nil)); resp.Code != 404 {
		t.Fatalf("unknown backend toggle: %d", resp.Code)
	}
	if resp := serve(h, req("POST", "/api/events/marker", `{"kind":"fault","backend":"Ghost","detail":"x"}`, nil)); resp.Code != 400 {
		t.Fatalf("marker for an unknown backend: %d", resp.Code)
	}
	if resp := serve(h, req("POST", "/api/events/marker", `{"kind":"fault","backend":"Backend-2","detail":"x"}`, nil)); resp.Code != 200 {
		t.Fatalf("valid marker: %d", resp.Code)
	}
}

func TestServerTimeoutsAreConfigured(t *testing.T) {
	s := newHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadHeaderTimeout > 30*time.Second || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<20 {
		t.Fatalf("missing slowloris/size protection: %+v", s)
	}
	if s.WriteTimeout != 0 {
		t.Fatal("WriteTimeout must stay unset: /api/events is a long-lived SSE stream")
	}
}

// ---------------------------------------------------------------------- privacy and boundedness
func TestClientAddressesAreMaskedInEventsAndLogsOmitPaths(t *testing.T) {
	for in, want := range map[string]string{"203.0.113.77": "203.0.113.0", "10.1.2.3": "10.1.2.0", "2001:db8:abcd:1234::1": "2001:db8:abcd::",
		"not-an-ip": "unknown", "": "unknown", "alice@example.com": "unknown"} {
		if got := maskClientIP(in); got != want {
			t.Errorf("maskClientIP(%q) = %q, want %q", in, got, want)
		}
	}
	h, pool := securedHandler(t, nil)
	bus := NewEventBus()
	h = lbHandler(pool, bus)
	ch, _ := bus.TrySubscribe(0)
	r := req("GET", "/secret/user/12345?token=abc", "", map[string]string{"X-Forwarded-For": "198.51.100.23"})
	serve(h, r)
	select {
	case data := <-ch:
		if strings.Contains(string(data), "198.51.100.23") || !strings.Contains(string(data), "198.51.100.0") {
			t.Fatalf("event stream leaked the client address: %s", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event published")
	}
	// operator opt-in shows the raw address
	h2, pool2 := securedHandler(t, func(s *SecurityConfig) { s.MaskClientIP = false })
	bus2 := NewEventBus()
	h2 = lbHandler(pool2, bus2)
	ch2, _ := bus2.TrySubscribe(0)
	serve(h2, req("GET", "/x", "", map[string]string{"X-Forwarded-For": "198.51.100.23"}))
	if data := <-ch2; !strings.Contains(string(data), "198.51.100.23") {
		t.Fatalf("LB_EXPOSE_CLIENT_IP must show the address: %s", data)
	}
}

func TestEventStreamSubscribersAreCapped(t *testing.T) {
	bus := NewEventBus()
	for i := 0; i < 3; i++ {
		if _, ok := bus.TrySubscribe(3); !ok {
			t.Fatalf("subscriber %d refused", i)
		}
	}
	if _, ok := bus.TrySubscribe(3); ok {
		t.Fatal("the 4th subscriber must be refused")
	}
	bus.Close()
	if bus.Count() != 0 {
		t.Fatal("Close must drop every subscriber")
	}
	if _, ok := bus.TrySubscribe(3); ok {
		t.Fatal("a closed bus must refuse subscribers")
	}
	// the HTTP endpoint answers 503 beyond the cap
	h, _ := securedHandler(t, func(s *SecurityConfig) { s.MaxSSE = 1 })
	srv := httptest.NewServer(h)
	defer srv.Close()
	open := func() (*http.Response, error) {
		rq, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
		return http.DefaultClient.Do(rq)
	}
	first, err := open()
	if err != nil || first.StatusCode != 200 {
		t.Fatalf("first stream: %v %v", err, first)
	}
	defer first.Body.Close()
	second, err := open()
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second stream beyond the cap: %d", second.StatusCode)
	}
}

func TestRotatingLogIsBounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.jsonl")
	r, err := newRotatingFile(p, 2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 200; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	var total int64
	files, _ := filepath.Glob(p + "*")
	for _, f := range files {
		st, _ := os.Stat(f)
		total += st.Size()
		if st.Size() > 2048+100 {
			t.Errorf("%s is %d bytes: not bounded", f, st.Size())
		}
	}
	if len(files) != 3 || total > 3*(2048+100) {
		t.Fatalf("expected the log plus 2 backups within the bound, got %d files / %d bytes", len(files), total)
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("older backups must be deleted")
	}
	if _, err := newRotatingFile(p, 10, 1); err == nil {
		t.Fatal("a tiny size limit must be refused")
	}
}

// ------------------------------------------------------------------ graceful shutdown (runLB)
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

type liveLB struct {
	base   string
	cancel context.CancelFunc
	done   chan error
	stops  []func()
}

// startLB starts runLB against three real (in-test) backends.
func startLB(t *testing.T, mod func(*lbOptions)) *liveLB {
	t.Helper()
	var specs []backendSpec
	var stops []func()
	for i := 1; i <= 3; i++ {
		port, ctrl := freeAddr(t), freeAddr(t)
		cfg := dummy.DefaultConfig(fmt.Sprintf("B%d", i), port)
		cfg.ControlPort, cfg.ServiceTime = ctrl, 5*time.Millisecond
		b, err := dummy.Start(cfg)
		if err != nil {
			t.Fatal(err)
		}
		stops = append(stops, b.Stop)
		specs = append(specs, backendSpec{Port: port, Name: fmt.Sprintf("B%d", i), Weight: i})
	}
	ready := make(chan string, 1)
	o := lbOptions{Addr: "127.0.0.1:0", Backends: specs, HealthEvery: 200 * time.Millisecond, Security: SecurityFromEnv(),
		Ready: ready, ShutdownGrace: 5 * time.Second}
	if mod != nil {
		mod(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runLB(ctx, o) }()
	select {
	case addr := <-ready:
		return &liveLB{base: "http://" + addr, cancel: cancel, done: done, stops: stops}
	case err := <-done:
		t.Fatalf("runLB exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("LB did not become ready")
	}
	return nil
}

func (l *liveLB) stop(t *testing.T) {
	t.Helper()
	l.cancel()
	select {
	case err := <-l.done:
		if err != nil {
			t.Fatalf("runLB returned %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runLB did not return after cancellation")
	}
	for _, s := range l.stops {
		s()
	}
}

func TestLBServesWithBaselineWhenTheAgentIsDown(t *testing.T) {
	t.Setenv("LB_AGENT_URL", "http://127.0.0.1:1") // nothing listens there
	t.Setenv("LB_AGENT_FAILURE_BACKOFF_S", "0.2")
	t.Setenv("LB_AGENT_STEP_S", "0.2")
	l := startLB(t, nil)
	defer l.stop(t)
	// traffic is served immediately, with the known-safe baseline, although the agent never answers
	for i := 0; i < 30; i++ {
		resp, err := http.Get(l.base + "/work")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("request %d: %v %v", i, err, resp)
		}
		resp.Body.Close()
	}
	var d DashboardResponse
	resp, _ := http.Get(l.base + "/api/dashboard")
	_ = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if d.Strategy != "Smooth-Weighted-Round-Robin" || d.Policy.Reason != "startup_safe_baseline" || !d.Agent.Enabled {
		t.Fatalf("baseline policy expected: %+v", d)
	}
}

func TestShutdownLetsInFlightRequestsFinishAndRefusesNewOnes(t *testing.T) {
	l := startLB(t, nil)
	var status atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(l.base + "/work?delay=700ms") // outlives the cancellation below
		if err == nil {
			status.Store(int32(resp.StatusCode))
			resp.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	l.cancel()
	if err := <-l.done; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for _, s := range l.stops {
		s()
	}
	if status.Load() != 200 {
		t.Fatalf("the in-flight request must complete during shutdown, got %d", status.Load())
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("shutdown took %v", time.Since(start))
	}
	if _, err := http.Get(l.base + "/work"); err == nil {
		t.Fatal("the server must refuse connections after shutdown")
	}
}

func TestOpenEventStreamsDoNotBlockShutdown(t *testing.T) {
	l := startLB(t, nil)
	resp, err := http.Get(l.base + "/api/events")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	go func() { _, _ = io.Copy(io.Discard, bufio.NewReader(resp.Body)) }()
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	l.cancel()
	select {
	case err := <-l.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("an open SSE stream held the shutdown open")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown with an SSE client took %v (grace period is 5 s: it was waiting for the stream)", time.Since(start))
	}
	resp.Body.Close()
	for _, s := range l.stops {
		s()
	}
}

func TestShutdownLeavesNoBackgroundGoroutines(t *testing.T) {
	t.Setenv("LB_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("LB_AGENT_STEP_S", "0.2")
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	l := startLB(t, nil)
	for i := 0; i < 40; i++ {
		resp, err := http.Get(l.base + "/work")
		if err == nil {
			resp.Body.Close()
		}
	}
	http.DefaultClient.CloseIdleConnections()
	l.stop(t)
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines before=%d after=%d; leaked stacks:\n%s", before, after, buf[:n])
}

// ---------------------------------------------------- concurrent policy swaps through the HTTP path
func TestConcurrentPolicySwapsWhileServingTrafficOverHTTP(t *testing.T) {
	l := startLB(t, func(o *lbOptions) { o.Security = SecurityFromEnv() })
	defer l.stop(t)
	post := func(path, body string) int {
		rq, _ := http.NewRequest("POST", l.base+path, strings.NewReader(body))
		rq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			return -1
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	var (
		stop              atomic.Bool
		failed, served    atomic.Int64
		swapWG, clientsWG sync.WaitGroup
	)
	for c := 0; c < 8; c++ { // concurrent clients
		clientsWG.Add(1)
		go func(c int) {
			defer clientsWG.Done()
			cl := &http.Client{Timeout: 5 * time.Second}
			for !stop.Load() {
				rq, _ := http.NewRequest("GET", l.base+"/work", nil)
				rq.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", c))
				resp, err := cl.Do(rq)
				if err != nil || resp.StatusCode != 200 {
					failed.Add(1)
				} else {
					served.Add(1)
				}
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(c)
	}
	// concurrent swappers: actions 0..2 + adaptive, kill/resume, reset, weight updates, markers
	swappers := []func(i int){
		func(i int) { post("/api/policy", fmt.Sprintf(`{"action":%d,"decision_id":"c-%d"}`, i%3, i)) },
		func(i int) { post("/api/policy", `{"action":4}`) },
		func(i int) { post("/api/strategy", `{"strategy":"Least-Connections"}`) },
		func(i int) { post("/api/backends/weight", fmt.Sprintf(`{"name":"B1","weight":%d}`, 1+i%9)) },
		func(i int) { post("/api/events/marker", `{"kind":"note","detail":"swap"}`) },
	}
	for _, f := range swappers {
		swapWG.Add(1)
		go func(f func(int)) {
			defer swapWG.Done()
			for i := 0; i < 60; i++ {
				f(i)
				time.Sleep(5 * time.Millisecond)
			}
		}(f)
	}
	swapWG.Wait()
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	clientsWG.Wait()

	if served.Load() < 200 {
		t.Fatalf("too little traffic served during the swaps: %d", served.Load())
	}
	if failed.Load() != 0 {
		t.Fatalf("%d of %d requests failed while policies were being swapped", failed.Load(), served.Load()+failed.Load())
	}
	// invariants after the storm: versions advanced, every connection counter returned to zero
	var st statusResponse
	resp, _ := http.Get(l.base + "/api/status")
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	for _, b := range st.Backends {
		if b.ActiveConns != 0 {
			t.Errorf("%s still has %d active connections: leak during policy swaps", b.Name, b.ActiveConns)
		}
	}
	var pol struct {
		Policy struct {
			Version uint64 `json:"version"`
		} `json:"policy"`
	}
	resp, _ = http.Get(l.base + "/api/policy")
	_ = json.NewDecoder(resp.Body).Decode(&pol)
	resp.Body.Close()
	if pol.Policy.Version < 20 {
		t.Errorf("policy version only reached %d: swaps did not apply", pol.Policy.Version)
	}
}

func TestDecisionLogUsesTheBoundedWriter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "go.jsonl")
	t.Setenv("LB_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("LB_AGENT_STEP_S", "0.1")
	t.Setenv("LB_AGENT_FAILURE_BACKOFF_S", "0.05")
	l := startLB(t, func(o *lbOptions) { o.DecisionLog, o.DecisionLogMB = p, 1 })
	for i := 0; i < 40; i++ {
		resp, err := http.Get(l.base + "/work")
		if err == nil {
			resp.Body.Close()
		}
	}
	time.Sleep(800 * time.Millisecond)
	l.stop(t)
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		t.Fatalf("decision log not written: %v", err)
	}
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("decision log must be owner-only, got %v", st.Mode().Perm())
	}
	var line map[string]any
	first := bytes.SplitN(mustRead(t, p), []byte("\n"), 2)[0]
	if err := json.Unmarshal(first, &line); err != nil {
		t.Fatalf("not JSONL: %v", err)
	}
	for k := range line {
		switch k {
		case "decision_id", "at", "latency_ms", "current_strategy", "proposed_strategy", "diagnostic", "model_version", "reason", "applied", "failure", "state_version", "mode":
		default:
			t.Errorf("unexpected field %q in the decision log", k)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
