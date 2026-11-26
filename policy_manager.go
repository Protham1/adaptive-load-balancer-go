package main

import (
	"adaptive-load/policy"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ManagerConfig holds every threshold of Phases 11-12. Nothing here is a magic
// number inside the logic: defaults are documented in documentation.md and every
// value can be overridden through LB_AGENT_* environment variables.
type ManagerConfig struct {
	MaxDecisionAge time.Duration   `json:"max_decision_age_ns"` // a response older than this is stale
	MinPolicyAge   time.Duration   `json:"min_policy_age_ns"`   // minimum time between two strategy switches
	SwitchMargin   float64         `json:"switch_margin"`       // required expected improvement to switch
	SafetyCooldown time.Duration   `json:"safety_cooldown_ns"`  // AI is locked out after a safety fallback
	MaxErrorRate   float64         `json:"max_error_rate"`      // window error rate that triggers fallback
	MaxP99Ms       float64         `json:"max_p99_ms"`          // window p99 that triggers fallback
	SafetyMinReqs  uint64          `json:"safety_min_requests"` // ignore the safety check on tiny windows
	SafeAction     policy.ActionID `json:"safe_action"`         // known-safe baseline / fallback strategy
	MinWeight      float64         `json:"min_weight"`          // bounds for AI-proposed weights
	MaxWeight      float64         `json:"max_weight"`
	Shadow         bool            `json:"shadow"` // record proposals, never apply them
}

// DefaultManagerConfig returns the documented defaults.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxDecisionAge: 5 * time.Second,
		MinPolicyAge:   30 * time.Second,
		SwitchMargin:   0.02,
		SafetyCooldown: 60 * time.Second,
		MaxErrorRate:   0.10,
		MaxP99Ms:       2000,
		SafetyMinReqs:  20,
		SafeAction:     policy.ActionStaticSWRR,
		MinWeight:      0.05,
		MaxWeight:      0.80,
	}
}

// Reason codes recorded for every proposal.
const (
	ReasonApplied           = "applied"
	ReasonRetainedSame      = "retained_same"
	ReasonShadow            = "shadow_recorded"
	ReasonKilled            = "rejected_killed"
	ReasonSafetyLock        = "rejected_safety_lock"
	ReasonUnknownStrategy   = "rejected_unknown_strategy"
	ReasonNotAllowed        = "rejected_not_allowed"
	ReasonDisabled          = "rejected_disabled"
	ReasonUnexpectedWeights = "rejected_unexpected_weights"
	ReasonInvalidWeights    = "rejected_invalid_weights"
	ReasonStale             = "rejected_stale"
	ReasonNoHealthy         = "rejected_no_healthy"
	ReasonMinPolicyAge      = "rejected_min_policy_age"
	ReasonSwitchMargin      = "rejected_switch_margin"
	ReasonApplyFailed       = "rejected_apply_failed"
)

// Proposal is an AI recommendation after contract validation.
type Proposal struct {
	DecisionID        string
	Strategy          string // wire name
	Weights           map[string]float64
	Scores            []*float64
	Explored          bool // the learner marked this as exploration of an untried/non-greedy action
	ModelVersion      string
	Diagnostic        string
	RequestedAt       time.Time // when the decision request was sent
	AllowedStrategies []string  // what Go offered
}

// Result is the outcome of the acceptance sequence.
type Result struct {
	Applied       bool   `json:"applied"`
	Switched      bool   `json:"switched"`
	Reason        string `json:"reason"`
	PolicyVersion uint64 `json:"policy_version"`
}

// SafetyEvent records a fallback or kill-switch action.
type SafetyEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // "fallback" | "kill" | "resume"
	Reason string    `json:"reason"`
}

// ManagerStatus is the externally visible state.
type ManagerStatus struct {
	Config       ManagerConfig  `json:"config"`
	Killed       bool           `json:"killed"`
	Baseline     string         `json:"baseline_strategy"`
	LockedUntil  time.Time      `json:"safety_locked_until"`
	LastSwitchAt time.Time      `json:"last_switch_at"`
	Reasons      map[string]int `json:"reason_counts"`
	SafetyEvents []SafetyEvent  `json:"safety_events"`
}

