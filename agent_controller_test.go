package main

import (
	"adaptive-load/agent"
	"adaptive-load/controller"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 13, 16, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------- fakes
type fakeTel struct {
	mu    sync.Mutex
	snap  *metrics.MetricsSnapshot
	state *metrics.StateRepresentation
}

func (f *fakeTel) CollectSnapshot() *metrics.MetricsSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.snap
	return &cp
}
func (f *fakeTel) GetStateRepresentation(...int) *metrics.StateRepresentation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		return nil
	}
	cp := *f.state
	return &cp
}
func (f *fakeTel) update(fn func(s *metrics.MetricsSnapshot, st *metrics.StateRepresentation)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.snap, f.state)
}

func newFakeTel(strategyName string) *fakeTel {
	snap := &metrics.MetricsSnapshot{
		Timestamp: t0,
		Window:    metrics.WindowMetadata{IsValid: true},
		GlobalTraffic: metrics.GlobalTrafficMetrics{
			Window: metrics.WindowMetadata{IsValid: true, CompletedRequests: 100},
		},
		Backends:        map[string]*metrics.BackendRuntimeMetrics{},
		CapacityConfigs: map[string]*metrics.BackendCapacityConfig{},
		Controller:      metrics.ControllerMetrics{CurrentStrategy: strategyName},
	}
	st := &metrics.StateRepresentation{Timestamp: t0}
	for i, n := range []string{"Backend-1", "Backend-2", "Backend-3"} {
		snap.Backends[n] = &metrics.BackendRuntimeMetrics{BackendID: n, Healthy: true}
		snap.CapacityConfigs[n] = &metrics.BackendCapacityConfig{StaticWeight: 3 - i}
		st.Backends = append(st.Backends, metrics.BackendStateFeatures{BackendID: n, Healthy: 1, CapacityRatio: 0.3})
	}
	return &fakeTel{snap: snap, state: st}
}

type fakeDecider struct {
	mu    sync.Mutex
	reqs  []*agent.DecisionRequest
	reply func(*agent.DecisionRequest) (*agent.DecisionResponse, error)
}

func (d *fakeDecider) Decide(_ context.Context, r *agent.DecisionRequest) (*agent.DecisionResponse, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, r)
	d.mu.Unlock()
	return d.reply(r)
}
func (d *fakeDecider) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.reqs) }
func (d *fakeDecider) last() *agent.DecisionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reqs[len(d.reqs)-1]
}

func choose(strategy string) func(*agent.DecisionRequest) (*agent.DecisionResponse, error) {
	return func(r *agent.DecisionRequest) (*agent.DecisionResponse, error) {
		return &agent.DecisionResponse{SchemaVersion: 1, DecisionID: r.DecisionID, Strategy: strategy,
			ModelVersion: "test-v1", Diagnostic: "ok"}, nil
	}
}

func fastMgrCfg() ManagerConfig {
	c := DefaultManagerConfig()
	c.MinPolicyAge = 30 * time.Second
	c.SafetyCooldown = 60 * time.Second
	return c
}

type rig struct {
	pool *ServerPool
	tel  *fakeTel
	mgr  *PolicyManager
	dec  *fakeDecider
	ctl  *AgentController
	now  time.Time
}

func newRig(t *testing.T, strat string, mc ManagerConfig) *rig {
	t.Helper()
	pool := newPolicyPool(t)
	pool.SetController(controller.New(controller.DefaultConfig()))
	if strat != "Round-Robin" {
		_ = pool.SetStrategyByName(strat)
	}
	r := &rig{pool: pool, tel: newFakeTel(pool.GetStrategy().Name()), now: t0, dec: &fakeDecider{reply: choose("least_connections")}}
	r.mgr = NewPolicyManager(pool, mc, t0)
	ac := DefaultAgentConfig()
	r.ctl = NewAgentController(pool, r.tel, r.mgr, r.dec, ac)
	r.ctl.clock = func() time.Time { return r.now }
	return r
}

// step advances the fake clock and the fake snapshot timestamp together.
func (r *rig) step(d time.Duration) {
	r.now = r.now.Add(d)
	r.tel.update(func(s *metrics.MetricsSnapshot, st *metrics.StateRepresentation) {
		s.Timestamp = r.now
		st.Timestamp = r.now
		s.Controller.CurrentStrategy = r.pool.GetStrategy().Name()
	})
	r.ctl.Step(context.Background(), r.now)
}

