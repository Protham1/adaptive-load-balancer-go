package strategy

// Peer represents an entity that can be checked for liveness
type Peer interface {
	IsAlive() bool
}

// SelectionStrategy defines the algorithm contract for selecting the next backend peer
type SelectionStrategy[T Peer] interface {
	// SelectNextBackend selects and returns the next alive backend from the given list, or nil if none are available
	SelectNextBackend(peers []T) T
	// Name returns the identifier of the strategy (e.g., "Round-Robin", "Least-Connections")
	Name() string
}
