package strategy

import (
	"sync/atomic"
)

// LeastConnectionsStrategy selects the alive peer with the fewest active connections
type LeastConnectionsStrategy[T ConnectionPeer] struct {
	tieBreaker uint64
}

// NewLeastConnections creates a new instance of LeastConnectionsStrategy
func NewLeastConnections[T ConnectionPeer]() *LeastConnectionsStrategy[T] {
	return &LeastConnectionsStrategy[T]{}
}

// Name returns the strategy identifier
func (s *LeastConnectionsStrategy[T]) Name() string {
	return "Least-Connections"
}

// SelectNextBackend selects the alive backend with the lowest active connections.
// If multiple backends have the same minimum active connections, round-robin tie-breaking is used.
func (s *LeastConnectionsStrategy[T]) SelectNextBackend(peers []T) T {
	var zero T
	if len(peers) == 0 {
		return zero
	}

	alivePeers := make([]T, 0, len(peers))
	for _, p := range peers {
		if p.IsAlive() {
			alivePeers = append(alivePeers, p)
		}
	}

	if len(alivePeers) == 0 {
		return zero
	}

	minConns := alivePeers[0].GetActiveConnections()
	leastLoaded := []T{alivePeers[0]}

	for _, p := range alivePeers[1:] {
		conns := p.GetActiveConnections()
		if conns < minConns {
			minConns = conns
			leastLoaded = []T{p}
		} else if conns == minConns {
			leastLoaded = append(leastLoaded, p)
		}
	}

	if len(leastLoaded) == 1 {
		return leastLoaded[0]
	}

	// Distribute evenly among tied backends
	idx := atomic.AddUint64(&s.tieBreaker, 1) % uint64(len(leastLoaded))
	return leastLoaded[idx]
}
