package main

import (
	"adaptive-load/agent"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------------------ event log
// Event is one entry of the bounded research event log shown on the dashboard timeline.
type Event struct {
	ID      uint64    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // policy | safety | kill | resume | reset | health | fault | note | experiment
	Backend string    `json:"backend,omitempty"`
	Detail  string    `json:"detail"`
}

// markerKinds are the kinds external tools (the load generator) may post.
var markerKinds = map[string]bool{"fault": true, "note": true, "experiment": true}

const (
	maxEventDetail = 200
	eventLogCap    = 500
	historyCap     = 900 // 30 minutes at 2 s
)

// EventLog is a bounded, append-only ring of events.
type EventLog struct {
	mu   sync.Mutex
	seq  uint64
	cap  int
	ring []Event
}

func NewEventLog(capacity int) *EventLog { return &EventLog{cap: capacity} }

// Add records an event (detail is truncated to maxEventDetail bytes).
func (l *EventLog) Add(kind, backend, detail string) Event {
	if l == nil {
		return Event{}
	}
	if len(detail) > maxEventDetail {
		detail = detail[:maxEventDetail]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e := Event{ID: l.seq, At: time.Now(), Kind: kind, Backend: backend, Detail: detail}
	l.ring = append(l.ring, e)
	if len(l.ring) > l.cap {
		l.ring = append([]Event(nil), l.ring[len(l.ring)-l.cap:]...)
	}
	return e
}

// Since returns events with ID > since (oldest first) and the latest ID.
func (l *EventLog) Since(since uint64) ([]Event, uint64) {
	if l == nil {
		return nil, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.ring {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out, l.seq
}

// ---------------------------------------------------------------------------------- history
// HistBackend is the per-backend part of a history point.
type HistBackend struct {
	RPS       float64 `json:"rps"`
	P95Ms     float64 `json:"p95_ms"`
	ErrorRate float64 `json:"error_rate"` // fraction of this backend's requests
	Active    int64   `json:"active_conns"`
	Healthy   bool    `json:"healthy"`
}

// HistPoint is one time-series sample (all values come from the metrics window, not per-request).
type HistPoint struct {
	T             int64                  `json:"t"` // unix ms
	RPS           float64                `json:"rps"`
	P50Ms         float64                `json:"p50_ms"`
	P95Ms         float64                `json:"p95_ms"`
	P99Ms         float64                `json:"p99_ms"`
	ErrorRate     float64                `json:"error_rate"` // fraction
	Active        int64                  `json:"active_requests"`
	Strategy      string                 `json:"strategy"`
	PolicyVersion uint64                 `json:"policy_version"`
	WindowValid   bool                   `json:"window_valid"`
	Backends      map[string]HistBackend `json:"backends"`
}

// History is a bounded ring of HistPoint.
type History struct {
	mu   sync.Mutex
	cap  int
	ring []HistPoint
}

func NewHistory(capacity int) *History { return &History{cap: capacity} }

func fraction(errsPerSec, rps float64) float64 {
	if rps <= 0 {
		return 0
	}
	f := errsPerSec / rps // metrics' ErrorRate.Raw is errors PER SECOND (see the Phase 12 safety monitor)
	if f > 1 {
		f = 1
	}
	return f
}

// pointFrom converts a metrics snapshot into a history point.
func pointFrom(snap *metrics.MetricsSnapshot, strategyName string, version uint64) HistPoint {
	gt := snap.GlobalTraffic
	p := HistPoint{
		T: snap.Timestamp.UnixMilli(), RPS: gt.CurrentRPS.Raw, P50Ms: gt.DurationStats.P50Ms, P95Ms: gt.DurationStats.P95Ms,
		P99Ms: gt.DurationStats.P99Ms, ErrorRate: fraction(gt.ErrorRate.Raw, gt.CurrentRPS.Raw), Active: gt.ActiveRequests,
		Strategy: strategyName, PolicyVersion: version, WindowValid: snap.Window.IsValid, Backends: map[string]HistBackend{},
	}
	for id, b := range snap.Backends {
		p.Backends[id] = HistBackend{RPS: b.RPS.Raw, P95Ms: b.Latency.P95Ms, ErrorRate: fraction(b.ErrorRate.Raw, b.RPS.Raw),
			Active: b.ActiveConns, Healthy: b.Healthy}
	}
	return p
}

// Add appends a point, evicting the oldest beyond capacity.
func (h *History) Add(p HistPoint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, p)
	if len(h.ring) > h.cap {
		h.ring = append([]HistPoint(nil), h.ring[len(h.ring)-h.cap:]...)
	}
}

// Since returns points with T > sinceMs.
func (h *History) Since(sinceMs int64) []HistPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []HistPoint{} // never nil: an empty history must encode as [] not null
	for _, p := range h.ring {
		if p.T > sinceMs {
			out = append(out, p)
		}
	}
	return out
}

// Run samples the collector every `every` until ctx is cancelled.
func (h *History) Run(ctx context.Context, tel Telemetry, pool *ServerPool, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Add(pointFrom(tel.CollectSnapshot(), pool.GetStrategy().Name(), pool.Policies().Current().Version))
		}
	}
}

