package strategy

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

const defaultVirtualReplicas = 100

// ConsistentHashStrategy implements consistent hashing with virtual nodes
type ConsistentHashStrategy[T HashablePeer] struct {
	mu              sync.RWMutex
	virtualReplicas int
	ring            []uint32
	ringMap         map[uint32]T
	lastPeerIDs     string
	fallbackCounter uint64
}

// NewConsistentHash creates a new ConsistentHashStrategy with default virtual replicas
func NewConsistentHash[T HashablePeer](replicas ...int) *ConsistentHashStrategy[T] {
	v := defaultVirtualReplicas
	if len(replicas) > 0 && replicas[0] > 0 {
		v = replicas[0]
	}
	return &ConsistentHashStrategy[T]{
		virtualReplicas: v,
		ringMap:         make(map[uint32]T),
	}
}

// Name returns the strategy identifier
func (c *ConsistentHashStrategy[T]) Name() string {
	return "Consistent-Hash"
}

// SelectNextBackend implements SelectionStrategy without an explicit key by using an internal counter
func (c *ConsistentHashStrategy[T]) SelectNextBackend(peers []T) T {
	counter := atomic.AddUint64(&c.fallbackCounter, 1)
	return c.SelectNextBackendWithKey(peers, strconv.FormatUint(counter, 10))
}

// SelectNextBackendWithKey routes requests based on the given routing key (e.g., Client IP or session ID)
func (c *ConsistentHashStrategy[T]) SelectNextBackendWithKey(peers []T, key string) T {
	var zero T
	if len(peers) == 0 {
		return zero
	}

	c.ensureRing(peers)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.ring) == 0 {
		return zero
	}

	hash := hashKey(key)
	idx := sort.Search(len(c.ring), func(i int) bool {
		return c.ring[i] >= hash
	})

	if idx == len(c.ring) {
		idx = 0
	}

	// Walk clockwise from idx to find the first alive backend
	totalRing := len(c.ring)
	for i := 0; i < totalRing; i++ {
		targetIdx := (idx + i) % totalRing
		peer := c.ringMap[c.ring[targetIdx]]
		if peer.IsAlive() {
			return peer
		}
	}

	return zero
}

// ensureRing updates the ring if the peers in the pool have changed
func (c *ConsistentHashStrategy[T]) ensureRing(peers []T) {
	c.mu.RLock()
	currentSignature := computePeerSignature(peers)
	if currentSignature == c.lastPeerIDs && len(c.ring) > 0 {
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if currentSignature == c.lastPeerIDs && len(c.ring) > 0 {
		return
	}

	c.ring = make([]uint32, 0, len(peers)*c.virtualReplicas)
	c.ringMap = make(map[uint32]T, len(peers)*c.virtualReplicas)

	for _, p := range peers {
		id := p.GetID()
		for v := 0; v < c.virtualReplicas; v++ {
			vKey := fmt.Sprintf("%s#vn%d", id, v)
			h := hashKey(vKey)
			c.ring = append(c.ring, h)
			c.ringMap[h] = p
		}
	}

	sort.Slice(c.ring, func(i, j int) bool {
		return c.ring[i] < c.ring[j]
	})

	c.lastPeerIDs = currentSignature
}

// computePeerSignature creates a fast identifier for the peer slice
func computePeerSignature[T HashablePeer](peers []T) string {
	var sig string
	for _, p := range peers {
		sig += p.GetID() + ";"
	}
	return sig
}

// hashKey computes a 32-bit FNV-1a hash
func hashKey(key string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	return hasher.Sum32()
}
