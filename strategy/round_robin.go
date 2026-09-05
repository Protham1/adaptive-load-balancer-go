package strategy

import (
	"sync/atomic"
)

// RoundRobinStrategy implements round-robin backend peer selection
type RoundRobinStrategy[T Peer] struct {
	current uint64
}

// NewRoundRobin creates a new instance of RoundRobinStrategy
func NewRoundRobin[T Peer]() *RoundRobinStrategy[T] {
	return &RoundRobinStrategy[T]{}
}

// Name returns the identifier of this strategy
func (r *RoundRobinStrategy[T]) Name() string {
	return "Round-Robin"
}

// SelectNextBackend atomically increments the counter and returns the next alive backend peer
func (r *RoundRobinStrategy[T]) SelectNextBackend(peers []T) T {
	var zero T
	if len(peers) == 0 {
		return zero
	}

	next := int(atomic.AddUint64(&r.current, uint64(1)) % uint64(len(peers)))
	l := len(peers) + next
	for i := next; i < l; i++ {
		idx := i % len(peers)
		if peers[idx].IsAlive() {
			if i != next {
				atomic.StoreUint64(&r.current, uint64(idx))
			}
			return peers[idx]
		}
	}
	return zero
}
