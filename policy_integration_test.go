package main

import (
	"adaptive-load/controller"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"adaptive-load/strategy"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func newPolicyPool(t *testing.T) *ServerPool {
	t.Helper()
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	for _, c := range []struct {
		n string
		w int
	}{{"Backend-1", 3}, {"Backend-2", 2}, {"Backend-3", 1}} {
		u, _ := url.Parse("http://localhost:1" + c.n[len(c.n)-1:])
		pool.AddBackend(NewBackendNamedWithWeight(u, c.n, c.w))
	}
	return pool
}

func TestPoolPublishesPolicyOnStrategySwitch(t *testing.T) {
	pool := newPolicyPool(t)
	if pool.Policies().Current().Strategy != policy.RoundRobin {
		t.Fatal("initial policy should mirror initial strategy")
	}
	v0 := pool.Policies().Current().Version
	if err := pool.SetStrategyByName("Smooth-Weighted-Round-Robin"); err != nil {
		t.Fatal(err)
	}
	p := pool.Policies().Current()
	if p.Strategy != policy.StaticSWRR || p.Version <= v0 {
		t.Fatalf("policy not republished: %+v", p)
	}
	if len(p.Weights) != 3 || p.BackendIDs[0] != "Backend-1" || p.Weights[0] != 0.5 {
		t.Fatalf("static policy should carry normalized capacity weights in sorted order: %+v", p)
	}
	if err := pool.SetStrategyByName("nope"); err == nil {
		t.Fatal("unknown strategy must error")
	}
}

func TestApplyActionValidation(t *testing.T) {
	pool := newPolicyPool(t)
	if err := pool.ApplyAction(policy.ActionConsistentHash); err == nil {
		t.Fatal("CH must be rejected without affinity")
	}
	if err := pool.ApplyAction(policy.ActionRuleAdaptive); err == nil {
		t.Fatal("adaptive must be rejected without a controller")
	}
	if err := pool.ApplyAction(policy.ActionID(42)); err == nil {
		t.Fatal("unknown action must be rejected")
	}
	if pool.GetStrategy().Name() != "Round-Robin" {
		t.Fatal("rejected actions must not change the strategy")
	}
	pool.SetAffinityRequired(true)
	pool.SetController(controller.New(controller.DefaultConfig()))
	for _, a := range []policy.ActionID{0, 1, 2, 3, 4} {
		if err := pool.ApplyAction(a); err != nil {
			t.Fatalf("action %d should apply: %v", a, err)
		}
		want, _ := a.Strategy()
		if pool.GetStrategy().Name() != string(want) {
			t.Fatalf("action %d: strategy %s want %s", a, pool.GetStrategy().Name(), want)
		}
	}
}

func TestAdaptiveWeightsReachRequestPath(t *testing.T) {
	pool := newPolicyPool(t)
	pool.SetController(controller.New(controller.DefaultConfig()))
	ids := []string{"Backend-1", "Backend-2", "Backend-3"}

	if pool.PublishAdaptiveWeights(ids, []float64{0.2, 0.2, 0.6}) {
		t.Fatal("weights must not apply while another strategy is active (shadow mode)")
	}
	if err := pool.ApplyAction(policy.ActionRuleAdaptive); err != nil {
		t.Fatal(err)
	}
	if !pool.PublishAdaptiveWeights(ids, []float64{0.2, 0.2, 0.6}) {
		t.Fatal("weights should apply under the adaptive strategy")
	}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		counts[pool.GetNextPeer().Name]++
	}
	if counts["Backend-3"] != 600 || counts["Backend-1"] != 200 {
		t.Fatalf("dynamic weights not honored: %v", counts)
	}
	// Excluded by health independent of policy.
	pool.GetBackendByName("Backend-3").SetAlive(false)
	for i := 0; i < 50; i++ {
		if pool.GetNextPeer().Name == "Backend-3" {
			t.Fatal("unhealthy backend selected")
		}
	}
}

func TestPolicyVersionReachesMetrics(t *testing.T) {
	pool := newPolicyPool(t)
	col := metrics.NewMetricsCollector(10_000_000_000)
	pool.SetCollector(col)
	_ = pool.SetStrategyByName("Least-Connections")
	snap := col.CollectSnapshot()
	want := "v" + jsonNum(pool.Policies().Current().Version)
	if snap.Controller.PolicyVersion != want {
		t.Fatalf("policy version %q want %q", snap.Controller.PolicyVersion, want)
	}
}

func jsonNum(v uint64) string { b, _ := json.Marshal(v); return string(b) }

func TestPolicyAPI(t *testing.T) {
	pool := newPolicyPool(t)
	pool.SetController(controller.New(controller.DefaultConfig()))
	h := lbHandler(pool, NewEventBus())

	get := httptest.NewRecorder()
	h(get, httptest.NewRequest(http.MethodGet, "/api/policy", nil))
	var resp struct {
		Policy     policy.Policy             `json:"policy"`
		Actions    []policy.ActionDescriptor `json:"actions"`
		ActionMask []bool                    `json:"action_mask"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Actions) != 5 || resp.ActionMask[3] || !resp.ActionMask[4] {
		t.Fatalf("unexpected actions/mask: %+v", resp)
	}

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/api/policy", strings.NewReader(body)))
		return w
	}
	if w := post(`{"action":3}`); w.Code != http.StatusBadRequest {
		t.Fatalf("CH should be 400, got %d", w.Code)
	}
	if w := post(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing action should be 400, got %d", w.Code)
	}
	if w := post(`{"action":4}`); w.Code != http.StatusOK {
		t.Fatalf("adaptive should be 200, got %d: %s", w.Code, w.Body.String())
	}
	if pool.GetStrategy().Name() != "Adaptive-Weighted-Round-Robin" {
		t.Fatal("strategy not switched")
	}

	ctl := httptest.NewRecorder()
	h(ctl, httptest.NewRequest(http.MethodGet, "/api/controller", nil))
	if ctl.Code != http.StatusOK {
		t.Fatalf("controller endpoint: %d", ctl.Code)
	}
}

func TestConcurrentPolicyUpdatesWhileSelecting(t *testing.T) {
	pool := newPolicyPool(t)
	pool.SetController(controller.New(controller.DefaultConfig()))
	_ = pool.ApplyAction(policy.ActionRuleAdaptive)
	ids := []string{"Backend-1", "Backend-2", "Backend-3"}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				pool.PublishAdaptiveWeights(ids, []float64{0.1, 0.3, 0.6})
				if i%50 == 0 {
					_ = pool.ApplyAction(policy.ActionRuleAdaptive)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if pool.GetNextPeer() == nil {
					t.Error("nil peer with healthy backends")
					return
				}
			}
		}()
	}
	wg.Wait()
}