func (r *rig) traffic(reqs, errs uint64) {
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) {
		s.GlobalTraffic.TotalRequests += reqs
		s.GlobalTraffic.TotalErrors += errs
		for _, b := range s.Backends {
			b.TotalRequests += reqs / 3
		}
	})
}

func (r *rig) syncSwitchCount(n uint64) {
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) { s.Controller.SwitchCount = n })
}

// ------------------------------------------------------------ manager tests
func allowedAll() []string { return policy.WireNames()[:3] } // rr, lc, static (CH masked)

func prop(strategy string, now time.Time) Proposal {
	return Proposal{DecisionID: "window-00001", Strategy: strategy, AllowedStrategies: append(allowedAll(), "rule_based_adaptive"),
		ModelVersion: "m", Diagnostic: "ok", RequestedAt: now}
}

func TestManagerAppliesAfterMinAgeAndRecordsReason(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	now := t0.Add(31 * time.Second)
	res := r.mgr.Propose(prop("least_connections", now), now)
	if !res.Applied || !res.Switched || res.Reason != ReasonApplied {
		t.Fatalf("%+v", res)
	}
	p := r.pool.Policies().Current()
	if p.Strategy != policy.LeastConnections || p.Reason != "ai:ok" || p.Version != res.PolicyVersion {
		t.Fatalf("policy %+v", p)
	}
	if r.pool.GetStrategy().Name() != "Least-Connections" {
		t.Fatal("strategy not swapped")
	}
}

func TestManagerRejectionReasons(t *testing.T) {
	now := t0.Add(31 * time.Second)
	cases := []struct {
		name   string
		mutate func(p *Proposal, r *rig)
		want   string
	}{
		{"unknown strategy", func(p *Proposal, _ *rig) { p.Strategy = "magic" }, ReasonUnknownStrategy},
		{"display name", func(p *Proposal, _ *rig) { p.Strategy = "Least-Connections" }, ReasonUnknownStrategy},
		{"not offered", func(p *Proposal, _ *rig) { p.AllowedStrategies = []string{"round_robin"} }, ReasonNotAllowed},
		{"consistent hash disabled", func(p *Proposal, _ *rig) {
			p.Strategy = "consistent_hash"
			p.AllowedStrategies = append(p.AllowedStrategies, "consistent_hash")
		}, ReasonDisabled},
		{"weights on non-adaptive", func(p *Proposal, _ *rig) { p.Weights = map[string]float64{"Backend-1": 0.5} }, ReasonUnexpectedWeights},
		{"weights wrong count", func(p *Proposal, _ *rig) {
			p.Strategy = "rule_based_adaptive"
			p.Weights = map[string]float64{"Backend-1": 0.5}
		}, ReasonInvalidWeights},
		{"weight out of bounds", func(p *Proposal, _ *rig) {
			p.Strategy = "rule_based_adaptive"
			p.Weights = map[string]float64{"Backend-1": 0.95, "Backend-2": 0.04, "Backend-3": 0.01}
		}, ReasonInvalidWeights},
		{"unknown backend id", func(p *Proposal, _ *rig) {
			p.Strategy = "rule_based_adaptive"
			p.Weights = map[string]float64{"Backend-1": 0.4, "Backend-2": 0.3, "Ghost": 0.3}
		}, ReasonInvalidWeights},
		{"stale decision", func(p *Proposal, _ *rig) { p.RequestedAt = now.Add(-time.Minute) }, ReasonStale},
		{"no healthy backend", func(_ *Proposal, r *rig) {
			for _, b := range r.pool.GetAllBackends() {
				b.SetAlive(false)
			}
		}, ReasonNoHealthy},
		{"min policy age", func(p *Proposal, _ *rig) { p.RequestedAt = t0.Add(5 * time.Second) }, ""},
	}
	for _, c := range cases {
		r := newRig(t, "Round-Robin", fastMgrCfg())
		p := prop("least_connections", now)
		c.mutate(&p, r)
		if c.want == "" { // special: evaluate inside the minimum age
			at := t0.Add(5 * time.Second)
			p.RequestedAt = at
			if res := r.mgr.Propose(p, at); res.Reason != ReasonMinPolicyAge || res.Applied {
				t.Errorf("%s: %+v", c.name, res)
			}
			continue
		}
		res := r.mgr.Propose(p, now)
		if res.Applied || res.Reason != c.want {
			t.Errorf("%s: got %+v want %s", c.name, res, c.want)
		}
		if r.pool.GetStrategy().Name() != "Round-Robin" {
			t.Errorf("%s: strategy changed by a rejected proposal", c.name)
		}
	}
}

