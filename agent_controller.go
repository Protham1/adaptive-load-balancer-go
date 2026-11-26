package main

import (
	"adaptive-load/agent"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// Telemetry is the aggregated, once-per-interval view of the system. Metrics are
// never sent to Python per HTTP request.
type Telemetry interface {
	CollectSnapshot() *metrics.MetricsSnapshot
	GetStateRepresentation(maxBackends ...int) *metrics.StateRepresentation
}

// AgentConfig controls the Go-side decision loop (Phase 10).
type AgentConfig struct {
	URL            string        `json:"url"`
	RequestTimeout time.Duration `json:"request_timeout_ns"`
	Settle         time.Duration `json:"settle_ns"`          // lets the metrics window contain only the new policy
	Measure        time.Duration `json:"measure_ns"`         // evaluation window length
	StepEvery      time.Duration `json:"step_every_ns"`      // how often the loop wakes
	FailureBackoff time.Duration `json:"failure_backoff_ns"` // pause after an API failure
	MinSamples     uint64        `json:"min_samples"`        // low-sample guard for learning
}

// DefaultAgentConfig returns the documented defaults.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		RequestTimeout: 2 * time.Second,
		Settle:         10 * time.Second,
		Measure:        20 * time.Second,
		StepEvery:      2 * time.Second,
		FailureBackoff: 10 * time.Second,
		MinSamples:     20,
	}
}

// LoadAgentConfigFromEnv reads LB_AGENT_* variables over the defaults. Safety
// thresholds are configuration, not constants; the effective values are exposed
// at GET /api/agent so experiments can report them.
func LoadAgentConfigFromEnv() (AgentConfig, ManagerConfig) {
	ac, mc := DefaultAgentConfig(), DefaultManagerConfig()
	ac.URL = os.Getenv("LB_AGENT_URL")
	dur := func(name string, dst *time.Duration, unit time.Duration) {
		if v := os.Getenv(name); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				*dst = time.Duration(f * float64(unit))
			}
		}
	}
	flt := func(name string, dst *float64) {
		if v := os.Getenv(name); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				*dst = f
			}
		}
	}
	dur("LB_AGENT_TIMEOUT_MS", &ac.RequestTimeout, time.Millisecond)
	dur("LB_AGENT_SETTLE_S", &ac.Settle, time.Second)
	dur("LB_AGENT_MEASURE_S", &ac.Measure, time.Second)
	dur("LB_AGENT_STEP_S", &ac.StepEvery, time.Second)
	dur("LB_AGENT_MAX_DECISION_AGE_S", &mc.MaxDecisionAge, time.Second)
	dur("LB_AGENT_MIN_POLICY_AGE_S", &mc.MinPolicyAge, time.Second)
	dur("LB_AGENT_SAFETY_COOLDOWN_S", &mc.SafetyCooldown, time.Second)
	flt("LB_AGENT_SWITCH_MARGIN", &mc.SwitchMargin)
	flt("LB_AGENT_MAX_ERROR_RATE", &mc.MaxErrorRate)
	flt("LB_AGENT_MAX_P99_MS", &mc.MaxP99Ms)
	if v := os.Getenv("LB_SAFE_ACTION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if _, ok := policy.ActionID(n).Strategy(); ok {
				mc.SafeAction = policy.ActionID(n)
			}
		}
	}
	mc.Shadow = os.Getenv("LB_AGENT_SHADOW") == "1"
	return ac, mc
}

// DecisionRecord is one entry of the bounded decision history.
type DecisionRecord struct {
	DecisionID string    `json:"decision_id"`
	At         time.Time `json:"at"`
	LatencyMs  float64   `json:"latency_ms"`
	Current    string    `json:"current_strategy"`
	Proposed   string    `json:"proposed_strategy,omitempty"`
	Diagnostic string    `json:"diagnostic,omitempty"`
	Model      string    `json:"model_version,omitempty"`
	Reason     string    `json:"reason"`
	Applied    bool      `json:"applied"`
	Failure    string    `json:"failure,omitempty"`
}

// AgentStats are cumulative counters.
type AgentStats struct {
	Requests     uint64            `json:"requests"`
	Successes    uint64            `json:"successes"`
	Failures     map[string]uint64 `json:"failures_by_kind"`
	HeldStale    uint64            `json:"held_stale_state"`
	LastLatency  float64           `json:"last_latency_ms"`
	MaxLatency   float64           `json:"max_latency_ms"`
	TotalLatency float64           `json:"-"`
	AvgLatency   float64           `json:"avg_latency_ms"`
}

