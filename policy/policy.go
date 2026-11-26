// Package policy defines the action space and the immutable policy snapshot that
// the request path reads atomically (Phase 6).
//
// A Policy is complete and immutable once published: the weights slice is copied
// on publish and never mutated afterwards, so request handlers can read it
// without locks while a controller (rule-based or learned) builds the next one.
package policy

import (
	"fmt"
	"math"
	"time"
)

// StrategyType is the name of a load-balancing strategy. The values match
// strategy.SelectionStrategy.Name() so a policy can be mapped to a strategy
// without a separate lookup table.
type StrategyType string

const (
	RoundRobin       StrategyType = "Round-Robin"
	LeastConnections StrategyType = "Least-Connections"
	StaticSWRR       StrategyType = "Smooth-Weighted-Round-Robin"
	ConsistentHash   StrategyType = "Consistent-Hash"
	AdaptiveSWRR     StrategyType = "Adaptive-Weighted-Round-Robin"
)

// ActionID is the stable numeric identifier of an agent action. IDs are part of
// the agent contract and must never be renumbered.
type ActionID int

const (
	ActionRoundRobin       ActionID = 0
	ActionLeastConnections ActionID = 1
	ActionStaticSWRR       ActionID = 2
	ActionConsistentHash   ActionID = 3
	ActionRuleAdaptive     ActionID = 4
)

// NumActions is the size of the action space.
const NumActions = 5

var actionStrategies = [NumActions]StrategyType{
	ActionRoundRobin:       RoundRobin,
	ActionLeastConnections: LeastConnections,
	ActionStaticSWRR:       StaticSWRR,
	ActionConsistentHash:   ConsistentHash,
	ActionRuleAdaptive:     AdaptiveSWRR,
}

var actionNotes = [NumActions]string{
	"Baseline for roughly homogeneous backends and short stateless requests.",
	"Useful when active requests are a reasonable proxy for remaining work.",
	"Uses configured capacity weights; weights must be normalized and valid.",
	"Only when session affinity is required; optimizes affinity, not even distribution.",
	"Rule-based (non-AI) metric controller producing bounded dynamic weights.",
}

// Strategy returns the strategy executed by the action.
func (a ActionID) Strategy() (StrategyType, bool) {
	if a < 0 || int(a) >= NumActions {
		return "", false
	}
	return actionStrategies[a], true
}

// ActionForStrategy is the inverse of ActionID.Strategy.
func ActionForStrategy(s StrategyType) (ActionID, bool) {
	for i, st := range actionStrategies {
		if st == s {
			return ActionID(i), true
		}
	}
	return -1, false
}

// Policy is a complete, immutable policy snapshot. Weights are relative
// per-backend shares aligned index-by-index with BackendIDs (stable sorted
// order, the same order as the State v1 vector). Treat every field as
// read-only after publication.
type Policy struct {
	Strategy   StrategyType `json:"strategy"`
	Weights    []float64    `json:"weights,omitempty"`
	BackendIDs []string     `json:"backend_ids,omitempty"`
	Version    uint64       `json:"version"`
	CreatedAt  time.Time    `json:"created_at"`

	index map[string]float64 // built once at publish, never mutated
}

// WeightFor returns the weight assigned to backend id in this snapshot.
func (p *Policy) WeightFor(id string) (float64, bool) {
	if p == nil {
		return 0, false
	}
	w, ok := p.index[id]
	return w, ok
}

// Action returns the action ID corresponding to the policy's strategy.
func (p *Policy) Action() (ActionID, bool) { return ActionForStrategy(p.Strategy) }

// validateWeights checks that ids/weights are consistent and usable.
func validateWeights(ids []string, weights []float64) error {
	if len(ids) != len(weights) {
		return fmt.Errorf("policy: %d weights for %d backends", len(weights), len(ids))
	}
	seen := make(map[string]struct{}, len(ids))
	for i, w := range weights {
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return fmt.Errorf("policy: invalid weight %v for backend %q", w, ids[i])
		}
		if _, dup := seen[ids[i]]; dup {
			return fmt.Errorf("policy: duplicate backend id %q", ids[i])
		}
		seen[ids[i]] = struct{}{}
	}
	return nil
}

func newPolicy(st StrategyType, ids []string, weights []float64, version uint64) *Policy {
	p := &Policy{
		Strategy:  st,
		Version:   version,
		CreatedAt: time.Now(),
	}
	if len(ids) > 0 {
		p.BackendIDs = append([]string(nil), ids...)
		p.Weights = append([]float64(nil), weights...)
		p.index = make(map[string]float64, len(ids))
		for i, id := range ids {
			p.index[id] = weights[i]
		}
	}
	return p
}