func TestManagerSwitchMarginAndShadow(t *testing.T) {
	now := t0.Add(31 * time.Second)
	r := newRig(t, "Round-Robin", fastMgrCfg())
	f := func(v float64) *float64 { return &v }
	p := prop("least_connections", now)
	p.Scores = []*float64{f(0.50), f(0.51), nil, nil, nil} // gain 0.01 < margin 0.02
	if res := r.mgr.Propose(p, now); res.Reason != ReasonSwitchMargin {
		t.Fatalf("%+v", res)
	}
	p.Scores = []*float64{f(0.50), f(0.60), nil, nil, nil}
	if res := r.mgr.Propose(p, now); !res.Applied {
		t.Fatalf("%+v", res)
	}
	mc := fastMgrCfg()
	mc.Shadow = true
	rs := newRig(t, "Round-Robin", mc)
	if res := rs.mgr.Propose(prop("least_connections", now), now); res.Applied || res.Reason != ReasonShadow {
		t.Fatalf("%+v", res)
	}
	if rs.pool.GetStrategy().Name() != "Round-Robin" {
		t.Fatal("shadow mode must not apply")
	}
}

func TestSwitchMarginDoesNotBlockFirstTimeExploration(t *testing.T) {
	now := t0.Add(31 * time.Second)
	f := func(v float64) *float64 { return &v }
	// identical scores (cold model): without the exemption the learner could never leave the baseline
	r := newRig(t, "Round-Robin", fastMgrCfg())
	p := prop("least_connections", now)
	p.Scores = []*float64{f(0.1), f(0.1), nil, nil, nil}
	if res := r.mgr.Propose(p, now); res.Reason != ReasonSwitchMargin {
		t.Fatalf("exploitation with no gain must be rejected: %+v", res)
	}
	p.Explored = true
	if res := r.mgr.Propose(p, now); !res.Applied {
		t.Fatalf("exploration must bypass the margin: %+v", res)
	}
	// ...but never the minimum policy age
	r2 := newRig(t, "Round-Robin", fastMgrCfg())
	early := t0.Add(5 * time.Second)
	p2 := prop("least_connections", early)
	p2.Explored = true
	if res := r2.mgr.Propose(p2, early); res.Reason != ReasonMinPolicyAge {
		t.Fatalf("exploration must still respect min policy age: %+v", res)
	}
}

func TestManagerRetainedSame(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	res := r.mgr.Propose(prop("round_robin", t0), t0)
	if res.Applied || res.Reason != ReasonRetainedSame {
		t.Fatalf("%+v", res)
	}
}

func TestManagerAdaptiveWeightsPublished(t *testing.T) {
	now := t0.Add(31 * time.Second)
	r := newRig(t, "Round-Robin", fastMgrCfg())
	p := prop("rule_based_adaptive", now)
	p.Weights = map[string]float64{"Backend-1": 0.2, "Backend-2": 0.3, "Backend-3": 0.5}
	if res := r.mgr.Propose(p, now); !res.Applied {
		t.Fatalf("%+v", res)
	}
	cur := r.pool.Policies().Current()
	if cur.Strategy != policy.AdaptiveSWRR || cur.Reason != "ai:weights" {
		t.Fatalf("%+v", cur)
	}
	if w, _ := cur.WeightFor("Backend-3"); w != 0.5 {
		t.Fatalf("weights not published: %+v", cur)
	}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		counts[r.pool.GetNextPeer().Name]++
	}
	if counts["Backend-3"] != 500 {
		t.Fatalf("request path ignores AI weights: %v", counts)
	}
}

