package strategy

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// mockConcurrentPeer is a thread-safe mock for generic peer testing
type mockConcurrentPeer struct {
	id    string
	alive bool
	conns int64
	w     int
	mu    sync.RWMutex
}

func (m *mockConcurrentPeer) IsAlive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.alive
}

func (m *mockConcurrentPeer) SetAlive(alive bool) {
	m.mu.Lock()
	m.alive = alive
	m.mu.Unlock()
}

func (m *mockConcurrentPeer) GetActiveConnections() int64 {
	return atomic.LoadInt64(&m.conns)
}

func (m *mockConcurrentPeer) IncConns() int64 {
	return atomic.AddInt64(&m.conns, 1)
}

func (m *mockConcurrentPeer) DecConns() int64 {
	return atomic.AddInt64(&m.conns, -1)
}

func (m *mockConcurrentPeer) GetWeight() int {
	return m.w
}

func (m *mockConcurrentPeer) GetID() string {
	return m.id
}

func TestConcurrentRoundRobin(t *testing.T) {
	p1 := &mockConcurrentPeer{id: "p1", alive: true}
	p2 := &mockConcurrentPeer{id: "p2", alive: true}
	p3 := &mockConcurrentPeer{id: "p3", alive: true}
	peers := []*mockConcurrentPeer{p1, p2, p3}

	rr := NewRoundRobin[*mockConcurrentPeer]()
	const totalRoutines = 20
	const iterationsPerRoutine = 100

	var wg sync.WaitGroup
	wg.Add(totalRoutines)

	counts := make([]uint64, 3)

	for i := 0; i < totalRoutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerRoutine; j++ {
				selected := rr.SelectNextBackend(peers)
				if selected == nil {
					t.Errorf("expected non-nil peer")
					continue
				}
				switch selected.id {
				case "p1":
					atomic.AddUint64(&counts[0], 1)
				case "p2":
					atomic.AddUint64(&counts[1], 1)
				case "p3":
					atomic.AddUint64(&counts[2], 1)
				}
			}
		}()
	}

	wg.Wait()

	total := counts[0] + counts[1] + counts[2]
	if total != totalRoutines*iterationsPerRoutine {
		t.Errorf("expected total selections %d, got %d", totalRoutines*iterationsPerRoutine, total)
	}
}

func TestConcurrentLeastConnections(t *testing.T) {
	p1 := &mockConcurrentPeer{id: "p1", alive: true}
	p2 := &mockConcurrentPeer{id: "p2", alive: true}
	peers := []*mockConcurrentPeer{p1, p2}

	lc := NewLeastConnections[*mockConcurrentPeer]()
	const totalRoutines = 20
	const iterationsPerRoutine = 50

	var wg sync.WaitGroup
	wg.Add(totalRoutines)

	for i := 0; i < totalRoutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerRoutine; j++ {
				selected := lc.SelectNextBackend(peers)
				if selected == nil {
					t.Errorf("expected non-nil peer")
					continue
				}
				selected.IncConns()
				// simulate very fast processing
				selected.DecConns()
			}
		}()
	}

	wg.Wait()

	if p1.GetActiveConnections() != 0 || p2.GetActiveConnections() != 0 {
		t.Errorf("leaked connections: p1=%d, p2=%d", p1.GetActiveConnections(), p2.GetActiveConnections())
	}
}

func TestConcurrentSmoothWeightedRoundRobin(t *testing.T) {
	p1 := &mockConcurrentPeer{id: "p1", alive: true, w: 3}
	p2 := &mockConcurrentPeer{id: "p2", alive: true, w: 1}
	peers := []*mockConcurrentPeer{p1, p2}

	swrr := NewSmoothWeightedRoundRobin[*mockConcurrentPeer]()
	const totalRoutines = 10
	const iterationsPerRoutine = 40

	var wg sync.WaitGroup
	wg.Add(totalRoutines)

	var p1Count, p2Count uint64

	for i := 0; i < totalRoutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerRoutine; j++ {
				selected := swrr.SelectNextBackend(peers)
				if selected == nil {
					t.Errorf("expected non-nil peer")
					continue
				}
				if selected.id == "p1" {
					atomic.AddUint64(&p1Count, 1)
				} else {
					atomic.AddUint64(&p2Count, 1)
				}
			}
		}()
	}

	wg.Wait()

	total := p1Count + p2Count
	if total != totalRoutines*iterationsPerRoutine {
		t.Errorf("expected total %d, got %d", totalRoutines*iterationsPerRoutine, total)
	}
	// Ratio should approximately reflect 3:1
	if p1Count <= p2Count {
		t.Errorf("expected p1 (%d) > p2 (%d) for 3:1 weight ratio", p1Count, p2Count)
	}
}

func TestConcurrentConsistentHash(t *testing.T) {
	p1 := &mockConcurrentPeer{id: "p1", alive: true}
	p2 := &mockConcurrentPeer{id: "p2", alive: true}
	p3 := &mockConcurrentPeer{id: "p3", alive: true}
	peers := []*mockConcurrentPeer{p1, p2, p3}

	ch := NewConsistentHash[*mockConcurrentPeer](100)
	const totalRoutines = 20
	const iterationsPerRoutine = 50

	var wg sync.WaitGroup
	wg.Add(totalRoutines)

	for i := 0; i < totalRoutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			clientIP := fmt.Sprintf("192.168.1.%d", workerID)
			initial := ch.SelectNextBackendWithKey(peers, clientIP)
			if initial == nil {
				t.Errorf("expected non-nil peer")
				return
			}
			for j := 0; j < iterationsPerRoutine; j++ {
				selected := ch.SelectNextBackendWithKey(peers, clientIP)
				if selected != initial {
					t.Errorf("affinity broken during concurrency: worker %d expected %s, got %s",
						workerID, initial.id, selected.id)
				}
			}
		}(i)
	}

	wg.Wait()
}
