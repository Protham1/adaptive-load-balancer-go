package strategy

import (
	"sync"
)

// swrrCore holds the smooth weighted round-robin state shared by the static and
// adaptive variants.
type swrrCore struct {
	mu             sync.Mutex
	currentWeights map[any]int
}

func newSWRRCore() *swrrCore {
	return &swrrCore{currentWeights: make(map[any]int)}
}

// swrrSelect implements Nginx's smooth weighted round-robin. weightOf must
// return a positive integer weight for each alive peer.
func swrrSelect[T Peer](c *swrrCore, peers []T, weightOf func(T) int) T {
	var zero T
	if len(peers) == 0 {
		return zero
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var totalWeight int
	var bestPeer T
	var bestCurrentWeight int
	found := false

	// Cleanup old peers no longer in pool
	activeSet := make(map[any]struct{}, len(peers))
	for _, p := range peers {
		activeSet[any(p)] = struct{}{}
	}
	for k := range c.currentWeights {
		if _, exists := activeSet[k]; !exists {
			delete(c.currentWeights, k)
		}
	}

	for _, p := range peers {
		if !p.IsAlive() {
			continue
		}

		w := weightOf(p)
		if w <= 0 {
			w = 1 // default minimum weight
		}

		key := any(p)
		curr := c.currentWeights[key] + w
		c.currentWeights[key] = curr
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
	c.currentWeights[any(bestPeer)] -= totalWeight

	return bestPeer
}

// SmoothWeightedRoundRobin implements Nginx's smooth weighted round-robin algorithm
// using the statically configured capacity weights.
type SmoothWeightedRoundRobin[T WeightedPeer] struct {
	core *swrrCore
}

// NewSmoothWeightedRoundRobin creates a new instance of SmoothWeightedRoundRobin
func NewSmoothWeightedRoundRobin[T WeightedPeer]() *SmoothWeightedRoundRobin[T] {
	return &SmoothWeightedRoundRobin[T]{core: newSWRRCore()}
}

// Name returns the strategy identifier
func (s *SmoothWeightedRoundRobin[T]) Name() string {
	return "Smooth-Weighted-Round-Robin"
}

// SelectNextBackend selects the next peer according to Nginx smooth weighted round-robin
func (s *SmoothWeightedRoundRobin[T]) SelectNextBackend(peers []T) T {
	return swrrSelect(s.core, peers, func(p T) int { return p.GetWeight() })
}