type pendingWindow struct {
	decisionID   string
	strategy     string // display name expected throughout the window
	switchCount  uint64 // expected collector switch count
	settleUntil  time.Time
	start        *metrics.MetricsSnapshot
	measureUntil time.Time
	cleanSoFar   bool
}

// AgentController is the Go side of the control loop. One goroutine calls Step.
type AgentController struct {
	pool    *ServerPool
	tel     Telemetry
	mgr     *PolicyManager
	decider agent.Decider
	cfg     AgentConfig
	clock   func() time.Time // injectable for deterministic tests

	mu          sync.Mutex
	seq         uint64
	pending     *pendingWindow
	prevOutcome *agent.PreviousOutcome
	nextAt      time.Time
	stats       AgentStats
	records     []DecisionRecord
}

// NewAgentController wires the loop together.
func NewAgentController(pool *ServerPool, tel Telemetry, mgr *PolicyManager, d agent.Decider, cfg AgentConfig) *AgentController {
	return &AgentController{pool: pool, tel: tel, mgr: mgr, decider: d, cfg: cfg, clock: time.Now,
		stats: AgentStats{Failures: map[string]uint64{}}}
}

// Run steps until ctx is cancelled.
func (a *AgentController) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.StepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.Step(ctx, now)
		}
	}
}

func wireOf(display string) string {
	if act, ok := policy.ActionForStrategy(policy.StrategyType(display)); ok {
		n, _ := act.WireName()
		return n
	}
	return ""
}

// Step advances the loop once. Order: safety monitor -> kill switch -> pending
// evaluation window -> new decision. It never blocks longer than RequestTimeout.
func (a *AgentController) Step(ctx context.Context, now time.Time) {
	snap := a.tel.CollectSnapshot()
	a.checkSafety(snap, now)
	if a.mgr.IsKilled() {
		a.mu.Lock()
		a.pending, a.prevOutcome = nil, nil
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	if a.pending != nil {
		done := a.advancePendingLocked(snap, now)
		if !done {
			a.mu.Unlock()
			return
		}
	}
	if now.Before(a.nextAt) || now.Before(a.mgr.LockedUntil()) {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	a.decide(ctx, snap, now)
}

// checkSafety is the conservative fallback: sustained high error rate or extreme
// p99 in a statistically meaningful window returns to the baseline immediately.
func (a *AgentController) checkSafety(snap *metrics.MetricsSnapshot, now time.Time) {
	mc := a.mgr.cfg
	gt := snap.GlobalTraffic
	if !snap.Window.IsValid || gt.Window.CompletedRequests < mc.SafetyMinReqs {
		return
	}
	cur, _ := policy.ActionForStrategy(policy.StrategyType(a.pool.GetStrategy().Name()))
	if cur == a.mgr.baselineAction() || now.Before(a.mgr.LockedUntil()) {
		return
	}
	// NOTE: metrics' ErrorRate.Raw is errors PER SECOND, not a fraction. The error
	// fraction of the window is errors/s divided by requests/s.
	errFrac := 0.0
	if rps := gt.CurrentRPS.Raw; rps > 0 {
		errFrac = gt.ErrorRate.Raw / rps
	}
	var reason string
	switch {
	case errFrac >= mc.MaxErrorRate:
		reason = fmt.Sprintf("error_fraction=%.3f>=%.3f", errFrac, mc.MaxErrorRate)
	case gt.DurationStats.P99Ms >= mc.MaxP99Ms:
		reason = fmt.Sprintf("p99=%.0fms>=%.0fms", gt.DurationStats.P99Ms, mc.MaxP99Ms)
	default:
		return
	}
	a.mgr.SafetyFallback(reason, now)
}

func (m *PolicyManager) baselineAction() policy.ActionID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseline
}

// advancePendingLocked returns true when the window finished or was abandoned.
func (a *AgentController) advancePendingLocked(snap *metrics.MetricsSnapshot, now time.Time) bool {
	p := a.pending
	abandon := func(why string) bool {
		log.Printf("[Agent] window %s abandoned: %s", p.decisionID, why)
		a.pending, a.prevOutcome = nil, nil
		a.nextAt = now
		return true
	}
	if snap.Controller.CurrentStrategy != p.strategy {
		return abandon("policy changed during window")
	}
	if snap.Controller.SwitchCount != p.switchCount {
		return abandon("unexpected strategy switch during window")
	}
	if p.start == nil {
		if !now.Before(p.settleUntil) {
			p.start, p.measureUntil = snap, now.Add(a.cfg.Measure)
		}
		return false
	}
	if now.Before(p.measureUntil) {
		return false
	}
	out := buildOutcome(p.decisionID, p.start, snap, a.cfg.MinSamples)
	a.pending, a.nextAt = nil, now
	if out == nil {
		a.prevOutcome = nil
		log.Printf("[Agent] window %s unusable (counters decreased or no time elapsed)", p.decisionID)
		return true
	}
	a.prevOutcome = out
	return true
}

// buildOutcome derives the previous-window evidence from two snapshots (counter deltas).
func buildOutcome(id string, start, end *metrics.MetricsSnapshot, minSamples uint64) *agent.PreviousOutcome {
	dt := end.Timestamp.Sub(start.Timestamp).Seconds()
	s, e := start.GlobalTraffic, end.GlobalTraffic
	if dt <= 0 || e.TotalRequests < s.TotalRequests || e.TotalErrors < s.TotalErrors {
		return nil
	}
	out := &agent.PreviousOutcome{
		DecisionID: id, WindowSeconds: dt,
		Requests: e.TotalRequests - s.TotalRequests, Errors: e.TotalErrors - s.TotalErrors,
		P95Ms: e.DurationStats.P95Ms, CleanExposure: true,
	}
	if out.Errors > out.Requests {
		out.Errors = out.Requests
	}
	if out.Requests < minSamples {
		out.CleanExposure = false // low-sample guard: never learn from tiny windows
	}
	for id, be := range end.Backends {
		bs, ok := start.Backends[id]
		if !ok || be.TotalRequests < bs.TotalRequests {
			continue
		}
		w := 1
		if c := end.CapacityConfigs[id]; c != nil && c.StaticWeight > 0 {
			w = c.StaticWeight
		}
		out.Backends = append(out.Backends, agent.BackendOutcome{
			ID: id, Requests: be.TotalRequests - bs.TotalRequests, Healthy: be.Healthy && bs.Healthy, Weight: w})
	}
	sortOutcomeBackends(out.Backends)
	return out
}

func sortOutcomeBackends(b []agent.BackendOutcome) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j-1].ID > b[j].ID; j-- {
			b[j-1], b[j] = b[j], b[j-1]
		}
	}
}

