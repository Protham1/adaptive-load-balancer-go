package policy

import (
	"fmt"
	"math"
)

// BackendInfo is the minimal backend description needed to validate actions.
type BackendInfo struct {
	ID           string
	StaticWeight int
}

// Constraints describes the current configuration that actions must be valid under.
type Constraints struct {
	Backends          []BackendInfo
	AffinityRequired  bool // Consistent Hash only when session affinity is a requirement
	AdaptiveAvailable bool // the rule-based controller is running
}

// ActionDescriptor is the externally visible description of one action.
type ActionDescriptor struct {
	ID       ActionID     `json:"id"`
	Strategy StrategyType `json:"strategy"`
	Valid    bool         `json:"valid"`
	Reason   string       `json:"reason,omitempty"`
	Notes    string       `json:"notes"`
}

// Validate reports whether action a may be taken under c.
func Validate(a ActionID, c Constraints) error {
	st, ok := a.Strategy()
	if !ok {
		return fmt.Errorf("unknown action id %d (valid: 0..%d)", a, NumActions-1)
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("action %d (%s) invalid: no backends configured", a, st)
	}
	switch a {
	case ActionStaticSWRR:
		if _, err := NormalizeStaticWeights(c.Backends); err != nil {
			return fmt.Errorf("action %d (%s) invalid: %w", a, st, err)
		}
	case ActionConsistentHash:
		if !c.AffinityRequired {
			return fmt.Errorf("action %d (%s) invalid: session affinity is not enabled for this deployment", a, st)
		}
	case ActionRuleAdaptive:
		if !c.AdaptiveAvailable {
			return fmt.Errorf("action %d (%s) invalid: rule-based controller is not running", a, st)
		}
	}
	return nil
}

// Describe lists every action with its validity under c. The list is always the
// full fixed set so an agent can build a stable action mask.
func Describe(c Constraints) []ActionDescriptor {
	out := make([]ActionDescriptor, NumActions)
	for i := 0; i < NumActions; i++ {
		a := ActionID(i)
		st, _ := a.Strategy()
		d := ActionDescriptor{ID: a, Strategy: st, Valid: true, Notes: actionNotes[i]}
		if err := Validate(a, c); err != nil {
			d.Valid, d.Reason = false, err.Error()
		}
		out[i] = d
	}
	return out
}

// ActionMask returns a boolean validity mask indexed by ActionID.
func ActionMask(c Constraints) []bool {
	m := make([]bool, NumActions)
	for i := range m {
		m[i] = Validate(ActionID(i), c) == nil
	}
	return m
}

// NormalizeStaticWeights validates the configured capacity weights and returns
// them as shares summing to 1, aligned with the given backend order.
func NormalizeStaticWeights(bs []BackendInfo) ([]float64, error) {
	if len(bs) == 0 {
		return nil, fmt.Errorf("no backends")
	}
	total := 0.0
	for _, b := range bs {
		if b.StaticWeight <= 0 {
			return nil, fmt.Errorf("backend %q has non-positive static weight %d", b.ID, b.StaticWeight)
		}
		total += float64(b.StaticWeight)
	}
	if total <= 0 || math.IsInf(total, 0) || math.IsNaN(total) {
		return nil, fmt.Errorf("static weights sum to an invalid total")
	}
	out := make([]float64, len(bs))
	for i, b := range bs {
		out[i] = float64(b.StaticWeight) / total
	}
	return out, nil
}
