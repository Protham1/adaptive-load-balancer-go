package strategy

// Peer represents an entity that can be checked for liveness
type Peer interface {
	IsAlive() bool
}

// ConnectionPeer represents a peer with active connection metrics
type ConnectionPeer interface {
	Peer
	GetActiveConnections() int64
}

// WeightedPeer represents a peer with capacity weight
type WeightedPeer interface {
	Peer
	GetWeight() int
}

// HashablePeer represents a peer with a unique identification key for hash rings
type HashablePeer interface {
	Peer
	GetID() string
}

// SelectionStrategy defines the algorithm contract for selecting the next backend peer
type SelectionStrategy[T Peer] interface {
	// SelectNextBackend selects and returns the next alive backend from the given list, or nil if none are available
	SelectNextBackend(peers []T) T
	// Name returns the identifier of the strategy (e.g., "Round-Robin", "Least-Connections")
	Name() string
}

// KeyedSelectionStrategy defines an algorithm contract that routes traffic based on a request routing key (e.g., Client IP or session ID)
type KeyedSelectionStrategy[T Peer] interface {
	SelectionStrategy[T]
	// SelectNextBackendWithKey selects the backend based on a specific routing key
	SelectNextBackendWithKey(peers []T, key string) T
}