// PolicyManager validates AI proposals and publishes immutable policy snapshots
// through the pool. The request path never calls into the manager: it only loads
// the active policy atomically.
type PolicyManager struct {
	pool *ServerPool
	cfg  ManagerConfig

	mu          sync.Mutex
	killed      bool
	baseline    policy.ActionID
	lastSwitch  time.Time
	lockedUntil time.Time
	reasons     map[string]int
	events      []SafetyEvent
}

// NewPolicyManager creates a manager. lastSwitch starts at the creation time so
// the very first AI switch also respects the minimum policy age.
func NewPolicyManager(pool *ServerPool, cfg ManagerConfig, now time.Time) *PolicyManager {
	return &PolicyManager{pool: pool, cfg: cfg, baseline: cfg.SafeAction, lastSwitch: now, reasons: map[string]int{}}
}

func (m *PolicyManager) count(reason string) { m.reasons[reason]++ }

// Propose runs the acceptance sequence. Every branch records a reason code.
//
//	enabled? -> weights/IDs valid? -> fresh? -> safety constraints ->
//	hysteresis (min age, switch margin) -> build immutable policy -> atomic publish
func (m *PolicyManager) Propose(p Proposal, now time.Time) Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := m.propose(p, now)
	m.count(res.Reason)
	return res
}

func (m *PolicyManager) reject(reason string) Result {
	return Result{Reason: reason, PolicyVersion: m.pool.Policies().Current().Version}
}

func (m *PolicyManager) propose(p Proposal, now time.Time) Result {
	if m.killed {
		return m.reject(ReasonKilled)
	}
	if now.Before(m.lockedUntil) {
		return m.reject(ReasonSafetyLock)
	}
	// 1. strategy is known, was offered, and is enabled under the current configuration
	action, ok := policy.ActionFromWire(p.Strategy)
	if !ok {
		return m.reject(ReasonUnknownStrategy)
	}
	offered := false
	for _, s := range p.AllowedStrategies {
		if s == p.Strategy {
			offered = true
		}
	}
	if !offered {
		return m.reject(ReasonNotAllowed)
	}
	if err := m.pool.ValidateAction(action); err != nil {
		return m.reject(ReasonDisabled)
	}
	// 2. weights / backend IDs
	ids, weights, wreason := m.checkWeights(action, p.Weights)
	if wreason != "" {
		return m.reject(wreason)
	}
	// 3. freshness of the decision
	if now.Sub(p.RequestedAt) > m.cfg.MaxDecisionAge {
		return m.reject(ReasonStale)
	}
	// 4. safety constraints: never act when nothing can serve traffic
	if m.pool.healthyCount() == 0 {
		return m.reject(ReasonNoHealthy)
	}
	cur, _ := policy.ActionForStrategy(policy.StrategyType(m.pool.GetStrategy().Name()))
	if action == cur {
		if weights != nil {
			if _, ok := m.pool.PublishAdaptiveWeightsReason(ids, weights, "ai:weights"); !ok {
				return m.reject(ReasonApplyFailed)
			}
			return Result{Applied: true, Reason: ReasonApplied, PolicyVersion: m.pool.Policies().Current().Version}
		}
		return Result{Reason: ReasonRetainedSame, PolicyVersion: m.pool.Policies().Current().Version}
	}
	// 5. hysteresis: minimum policy age, then the expected-improvement margin
	if now.Sub(m.lastSwitch) < m.cfg.MinPolicyAge {
		return m.reject(ReasonMinPolicyAge)
	}
	// The margin guards EXPLOITATION (do not switch for a negligible expected gain). First-time
	// exploration of an untried action has, by construction, no expected gain to demonstrate, so
	// it is exempt; min policy age, safety lockout, health and validity checks still apply.
	if !p.Explored && int(action) < len(p.Scores) && int(cur) < len(p.Scores) && p.Scores[action] != nil && p.Scores[cur] != nil {
		if *p.Scores[action]-*p.Scores[cur] < m.cfg.SwitchMargin {
			return m.reject(ReasonSwitchMargin)
		}
	}
	if m.cfg.Shadow {
		return m.reject(ReasonShadow)
	}
	// 6. build + atomically publish (ApplyActionReason constructs a fresh strategy instance)
	if err := m.pool.ApplyActionReason(action, "ai:"+p.Diagnostic); err != nil {
		return m.reject(ReasonApplyFailed)
	}
	if weights != nil {
		m.pool.PublishAdaptiveWeightsReason(ids, weights, "ai:weights")
	}
	m.lastSwitch = now
	return Result{Applied: true, Switched: true, Reason: ReasonApplied, PolicyVersion: m.pool.Policies().Current().Version}
}

