package main

import (
	"adaptive-load/agent"
	"adaptive-load/controller"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doReq(h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("bad json %q: %v", w.Body.String(), err)
	}
}

// ------------------------------------------------------------------------------- event log
func TestEventLogIsBoundedAndSupportsSince(t *testing.T) {
	l := NewEventLog(5)
	for i := 0; i < 12; i++ {
		l.Add("note", "", "e")
	}
	evs, last := l.Since(0)
	if len(evs) != 5 || last != 12 || evs[0].ID != 8 {
		t.Fatalf("ring not bounded: %d events, last %d, first id %d", len(evs), last, evs[0].ID)
	}
	if tail, _ := l.Since(10); len(tail) != 2 {
		t.Fatalf("since filter: %d", len(tail))
	}
	e := l.Add("note", "", strings.Repeat("x", 1000))
	if len(e.Detail) != maxEventDetail {
		t.Fatalf("detail not truncated: %d", len(e.Detail))
	}
	var nilLog *EventLog
	nilLog.Add("note", "", "safe on nil")
}

func TestPolicySwitchHealthToggleAndSafetyEmitEvents(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	h := lbHandler(r.pool, NewEventBus())
	_ = r.pool.SetStrategyByName("Least-Connections")
	doReq(h, http.MethodPost, "/api/backends/toggle", `{"name":"Backend-2"}`)
	r.mgr.SafetyFallback("test reason", t0)
	base := policy.ActionRoundRobin
	_ = r.mgr.Kill(true, &base, t0)
	_ = r.mgr.Kill(false, nil, t0)
	var out struct {
		Events []Event `json:"events"`
	}
	decodeJSON(t, doReq(h, http.MethodGet, "/api/events/log", ""), &out)
	kinds := map[string]bool{}
	for _, e := range out.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"policy", "health", "safety", "kill", "resume"} {
		if !kinds[k] {
			t.Errorf("missing %q event: %+v", k, out.Events)
		}
	}
}

func TestMarkerEndpointValidatesAndLogs(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	h := lbHandler(r.pool, NewEventBus())
	if w := doReq(h, http.MethodPost, "/api/events/marker", `{"kind":"fault","backend":"Backend-2","detail":"{\"delay_ms\":150}"}`); w.Code != 200 {
		t.Fatalf("valid marker rejected: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{"kind":"policy","detail":"spoof"}`, `{"kind":"safety"}`, `not json`,
		`{"kind":"note","detail":"` + strings.Repeat("x", 300) + `"}`, `{"kind":"note","backend":"` + strings.Repeat("b", 80) + `"}`} {
		if w := doReq(h, http.MethodPost, "/api/events/marker", bad); w.Code != 400 {
			t.Errorf("%.40s should be 400, got %d", bad, w.Code)
		}
	}
	var out struct {
		Events []Event `json:"events"`
		Last   uint64  `json:"last_id"`
	}
	decodeJSON(t, doReq(h, http.MethodGet, "/api/events/log", ""), &out)
	faults := 0
	for _, e := range out.Events {
		if e.Kind == "fault" && e.Backend == "Backend-2" {
			faults++
		}
	}
	if faults != 1 {
		t.Fatalf("expected exactly the one valid marker, got %d (events %+v)", faults, out.Events)
	}
	var again struct {
		Events []Event `json:"events"`
	}
	decodeJSON(t, doReq(h, http.MethodGet, "/api/events/log?since="+numStr(out.Last), ""), &again)
	if len(again.Events) != 0 {
		t.Fatal("since=<last> must return nothing")
	}
}

func numStr(v uint64) string { b, _ := json.Marshal(v); return string(b) }

// ----------------------------------------------------------------------------------- history
func snapshotWith(rps, errPerSec, p95 float64) *metrics.MetricsSnapshot {
	tel := newFakeTel("Round-Robin")
	s := tel.snap
	s.Timestamp = t0
	s.GlobalTraffic.CurrentRPS.Raw, s.GlobalTraffic.ErrorRate.Raw = rps, errPerSec
	s.GlobalTraffic.DurationStats.P95Ms, s.GlobalTraffic.DurationStats.P99Ms = p95, p95*1.5
	s.Backends["Backend-1"].RPS.Raw = rps * 0.5
	s.Backends["Backend-1"].ErrorRate.Raw = errPerSec
	s.Backends["Backend-1"].Latency.P95Ms = p95
	s.Backends["Backend-2"].RPS.Raw = rps * 0.3
	s.Backends["Backend-3"].RPS.Raw = rps * 0.2
	return s
}

func TestHistoryPointConvertsErrorsPerSecondToFraction(t *testing.T) {
	p := pointFrom(snapshotWith(200, 20, 80), "Round-Robin", 3)
	if p.RPS != 200 || p.ErrorRate != 0.1 || p.P95Ms != 80 || p.PolicyVersion != 3 || p.T != t0.UnixMilli() {
		t.Fatalf("%+v", p)
	}
	if b := p.Backends["Backend-1"]; b.RPS != 100 || b.ErrorRate != 0.2 || !b.Healthy {
		t.Fatalf("backend point %+v", b)
	}
	if pointFrom(snapshotWith(0, 5, 0), "x", 1).ErrorRate != 0 {
		t.Fatal("zero traffic must not divide by zero")
	}
	if pointFrom(snapshotWith(10, 99, 0), "x", 1).ErrorRate != 1 {
		t.Fatal("fraction is capped at 1")
	}
}

func TestHistoryRingBoundedAndEndpoint(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	for i := 0; i < historyCap+25; i++ {
		p := pointFrom(snapshotWith(100, 0, 20), "Round-Robin", 1)
		p.T = int64(1000 + i)
		r.pool.History().Add(p)
	}
	pts := r.pool.History().Since(0)
	if len(pts) != historyCap || pts[0].T != int64(1000+25) {
		t.Fatalf("history not bounded: %d points, first %d", len(pts), pts[0].T)
	}
	h := lbHandler(r.pool, NewEventBus())
	var out struct {
		Points []HistPoint `json:"points"`
	}
	decodeJSON(t, doReq(h, http.MethodGet, "/api/history?since="+numStr(uint64(1000+historyCap+20)), ""), &out)
	if len(out.Points) != 4 {
		t.Fatalf("since filter returned %d points", len(out.Points))
	}
	if w := doReq(h, http.MethodGet, "/api/history?since=99999999999", ""); !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Fatalf("an empty history must encode as []: %s", w.Body.String())
	}
}