func TestSafetyFallbackBypassesMinAgeAndLocksOut(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	now := t0.Add(2 * time.Second) // well inside the min policy age
	r.mgr.SafetyFallback("test", now)
	if r.pool.GetStrategy().Name() != "Smooth-Weighted-Round-Robin" {
		t.Fatalf("fallback must go to the safe baseline, got %s", r.pool.GetStrategy().Name())
	}
	if p := r.pool.Policies().Current(); p.Reason != "safety:test" {
		t.Fatalf("reason %q", p.Reason)
	}
	later := now.Add(30 * time.Second)
	if res := r.mgr.Propose(prop("least_connections", later), later); res.Reason != ReasonSafetyLock {
		t.Fatalf("AI must be locked out: %+v", res)
	}
	after := now.Add(61 * time.Second)
	if res := r.mgr.Propose(prop("least_connections", after), after); !res.Applied {
		t.Fatalf("lockout must expire: %+v", res)
	}
}

func TestKillSwitchAndResume(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	base := policy.ActionRoundRobin
	if err := r.mgr.Kill(true, &base, t0); err != nil {
		t.Fatal(err)
	}
	if r.pool.GetStrategy().Name() != "Round-Robin" || !r.mgr.IsKilled() {
		t.Fatal("kill must apply the selected baseline")
	}
	now := t0.Add(time.Minute)
	if res := r.mgr.Propose(prop("least_connections", now), now); res.Reason != ReasonKilled {
		t.Fatalf("%+v", res)
	}
	if err := r.mgr.Kill(false, nil, now); err != nil {
		t.Fatal(err)
	}
	if res := r.mgr.Propose(prop("least_connections", now), now); !res.Applied {
		t.Fatalf("%+v", res)
	}
	bad := policy.ActionConsistentHash
	if err := r.mgr.Kill(true, &bad, now); err == nil {
		t.Fatal("an invalid baseline must be refused")
	}
}

func TestInFlightSelectionUnaffectedBySwap(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	peer := r.pool.GetNextPeer()
	peer.IncConns() // in-flight request holds its backend
	now := t0.Add(31 * time.Second)
	r.mgr.Propose(prop("least_connections", now), now)
	if peer.GetActiveConnections() != 1 {
		t.Fatal("a policy swap must not touch in-flight requests")
	}
	peer.DecConns()
}

func TestConcurrentProposalsWhileServing(t *testing.T) {
	r := newRig(t, "Round-Robin", func() ManagerConfig { c := fastMgrCfg(); c.MinPolicyAge = 0; return c }())
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if r.pool.GetNextPeer() == nil {
						t.Error("nil peer")
						return
					}
				}
			}
		}()
	}
	names := []string{"round_robin", "least_connections", "static_swrr", "rule_based_adaptive"}
	for i := 0; i < 400; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		r.mgr.Propose(prop(names[i%4], now), now)
		if i%97 == 0 {
			r.mgr.SafetyFallback("churn", now)
		}
	}
	close(stop)
	wg.Wait()
}

// --------------------------------------------------------- controller tests
func TestFullCycleSendsOutcomeWithCleanExposure(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.step(31 * time.Second) // first decision: min age elapsed -> apply LC
	if r.dec.count() != 1 || r.pool.GetStrategy().Name() != "Least-Connections" {
		t.Fatalf("calls=%d strat=%s", r.dec.count(), r.pool.GetStrategy().Name())
	}
	if r.dec.last().PreviousOutcome != nil || r.dec.last().CurrentStrategy != "round_robin" {
		t.Fatalf("first request: %+v", r.dec.last())
	}
	r.syncSwitchCount(1)
	r.step(2 * time.Second) // still settling
	r.step(9 * time.Second) // settle (10s) done -> start snapshot
	r.traffic(3000, 30)
	r.step(20 * time.Second) // measure done
	r.traffic(0, 0)
	r.step(2 * time.Second) // next decision carries the outcome
	if r.dec.count() != 2 {
		t.Fatalf("expected a second decision, got %d", r.dec.count())
	}
	po := r.dec.last().PreviousOutcome
	if po == nil || po.DecisionID != "window-00001" || !po.CleanExposure {
		t.Fatalf("previous outcome %+v", po)
	}
	if po.Requests != 3000 || po.Errors != 30 || len(po.Backends) != 3 || po.WindowSeconds < 19 {
		t.Fatalf("outcome counters wrong: %+v", po)
	}
}