// checkWeights validates optional AI weights. They are only meaningful for the
// adaptive strategy, must name every configured backend, and stay within bounds.
func (m *PolicyManager) checkWeights(action policy.ActionID, w map[string]float64) ([]string, []float64, string) {
	if len(w) == 0 {
		return nil, nil, ""
	}
	if action != policy.ActionRuleAdaptive {
		return nil, nil, ReasonUnexpectedWeights
	}
	names := m.pool.backendNames()
	if len(w) != len(names) {
		return nil, nil, ReasonInvalidWeights
	}
	sort.Strings(names)
	out := make([]float64, len(names))
	for i, n := range names {
		v, ok := w[n]
		if !ok || v < m.cfg.MinWeight || v > m.cfg.MaxWeight {
			return nil, nil, ReasonInvalidWeights
		}
		out[i] = v
	}
	return names, out, ""
}

// SafetyFallback switches to the known-safe baseline immediately (bypassing the
// minimum policy age) and locks out AI proposals for SafetyCooldown.
func (m *PolicyManager) SafetyFallback(reason string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.applyBaselineLocked("safety:" + reason); err != nil {
		log.Printf("[Safety] fallback failed: %v", err)
	}
	m.lockedUntil = now.Add(m.cfg.SafetyCooldown)
	m.lastSwitch = now
	m.events = append(m.events, SafetyEvent{At: now, Kind: "fallback", Reason: reason})
	m.trimEvents()
	m.count("safety_fallback")
	log.Printf("[Safety] fallback to baseline (%s); AI locked out until %s", reason, m.lockedUntil.Format(time.RFC3339))
}

func (m *PolicyManager) applyBaselineLocked(reason string) error {
	cur, _ := policy.ActionForStrategy(policy.StrategyType(m.pool.GetStrategy().Name()))
	if cur == m.baseline {
		return nil
	}
	return m.pool.ApplyActionReason(m.baseline, reason)
}

// Kill is the manual kill switch: AI decisions are disabled and the selected
// baseline is applied. Kill(false) re-enables AI proposals (the baseline stays
// active until the next accepted proposal).
func (m *PolicyManager) Kill(on bool, baseline *policy.ActionID, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if baseline != nil {
		if err := m.pool.ValidateAction(*baseline); err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		m.baseline = *baseline
	}
	m.killed = on
	if on {
		if err := m.applyBaselineLocked("kill_switch"); err != nil {
			return err
		}
		m.lastSwitch = now
		m.events = append(m.events, SafetyEvent{At: now, Kind: "kill", Reason: "manual kill switch"})
	} else {
		m.events = append(m.events, SafetyEvent{At: now, Kind: "resume", Reason: "manual resume"})
	}
	m.trimEvents()
	return nil
}

func (m *PolicyManager) trimEvents() {
	if len(m.events) > 50 {
		m.events = append([]SafetyEvent(nil), m.events[len(m.events)-50:]...)
	}
}

// IsKilled reports the kill-switch state.
func (m *PolicyManager) IsKilled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.killed
}

// LockedUntil returns the end of the safety lockout (zero if none).
func (m *PolicyManager) LockedUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockedUntil
}

// Status returns a copy of the externally visible state.
func (m *PolicyManager) Status() ManagerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	rc := make(map[string]int, len(m.reasons))
	for k, v := range m.reasons {
		rc[k] = v
	}
	st, _ := m.baseline.Strategy()
	return ManagerStatus{Config: m.cfg, Killed: m.killed, Baseline: string(st), LockedUntil: m.lockedUntil,
		LastSwitchAt: m.lastSwitch, Reasons: rc, SafetyEvents: append([]SafetyEvent(nil), m.events...)}
}