// -------------------------------------------------------------------------------- dashboard
func TestDashboardEffectiveWeightsPerStrategy(t *testing.T) {
	r := newRig(t, "Smooth-Weighted-Round-Robin", fastMgrCfg())
	snap := snapshotWith(300, 3, 25)
	d := buildDashboard(r.pool, snap, t0)
	if d.Strategy != "Smooth-Weighted-Round-Robin" || len(d.Backends) != 3 || d.Backends[0].Name != "Backend-1" {
		t.Fatalf("%+v", d)
	}
	sum := 0.0
	for _, b := range d.Backends {
		if b.EffectiveWeight == nil || !strings.Contains(b.WeightBasis, "policy weight") {
			t.Fatalf("static SWRR must expose policy weights: %+v", b)
		}
		sum += *b.EffectiveWeight
	}
	if sum < 0.999 || sum > 1.001 || *d.Backends[0].EffectiveWeight != 0.5 {
		t.Fatalf("weights not normalized capacity shares: %v", sum)
	}
	// adaptive weights published by the controller/AI appear as the effective weights
	_ = r.pool.ApplyAction(policy.ActionRuleAdaptive)
	r.pool.PublishAdaptiveWeightsReason([]string{"Backend-1", "Backend-2", "Backend-3"}, []float64{0.2, 0.3, 0.5}, "test")
	d = buildDashboard(r.pool, snap, t0)
	if *d.Backends[2].EffectiveWeight != 0.5 || d.Policy.Reason != "test" {
		t.Fatalf("adaptive weights not shown: %+v", d.Backends[2])
	}
	// weight-free strategies must not invent a weight
	_ = r.pool.SetStrategyByName("Round-Robin")
	d = buildDashboard(r.pool, snap, t0)
	for _, b := range d.Backends {
		if b.EffectiveWeight != nil || !strings.HasPrefix(b.WeightBasis, "not used by") {
			t.Fatalf("round robin has no weights: %+v", b)
		}
	}
	// observed share is real traffic, adds up to 1, and per-backend rows carry the metrics
	share := 0.0
	for _, b := range d.Backends {
		share += b.ObservedShare
	}
	if share < 0.999 || share > 1.001 || d.Backends[0].RPS != 150 || d.Backends[0].P95Ms != 25 || d.Backends[0].ErrorRate != 0.02 {
		t.Fatalf("per-backend metrics wrong: share=%v %+v", share, d.Backends[0])
	}
	if d.Global.ErrorRate != 0.01 {
		t.Fatalf("global error fraction %v", d.Global.ErrorRate)
	}
}