func TestFailureRetainsPolicyAndBacksOff(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.dec.reply = func(*agent.DecisionRequest) (*agent.DecisionResponse, error) {
		return nil, &agent.CallError{Kind: agent.FailTimeout, Err: errors.New("deadline")}
	}
	r.step(31 * time.Second)
	r.step(2 * time.Second) // inside the 10s failure backoff: must not call again
	if r.dec.count() != 1 {
		t.Fatalf("backoff violated: %d calls", r.dec.count())
	}
	if r.pool.GetStrategy().Name() != "Round-Robin" {
		t.Fatal("an agent outage must keep the last valid policy")
	}
	st := r.ctl.Status()
	if st.Stats.Failures["timeout"] != 1 || st.Recent[0].Reason != "agent_unavailable" {
		t.Fatalf("failure not recorded: %+v", st)
	}
	r.step(10 * time.Second)
	if r.dec.count() != 2 {
		t.Fatal("must retry after backoff")
	}
}

func TestStaleStateIsNotSentToAgent(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.tel.update(func(_ *metrics.MetricsSnapshot, st *metrics.StateRepresentation) {
		st.SafePolicyRetainRecommended = true
	})
	r.step(31 * time.Second)
	if r.dec.count() != 0 || r.ctl.Status().Stats.HeldStale != 1 {
		t.Fatal("stale state must be held locally")
	}
}

func TestStaleDecisionFromAgentRejected(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.dec.reply = func(*agent.DecisionRequest) (*agent.DecisionResponse, error) {
		return nil, &agent.CallError{Kind: agent.FailStale, Err: agent.ErrStaleDecision}
	}
	r.step(31 * time.Second)
	if r.pool.GetStrategy().Name() != "Round-Robin" || r.ctl.Status().Stats.Failures["stale"] != 1 {
		t.Fatal("stale decision must be dropped and counted")
	}
}

func TestSlowAgentResponseIsStaleForManager(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.dec.reply = func(q *agent.DecisionRequest) (*agent.DecisionResponse, error) {
		r.now = r.now.Add(8 * time.Second) // answer arrives after MaxDecisionAge (5s)
		return choose("least_connections")(q)
	}
	r.step(31 * time.Second)
	if r.pool.GetStrategy().Name() != "Round-Robin" || r.mgr.Status().Reasons[ReasonStale] != 1 {
		t.Fatalf("late answer must be rejected: %+v", r.mgr.Status().Reasons)
	}
}

func TestRejectedProposalGetsNoEvaluationWindow(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.dec.reply = choose("consistent_hash") // never offered -> rejected
	r.step(31 * time.Second)
	if r.ctl.pending != nil {
		t.Fatal("a rejected proposal must not be evaluated as if it were in force")
	}
}

func TestForeignSwitchAbandonsWindowAndWithholdsOutcome(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.step(31 * time.Second)
	r.syncSwitchCount(1)
	r.step(11 * time.Second)
	r.traffic(3000, 0)
	_ = r.pool.SetStrategyByName("Round-Robin") // operator intervenes mid-window
	r.syncSwitchCount(2)
	r.step(21 * time.Second)
	r.step(2 * time.Second)
	if r.dec.count() >= 2 && r.dec.last().PreviousOutcome != nil {
		t.Fatalf("an unclean window must not produce an outcome: %+v", r.dec.last().PreviousOutcome)
	}
}

func TestLowSampleWindowNotClean(t *testing.T) {
	r := newRig(t, "Round-Robin", fastMgrCfg())
	r.step(31 * time.Second)
	r.syncSwitchCount(1)
	r.step(11 * time.Second)
	r.traffic(6, 0) // far below MinSamples
	r.step(21 * time.Second)
	r.step(2 * time.Second)
	po := r.dec.last().PreviousOutcome
	if po == nil || po.CleanExposure {
		t.Fatalf("tiny windows must be reported as not clean: %+v", po)
	}
}