func clamp01(v float64) float64 {
	if v != v || v < 0 { // NaN or negative
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func featuresFromState(st *metrics.StateRepresentation) agent.Features {
	f := agent.Features{Global: agent.GlobalFeatures{
		RPSNormalized:                clamp01(st.Global.RPSNormalized),
		RequestDurationP95Normalized: clamp01(st.Global.RequestDurationP95Normalized),
		LongRequestRatio:             clamp01(st.Global.LongRequestRatio),
		GlobalErrorRate:              clamp01(st.Global.GlobalErrorRate),
	}}
	for _, b := range st.Backends { // already in stable sorted order
		f.Backends = append(f.Backends, agent.BackendFeatures{
			ID: b.BackendID, Healthy: clamp01(b.Healthy), CPUUtilization: clamp01(b.CPUUtilization),
			MemoryUtilization: clamp01(b.MemoryUtilization), ActiveConnectionsNormalized: clamp01(b.ActiveConnectionsNormalized),
			BackendRPSNormalized: clamp01(b.BackendRPSNormalized), P95LatencyNormalized: clamp01(b.P95LatencyNormalized),
			ErrorRate: clamp01(b.ErrorRate), CapacityRatio: clamp01(b.CapacityRatio)})
	}
	return f
}

func (a *AgentController) record(r DecisionRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
	if len(a.records) > 50 {
		a.records = append([]DecisionRecord(nil), a.records[len(a.records)-50:]...)
	}
}

// decide builds one aggregated request, calls the agent with a hard timeout, and
// hands the (validated) proposal to the policy manager. Any failure retains the
// last known valid policy.
func (a *AgentController) decide(ctx context.Context, snap *metrics.MetricsSnapshot, now time.Time) {
	state := a.tel.GetStateRepresentation()
	curName := a.pool.GetStrategy().Name()
	if state == nil || state.SafePolicyRetainRecommended || state.IsStale || len(state.Backends) == 0 {
		a.mu.Lock()
		a.stats.HeldStale++
		a.mu.Unlock()
		a.record(DecisionRecord{At: now, Current: wireOf(curName), Reason: "held_stale_or_invalid_state"})
		return
	}

	a.mu.Lock()
	a.seq++
	id := fmt.Sprintf("window-%05d", a.seq)
	prev := a.prevOutcome
	a.prevOutcome = nil
	a.mu.Unlock()

	var allowed []string
	for _, d := range policy.Describe(a.pool.Constraints()) {
		if d.Valid {
			n, _ := d.ID.WireName()
			allowed = append(allowed, n)
		}
	}
	req := &agent.DecisionRequest{
		SchemaVersion: agent.SchemaVersion, DecisionID: id, StateVersion: agent.StateVersion,
		ObservedAt: state.Timestamp.UTC(), WindowSeconds: (a.cfg.Settle + a.cfg.Measure).Seconds(),
		CurrentStrategy: wireOf(curName), AllowedStrategies: allowed,
		Features: featuresFromState(state), PreviousOutcome: prev,
	}

	sent := a.clock()
	resp, err := a.decider.Decide(ctx, req)
	lat := float64(a.clock().Sub(sent).Microseconds()) / 1000.0

	a.mu.Lock()
	a.stats.Requests++
	a.stats.LastLatency = lat
	a.stats.TotalLatency += lat
	if lat > a.stats.MaxLatency {
		a.stats.MaxLatency = lat
	}
	if err != nil {
		kind := "unknown"
		var ce *agent.CallError
		if errors.As(err, &ce) {
			kind = string(ce.Kind)
		}
		a.stats.Failures[kind]++
		a.nextAt = now.Add(a.cfg.FailureBackoff)
		a.stats.AvgLatency = a.stats.TotalLatency / float64(a.stats.Requests)
		a.mu.Unlock()
		log.Printf("[Agent] decision %s FAILED (%s) after %.1fms: %v; retaining current policy", id, kind, lat, err)
		a.record(DecisionRecord{DecisionID: id, At: now, LatencyMs: lat, Current: wireOf(curName), Reason: "agent_unavailable", Failure: kind})
		return
	}
	a.stats.Successes++
	a.stats.AvgLatency = a.stats.TotalLatency / float64(a.stats.Requests)
	a.mu.Unlock()

	res := a.mgr.Propose(Proposal{
		DecisionID: id, Strategy: resp.Strategy, Weights: resp.Weights, Scores: resp.Scores,
		ModelVersion: resp.ModelVersion, Diagnostic: resp.Diagnostic, RequestedAt: sent, AllowedStrategies: allowed,
	}, a.clock())
	log.Printf("[Agent] decision %s latency=%.1fms proposed=%s (%s, model %s) -> %s",
		id, lat, resp.Strategy, resp.Diagnostic, resp.ModelVersion, res.Reason)
	a.record(DecisionRecord{DecisionID: id, At: now, LatencyMs: lat, Current: wireOf(curName), Proposed: resp.Strategy,
		Diagnostic: resp.Diagnostic, Model: resp.ModelVersion, Reason: res.Reason, Applied: res.Applied})

	a.mu.Lock()
	defer a.mu.Unlock()
	cycle := a.cfg.Settle + a.cfg.Measure
	if res.Applied || res.Reason == ReasonRetainedSame {
		// Only a decision that is actually in force gets an evaluation window.
		settle := time.Duration(0)
		expected := snap.Controller.SwitchCount
		if res.Switched {
			settle = a.cfg.Settle
			expected++
		}
		a.pending = &pendingWindow{decisionID: id, strategy: a.pool.GetStrategy().Name(),
			switchCount: expected, settleUntil: now.Add(settle), cleanSoFar: true}
		if settle == 0 {
			a.pending.start, a.pending.measureUntil = snap, now.Add(a.cfg.Measure)
		}
		return
	}
	a.nextAt = now.Add(cycle)
}

// AgentStatus is the payload of GET /api/agent.
type AgentStatus struct {
	Enabled bool             `json:"enabled"`
	Config  AgentConfig      `json:"config"`
	Stats   AgentStats       `json:"stats"`
	Manager ManagerStatus    `json:"manager"`
	Policy  *policy.Policy   `json:"policy"`
	Recent  []DecisionRecord `json:"recent_decisions"`
}

// Status returns a snapshot for the API.
func (a *AgentController) Status() AgentStatus {
	a.mu.Lock()
	st := a.stats
	st.Failures = make(map[string]uint64, len(a.stats.Failures))
	for k, v := range a.stats.Failures {
		st.Failures[k] = v
	}
	recent := append([]DecisionRecord(nil), a.records...)
	a.mu.Unlock()
	return AgentStatus{Enabled: true, Config: a.cfg, Stats: st, Manager: a.mgr.Status(),
		Policy: a.pool.Policies().Current(), Recent: recent}
}