func TestDashboardLabelsHostResourcesHonestlyAndHandlesMissingCPU(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	snap := snapshotWith(100, 0, 20)
	snap.Host.CPUUtilization = metrics.NewOptionalFloat(37.5)
	snap.Host.MemoryTotalBytes, snap.Host.MemoryUsedBytes = 1000, 250
	d := buildDashboard(r.pool, snap, t0)
	b := d.Backends[0]
	if b.ResourceScope != "host" || b.CPUPct == nil || *b.CPUPct != 37.5 || b.MemoryPct == nil || *b.MemoryPct != 25 {
		t.Fatalf("%+v", b)
	}
	if !strings.Contains(d.Host.Note, "host-level") {
		t.Fatal("the host-level limitation must be stated")
	}
	snap.Host.CPUUtilization = metrics.MissingFloat()
	if buildDashboard(r.pool, snap, t0).Backends[0].CPUPct != nil {
		t.Fatal("a missing CPU reading must stay missing (null), never become 0")
	}
}

func TestFallbackIndicatorKillSwitchAndSafetyLockout(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	r.pool.SetAgent(r.ctl)
	snap := snapshotWith(100, 0, 20)
	now := time.Now()
	d := buildDashboard(r.pool, snap, now)
	if !d.Agent.Enabled || !d.Agent.AIActive || d.Fallback.Active || d.Fallback.Kind != "none" {
		t.Fatalf("healthy AI: %+v %+v", d.Agent, d.Fallback)
	}
	r.mgr.SafetyFallback("error_fraction=0.20>=0.10", now)
	d = buildDashboard(r.pool, snap, now)
	if !d.Fallback.Active || d.Fallback.Kind != "safety_lockout" || d.Fallback.Until == nil ||
		!strings.Contains(d.Fallback.Reason, "error_fraction") || !d.Fallback.AtBaseline {
		t.Fatalf("lockout: %+v", d.Fallback)
	}
	base := policy.ActionStaticSWRR
	_ = r.mgr.Kill(true, &base, now)
	d = buildDashboard(r.pool, snap, now)
	if d.Fallback.Kind != "kill_switch" || d.Agent.AIActive || !d.Agent.Killed {
		t.Fatalf("kill: %+v %+v", d.Agent, d.Fallback)
	}
	_ = r.mgr.Kill(false, nil, now)
	d = buildDashboard(r.pool, snap, now.Add(2*time.Minute)) // lockout expired
	if d.Fallback.Active || !d.Agent.AIActive {
		t.Fatalf("resumed after lockout expiry: %+v %+v", d.Agent, d.Fallback)
	}
}

func TestDashboardWithoutAgentOrCollector(t *testing.T) {
	pool := newPolicyPool(t)
	h := lbHandler(pool, NewEventBus())
	if w := doReq(h, http.MethodGet, "/api/dashboard", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no collector -> 503, got %d", w.Code)
	}
	col := metrics.NewMetricsCollector(10 * time.Second)
	pool.SetCollector(col)
	for _, b := range pool.GetAllBackends() {
		col.RegisterBackend(b.Name, b.URL.String(), b.GetWeight(), 2, 1<<20, 100)
	}
	var d DashboardResponse
	decodeJSON(t, doReq(h, http.MethodGet, "/api/dashboard", ""), &d)
	if d.Agent.Enabled || d.Fallback.Active || len(d.Backends) != 3 {
		t.Fatalf("%+v", d)
	}
}

