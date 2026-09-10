package strategy

import (
	"sync"
)

// SmoothWeightedRoundRobin implements Nginx's smooth weighted round-robin algorithm
type SmoothWeightedRoundRobin[T WeightedPeer] struct {
	mu             sync.Mutex
	currentWeights map[any]int
}

// NewSmoothWeightedRoundRobin creates a new instance of SmoothWeightedRoundRobin
func NewSmoothWeightedRoundRobin[T WeightedPeer]() *SmoothWeightedRoundRobin[T] {
	return &SmoothWeightedRoundRobin[T]{
		currentWeights: make(map[any]int),
	}
}

// Name returns the strategy identifier
func (s *SmoothWeightedRoundRobin[T]) Name() string {
	return "Smooth-Weighted-Round-Robin"
}

// SelectNextBackend selects the next peer according to Nginx smooth weighted round-robin
func (s *SmoothWeightedRoundRobin[T]) SelectNextBackend(peers []T) T {
	var zero T
	if len(peers) == 0 {
		return zero
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var totalWeight int
	var bestPeer T
	var bestCurrentWeight int
	found := false

	// Cleanup old peers no longer in pool
	activeSet := make(map[any]struct{}, len(peers))
	for _, p := range peers {
		activeSet[any(p)] = struct{}{}
	}
	for k := range s.currentWeights {
		if _, exists := activeSet[k]; !exists {
			delete(s.currentWeights, k)
		}
	}

	for _, p := range peers {
		if !p.IsAlive() {
			continue
		}

		w := p.GetWeight()
		if w <= 0 {
			w = 1 // default minimum weight
		}

		key := any(p)
		curr := s.currentWeights[key] + w
		s.currentWeights[key] = curr
		totalWeight += w

		if !found || curr > bestCurrentWeight {
			found = true
			bestCurrentWeight = curr
			bestPeer = p
		}
	}

	if !found {
		return zero
	}

	// Decrement total weight from selected peer's current weight
	s.currentWeights[any(bestPeer)] -= totalWeight

	return bestPeer
}
