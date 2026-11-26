package policy

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Store publishes immutable policies. Readers use Current (lock-free atomic
// load); writers are serialized so versions are strictly increasing.
type Store struct {
	mu      sync.Mutex // serializes writers only
	version uint64
	cur     atomic.Pointer[Policy]
}

// NewStore creates a store whose initial policy (version 1) uses strategy st.
func NewStore(st StrategyType) *Store {
	s := &Store{}
	s.version = 1
	s.cur.Store(newPolicy(st, nil, nil, 1))
	return s
}

// Current returns the active immutable policy. Never nil.
func (s *Store) Current() *Policy { return s.cur.Load() }

// Publish validates, copies, and atomically installs a new policy.
func (s *Store) Publish(st StrategyType, ids []string, weights []float64) (*Policy, error) {
	return s.PublishReason(st, ids, weights, "")
}

// PublishReason is Publish with a reason code recorded on the snapshot.
func (s *Store) PublishReason(st StrategyType, ids []string, weights []float64, reason string) (*Policy, error) {
	if _, ok := ActionForStrategy(st); !ok {
		return nil, fmt.Errorf("policy: unknown strategy %q", st)
	}
	if err := validateWeights(ids, weights); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	p := newPolicy(st, ids, weights, s.version)
	p.Reason = reason
	s.cur.Store(p)
	return p, nil
}

// PublishWeightsIf installs new weights only if the active strategy is still st.
// It returns (nil, false, nil) when the active strategy differs, so a stale
// controller cannot overwrite a policy chosen in the meantime.
func (s *Store) PublishWeightsIf(st StrategyType, ids []string, weights []float64) (*Policy, bool, error) {
	return s.PublishWeightsIfReason(st, ids, weights, "")
}

// PublishWeightsIfReason is PublishWeightsIf with a reason code on the snapshot.
func (s *Store) PublishWeightsIfReason(st StrategyType, ids []string, weights []float64, reason string) (*Policy, bool, error) {
	if err := validateWeights(ids, weights); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Load().Strategy != st {
		return nil, false, nil
	}
	s.version++
	p := newPolicy(st, ids, weights, s.version)
	p.Reason = reason
	s.cur.Store(p)
	return p, true, nil
}