func TestSafetyMonitorFallsBackOnErrorRate(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) {
		s.GlobalTraffic.ErrorRate.Raw = 25 // errors per second
		s.GlobalTraffic.CurrentRPS.Raw = 100
		s.GlobalTraffic.Window.CompletedRequests = 500
	})
	calls := r.dec.count()
	r.step(2 * time.Second)
	if r.pool.GetStrategy().Name() != "Smooth-Weighted-Round-Robin" {
		t.Fatalf("expected fallback to safe baseline, got %s", r.pool.GetStrategy().Name())
	}
	if r.dec.count() != calls {
		t.Fatal("AI must not be consulted during the safety lockout")
	}
	if len(r.mgr.Status().SafetyEvents) != 1 {
		t.Fatal("safety event not recorded")
	}
}

func TestSafetyMonitorIgnoresTinyWindowsAndP99Triggers(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) {
		s.GlobalTraffic.ErrorRate.Raw = 90
		s.GlobalTraffic.CurrentRPS.Raw = 100
		s.GlobalTraffic.Window.CompletedRequests = 3
	})
	r.step(2 * time.Second)
	if r.pool.GetStrategy().Name() != "Least-Connections" {
		t.Fatal("tiny windows must not trigger the safety fallback")
	}
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) {
		s.GlobalTraffic.ErrorRate.Raw = 0
		s.GlobalTraffic.Window.CompletedRequests = 500
		s.GlobalTraffic.DurationStats.P99Ms = 4000
	})
	r.step(2 * time.Second)
	if r.pool.GetStrategy().Name() != "Smooth-Weighted-Round-Robin" {
		t.Fatal("extreme p99 must trigger fallback")
	}
}

// Regression: ErrorRate.Raw is errors/second. 28 errors/s at 1000 rps is 2.8%, which
// must NOT trigger the 10% fallback (it did before the unit was handled).
func TestSafetyMonitorUsesErrorFractionNotErrorsPerSecond(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	r.tel.update(func(s *metrics.MetricsSnapshot, _ *metrics.StateRepresentation) {
		s.GlobalTraffic.ErrorRate.Raw = 28
		s.GlobalTraffic.CurrentRPS.Raw = 1000
		s.GlobalTraffic.Window.CompletedRequests = 10000
	})
	r.step(2 * time.Second)
	if r.pool.GetStrategy().Name() != "Least-Connections" {
		t.Fatal("2.8% errors must not trigger a 10% fallback")
	}
}

func TestKillSwitchStopsAgentRequests(t *testing.T) {
	r := newRig(t, "Least-Connections", fastMgrCfg())
	if err := r.mgr.Kill(true, nil, t0); err != nil {
		t.Fatal(err)
	}
	r.step(60 * time.Second)
	if r.dec.count() != 0 || r.pool.GetStrategy().Name() != "Smooth-Weighted-Round-Robin" {
		t.Fatalf("calls=%d strategy=%s", r.dec.count(), r.pool.GetStrategy().Name())
	}
}

func TestFeaturesFromStateClipsAndOrders(t *testing.T) {
	st := newFakeTel("Round-Robin").state
	st.Global.RPSNormalized = 3
	st.Backends[0].ErrorRate = -1
	f := featuresFromState(st)
	if f.Global.RPSNormalized != 1 || f.Backends[0].ErrorRate != 0 || f.Backends[0].ID != "Backend-1" {
		t.Fatalf("%+v", f)
	}
}

func TestEnvConfigOverrides(t *testing.T) {
	t.Setenv("LB_AGENT_URL", "http://127.0.0.1:8090")
	t.Setenv("LB_AGENT_MIN_POLICY_AGE_S", "5")
	t.Setenv("LB_AGENT_MAX_ERROR_RATE", "0.2")
	t.Setenv("LB_SAFE_ACTION", "0")
	t.Setenv("LB_AGENT_SHADOW", "1")
	ac, mc := LoadAgentConfigFromEnv()
	if ac.URL == "" || mc.MinPolicyAge != 5*time.Second || mc.MaxErrorRate != 0.2 || mc.SafeAction != policy.ActionRoundRobin || !mc.Shadow {
		t.Fatalf("%+v %+v", ac, mc)
	}
	t.Setenv("LB_SAFE_ACTION", "99") // invalid -> ignored
	if _, mc2 := LoadAgentConfigFromEnv(); mc2.SafeAction != policy.ActionStaticSWRR {
		t.Fatal("invalid safe action must fall back to the default")
	}
}