// ---------------------------------------------------------------- learner proxy and reset
func fakeAgentServer(t *testing.T, resets *int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/learner" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"learner":{"name":"linucb"},"confidence":{"available":false},"limit":"` + r.URL.Query().Get("limit") + `"}`))
		case r.URL.Path == "/v1/reset" && r.Method == http.MethodPost:
			*resets++
			_, _ = w.Write([]byte(`{"status":"reset","resets":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestLearnerProxyPassesThroughAndFailsSoft(t *testing.T) {
	resets := 0
	srv := fakeAgentServer(t, &resets)
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.pool.SetAgent(NewAgentController(r.pool, r.tel, r.mgr, agent.NewClient(srv.URL, time.Second), DefaultAgentConfig()))
	h := lbHandler(r.pool, NewEventBus())
	w := doReq(h, http.MethodGet, "/api/agent/learner?limit=7", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"linucb"`) || !strings.Contains(w.Body.String(), `"limit":"7"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	srv.Close()
	var out map[string]any
	decodeJSON(t, doReq(h, http.MethodGet, "/api/agent/learner", ""), &out)
	if out["available"] != false || out["error"] == nil {
		t.Fatalf("an unreachable agent must yield available:false, got %v", out)
	}
	// agent not configured
	bare := lbHandler(newPolicyPool(t), NewEventBus())
	decodeJSON(t, doReq(bare, http.MethodGet, "/api/agent/learner", ""), &out)
	if out["available"] != false {
		t.Fatalf("%v", out)
	}
}

func TestResetToBaselineAppliesBaselineClearsLockoutAndResetsModel(t *testing.T) {
	resets := 0
	srv := fakeAgentServer(t, &resets)
	r := newRig(t, "Least-Connections", fastMgrCfg())
	ac := NewAgentController(r.pool, r.tel, r.mgr, agent.NewClient(srv.URL, time.Second), DefaultAgentConfig())
	r.pool.SetAgent(ac)
	ac.pending = &pendingWindow{decisionID: "x"}
	r.mgr.SafetyFallback("test", time.Now())
	_ = r.pool.SetStrategyByName("Least-Connections") // AI had moved away from the baseline
	h := lbHandler(r.pool, NewEventBus())
	var out map[string]any
	decodeJSON(t, doReq(h, http.MethodPost, "/api/agent/reset", `{"archive":true}`), &out)
	if out["status"] != "ok" || out["model_reset"] != true || resets != 1 {
		t.Fatalf("%v resets=%d", out, resets)
	}
	if r.pool.GetStrategy().Name() != "Smooth-Weighted-Round-Robin" || r.pool.Policies().Current().Reason != "reset_to_baseline" {
		t.Fatalf("baseline not applied: %s / %s", r.pool.GetStrategy().Name(), r.pool.Policies().Current().Reason)
	}
	if !r.mgr.LockedUntil().IsZero() || ac.pending != nil {
		t.Fatal("lockout and pending window must be cleared")
	}
	// the policy is reset even if the agent cannot be reached; the response says the model was NOT reset
	srv.Close()
	decodeJSON(t, doReq(h, http.MethodPost, "/api/agent/reset", `{}`), &out)
	if out["model_reset"] != false || out["agent_error"] == nil {
		t.Fatalf("%v", out)
	}
	if w := doReq(lbHandler(newPolicyPool(t), NewEventBus()), http.MethodPost, "/api/agent/reset", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no agent -> 503, got %d", w.Code)
	}
}

func TestResetWhenAlreadyAtBaselinePublishesAFreshPolicy(t *testing.T) {
	r := newRig(t, "Smooth-Weighted-Round-Robin", fastMgrCfg()) // baseline (static SWRR) already active
	before := r.pool.Policies().Current().Version
	oldStrategy := r.pool.GetStrategy()
	if err := r.mgr.ResetToBaseline(time.Now()); err != nil {
		t.Fatal(err)
	}
	p := r.pool.Policies().Current()
	if p.Version <= before || p.Reason != "reset_to_baseline" || r.pool.GetStrategy() == oldStrategy {
		t.Fatalf("reset must publish a new policy and a new strategy instance: v%d->v%d reason=%q", before, p.Version, p.Reason)
	}
}

func TestRuleControllerSummaryAppearsInDashboard(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	rc := controller.New(controller.DefaultConfig())
	r.pool.SetController(rc)
	d := buildDashboard(r.pool, snapshotWith(100, 0, 20), t0)
	if !d.Controller.Present {
		t.Fatal("rule-based controller status missing")
	}
}
