package dummy

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func start(t *testing.T, mod func(*Config)) (*Backend, string) {
	t.Helper()
	port := freePort(t)
	cfg := DefaultConfig("B", port)
	cfg.ControlPort = freePort(t)
	cfg.ServiceTime = 2 * time.Millisecond
	if mod != nil {
		mod(&cfg)
	}
	b, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	return b, "http://127.0.0.1:" + port
}

func ctl(b *Backend) string { return b.ControlURL() + "/__fault" }

func get(url string) (int, string, error) {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b), nil
}

func post(url, body string) (int, error) {
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()
	return r.StatusCode, nil
}

func TestServesAndReportsFaults(t *testing.T) {
	b, u := start(t, nil)
	if code, body, err := get(u + "/work"); err != nil || code != 200 || !strings.Contains(body, "Response from B") {
		t.Fatalf("%d %q %v", code, body, err)
	}
	if code, body, _ := get(ctl(b)); code != 200 || !strings.Contains(body, `"error_rate":0`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestErrorInjectionRateAndSeedReproducible(t *testing.T) {
	run := func() string {
		b, u := start(t, nil)
		if c, _ := post(ctl(b), `{"error_rate":0.3,"seed":42}`); c != 200 {
			t.Fatal("fault rejected")
		}
		var pattern strings.Builder
		fails := 0
		for i := 0; i < 400; i++ {
			code, _, _ := get(u + "/w")
			if code == 503 {
				fails++
				pattern.WriteByte('x')
			} else {
				pattern.WriteByte('.')
			}
		}
		if fails < 90 || fails > 150 { // 30% of 400 = 120
			t.Fatalf("error rate off: %d/400", fails)
		}
		return pattern.String()
	}
	if a, b := run(), run(); a != b {
		t.Fatal("same seed must reproduce the same error pattern")
	}
}

func TestLatencyInjection(t *testing.T) {
	b, u := start(t, nil)
	_, _ = post(ctl(b), `{"delay_ms":150}`)
	s := time.Now()
	_, _, _ = get(u + "/w")
	if d := time.Since(s); d < 140*time.Millisecond {
		t.Fatalf("delay not applied: %v", d)
	}
	_, _ = post(ctl(b), `{"delay_ms":0}`)
	s = time.Now()
	_, _, _ = get(u + "/w")
	if time.Since(s) > 100*time.Millisecond {
		t.Fatal("delay not cleared")
	}
}

func TestDownRefusesConnectionsAndRecoversRemotely(t *testing.T) {
	b, u := start(t, nil)
	if c, _ := post(ctl(b), `{"down":true}`); c != 200 {
		t.Fatal("down rejected")
	}
	if _, _, err := get(u + "/health"); err == nil {
		t.Fatal("a downed backend must refuse connections")
	}
	hp := strings.TrimPrefix(u, "http://")
	if conn, err := net.DialTimeout("tcp", hp, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("TCP health check would still succeed")
	}
	// the control port stays up, so the backend can be recovered remotely
	if c, _ := post(ctl(b), `{"down":false}`); c != 200 {
		t.Fatal("recovery rejected")
	}
	if code, _, err := get(u + "/health"); err != nil || code != 200 {
		t.Fatalf("should have recovered: %d %v", code, err)
	}
}

func TestRecoveryViaAPI(t *testing.T) {
	b, u := start(t, nil)
	if err := b.apply(faultPatch{Down: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := get(u + "/health"); err == nil {
		t.Fatal("should be down")
	}
	if err := b.apply(faultPatch{Down: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if code, _, err := get(u + "/health"); err != nil || code != 200 {
		t.Fatalf("should have recovered: %d %v", code, err)
	}
}

func TestBoundedCapacityShedsLoad(t *testing.T) {
	_, u := start(t, func(c *Config) {
		c.Concurrency, c.ServiceTime, c.QueueTimeout = 1, 80*time.Millisecond, 20*time.Millisecond
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, _ := get(u + "/w")
			mu.Lock()
			codes[c]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[503] == 0 || codes[200] == 0 {
		t.Fatalf("overload should shed some requests and serve some: %v", codes)
	}
}

func TestCPUBurnerStartsAndStops(t *testing.T) {
	b, _ := start(t, nil)
	if err := b.apply(faultPatch{CPUBurn: ptr(0.5), CPUThreads: ptr(1)}); err != nil {
		t.Fatal(err)
	}
	if b.Faults().CPUBurn != 0.5 {
		t.Fatal("cpu burn not recorded")
	}
	if err := b.apply(faultPatch{CPUBurn: ptr(0.0)}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidFaultValuesRejected(t *testing.T) {
	b, _ := start(t, nil)
	for _, body := range []string{`{"error_rate":2}`, `{"cpu_burn":-1}`, `{"delay_ms":-5}`, `{"concurrency":0}`, `not json`} {
		if c, _ := post(ctl(b), body); c != 400 {
			t.Errorf("%s -> %d, want 400", body, c)
		}
	}
}