// ---------------------------------------------------------------------------------- dashboard
// DashBackend is one row of the per-backend table.
type DashBackend struct {
	Name             string   `json:"name"`
	URL              string   `json:"url"`
	Healthy          bool     `json:"healthy"`
	ConfiguredWeight int      `json:"configured_weight"`
	EffectiveWeight  *float64 `json:"effective_weight"` // nil when the active strategy does not use weights
	WeightBasis      string   `json:"weight_basis"`
	ObservedShare    float64  `json:"observed_share"` // fraction of recent traffic actually routed here
	ActiveConns      int64    `json:"active_conns"`
	TotalRequests    uint64   `json:"total_requests"`
	RPS              float64  `json:"rps"`
	P95Ms            float64  `json:"p95_ms"`
	ErrorRate        float64  `json:"error_rate"`
	CPUPct           *float64 `json:"cpu_pct"`    // host-level reading shared by all backends; nil if unavailable
	MemoryPct        *float64 `json:"memory_pct"` // host-level
	ResourceScope    string   `json:"resource_scope"`
}

// FallbackState is the clear AI-fallback indicator.
type FallbackState struct {
	Active     bool       `json:"active"`
	Kind       string     `json:"kind"` // none | kill_switch | safety_lockout
	Until      *time.Time `json:"until,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	AtBaseline bool       `json:"at_baseline"` // the active strategy equals the safe baseline
	Baseline   string     `json:"baseline,omitempty"`
}

// AgentSummary is the controller status block.
type AgentSummary struct {
	Enabled        bool            `json:"enabled"`   // an agent is configured
	AIActive       bool            `json:"ai_active"` // configured, not killed, not shadow
	Killed         bool            `json:"killed"`
	Shadow         bool            `json:"shadow"`
	LastDecisionAt *time.Time      `json:"last_decision_at,omitempty"`
	LastDecision   *DecisionRecord `json:"last_decision,omitempty"`
	ModelVersion   string          `json:"model_version,omitempty"`
	Requests       uint64          `json:"requests"`
	Failures       uint64          `json:"failures"`
	AvgLatencyMs   float64         `json:"avg_latency_ms"`
	Accepted       int             `json:"accepted_recent"`
	Rejected       int             `json:"rejected_recent"`
}

// DashboardResponse is GET /api/dashboard.
type DashboardResponse struct {
	Now      time.Time      `json:"now"`
	Strategy string         `json:"strategy"`
	Policy   *policy.Policy `json:"policy"`
	Agent    AgentSummary   `json:"agent"`
	Fallback FallbackState  `json:"fallback"`
	Global   struct {
		RPS         float64 `json:"rps"`
		P95Ms       float64 `json:"p95_ms"`
		P99Ms       float64 `json:"p99_ms"`
		ErrorRate   float64 `json:"error_rate"`
		WindowValid bool    `json:"window_valid"`
	} `json:"global"`
	Controller struct {
		Present    bool   `json:"present"`
		Updated    bool   `json:"updated"`
		HoldReason string `json:"hold_reason"`
		Windows    uint64 `json:"windows"`
	} `json:"rule_based_controller"`
	Backends []DashBackend `json:"backends"`
	Host     struct {
		Source string `json:"source"`
		Note   string `json:"note"`
	} `json:"host_resources"`
}

// buildDashboard assembles the payload from the live system. snap may be a real or fake snapshot.
func buildDashboard(pool *ServerPool, snap *metrics.MetricsSnapshot, now time.Time) DashboardResponse {
	var d DashboardResponse
	d.Now = now
	d.Strategy = pool.GetStrategy().Name()
	pol := pool.Policies().Current()
	d.Policy = pol

	gt := snap.GlobalTraffic
	d.Global.RPS, d.Global.P95Ms, d.Global.P99Ms = gt.CurrentRPS.Raw, gt.DurationStats.P95Ms, gt.DurationStats.P99Ms
	d.Global.ErrorRate, d.Global.WindowValid = fraction(gt.ErrorRate.Raw, gt.CurrentRPS.Raw), snap.Window.IsValid

	// per-backend rows in stable sorted order
	cons := pool.Constraints()
	totalStatic := 0.0
	for _, b := range cons.Backends {
		totalStatic += float64(b.StaticWeight)
	}
	weightBased := pol.Strategy == policy.StaticSWRR || pol.Strategy == policy.AdaptiveSWRR
	totalRPS := 0.0
	for _, bm := range snap.Backends {
		totalRPS += bm.RPS.Raw
	}
	var cpu, mem *float64
	if snap.Host.CPUUtilization.Valid {
		v := snap.Host.CPUUtilization.Value
		cpu = &v
	}
	if snap.Host.MemoryPercent.Valid {
		v := snap.Host.MemoryPercent.Value
		mem = &v
	} else if snap.Host.MemoryTotalBytes > 0 {
		v := 100 * float64(snap.Host.MemoryUsedBytes) / float64(snap.Host.MemoryTotalBytes)
		mem = &v
	}
	backs := map[string]*Backend{}
	for _, b := range pool.GetAllBackends() {
		backs[b.Name] = b
	}
	for _, info := range cons.Backends {
		b := backs[info.ID]
		row := DashBackend{Name: info.ID, ConfiguredWeight: info.StaticWeight, ResourceScope: "host", CPUPct: cpu, MemoryPct: mem}
		if b != nil {
			row.URL, row.Healthy, row.ActiveConns, row.TotalRequests = b.URL.String(), b.IsAlive(), b.GetActiveConnections(), b.GetTotalRequests()
		}
		if bm := snap.Backends[info.ID]; bm != nil {
			row.RPS, row.P95Ms, row.ErrorRate = bm.RPS.Raw, bm.Latency.P95Ms, fraction(bm.ErrorRate.Raw, bm.RPS.Raw)
		}
		if totalRPS > 0 {
			row.ObservedShare = row.RPS / totalRPS
		}
		switch {
		case weightBased:
			if w, ok := pol.WeightFor(info.ID); ok {
				row.EffectiveWeight, row.WeightBasis = &w, "policy weight ("+string(pol.Strategy)+")"
			} else if totalStatic > 0 {
				w := float64(info.StaticWeight) / totalStatic
				row.EffectiveWeight, row.WeightBasis = &w, "static capacity share"
			}
		default:
			row.WeightBasis = "not used by " + string(pol.Strategy)
		}
		d.Backends = append(d.Backends, row)
	}
	d.Host.Source = snap.Host.Source
	d.Host.Note = "CPU and memory are host-level readings shared by every backend process on this machine; they are not per-backend values."

	if rc := pool.GetController(); rc != nil {
		last := rc.Last()
		d.Controller.Present, d.Controller.Updated, d.Controller.HoldReason, d.Controller.Windows = true, last.Updated, last.HoldReason, last.Windows
	}

	ag := pool.GetAgent()
	d.Agent.Enabled = ag != nil
	d.Fallback = FallbackState{Kind: "none"}
	if ag != nil {
		st := ag.Status()
		d.Agent.Killed, d.Agent.Shadow = st.Manager.Killed, st.Manager.Config.Shadow
		d.Agent.AIActive = !d.Agent.Killed && !d.Agent.Shadow
		d.Agent.Requests = st.Stats.Requests
		for _, v := range st.Stats.Failures {
			d.Agent.Failures += v
		}
		d.Agent.AvgLatencyMs = st.Stats.AvgLatency
		for i := len(st.Recent) - 1; i >= 0; i-- {
			r := st.Recent[i]
			if r.DecisionID == "" {
				continue
			}
			if d.Agent.LastDecision == nil {
				rr, at := r, r.At
				d.Agent.LastDecision, d.Agent.LastDecisionAt = &rr, &at
			}
			if d.Agent.ModelVersion == "" && r.Model != "" {
				d.Agent.ModelVersion = r.Model
			}
		}
		for _, r := range st.Recent {
			if r.Proposed != "" {
				if r.Applied {
					d.Agent.Accepted++
				} else {
					d.Agent.Rejected++
				}
			}
		}
		d.Fallback.Baseline = st.Manager.Baseline
		d.Fallback.AtBaseline = d.Strategy != "" && strings.EqualFold(string(pol.Strategy), st.Manager.Baseline)
		switch {
		case st.Manager.Killed:
			d.Fallback.Active, d.Fallback.Kind, d.Fallback.Reason = true, "kill_switch", "AI disabled by the operator; baseline strategy in force"
		case now.Before(st.Manager.LockedUntil):
			u := st.Manager.LockedUntil
			d.Fallback.Active, d.Fallback.Kind, d.Fallback.Until = true, "safety_lockout", &u
			if n := len(st.Manager.SafetyEvents); n > 0 {
				d.Fallback.Reason = st.Manager.SafetyEvents[n-1].Reason
			}
		}
	}
	return d
}

// agentIntrospector narrows the decider to the optional learner-introspection calls.
func (a *AgentController) introspector() (agent.Introspector, bool) {
	in, ok := a.decider.(agent.Introspector)
	return in, ok
}

// ResetPending forgets the in-flight evaluation window (used by reset-to-baseline).
func (a *AgentController) ResetPending(now time.Time) {
	a.mu.Lock()
	a.pending, a.prevOutcome, a.nextAt = nil, nil, now
	a.mu.Unlock()
}

// ResetToBaseline applies the safe baseline immediately, clears any safety lockout, and records the event.
// AI enable/disable state is unchanged.
func (m *PolicyManager) ResetToBaseline(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Always publish a FRESH baseline policy (new strategy instance, new version, reason recorded), even
	// if the baseline is already active: a reset must leave a trace and clear any strategy-internal state.
	if err := m.pool.ApplyActionReason(m.baseline, "reset_to_baseline"); err != nil {
		return fmt.Errorf("apply baseline: %w", err)
	}
	m.lockedUntil = time.Time{}
	m.lastSwitch = now
	m.events = append(m.events, SafetyEvent{At: now, Kind: "reset", Reason: "reset to baseline"})
	m.trimEvents()
	m.count("reset_to_baseline")
	m.pool.Events().Add("reset", "", "reset to baseline")
	return nil
}
