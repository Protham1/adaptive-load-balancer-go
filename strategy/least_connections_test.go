package strategy

import (
	"sync/atomic"
	"testing"
)

type mockConnPeer struct {
	id    string
	alive bool
	conns int64
}

func (m *mockConnPeer) IsAlive() bool {
	return m.alive
}

func (m *mockConnPeer) GetActiveConnections() int64 {
	return atomic.LoadInt64(&m.conns)
}

func (m *mockConnPeer) setConns(c int64) {
	atomic.StoreInt64(&m.conns, c)
}

func TestLeastConnectionsStrategy(t *testing.T) {
	p1 := &mockConnPeer{id: "p1", alive: true, conns: 5}
	p2 := &mockConnPeer{id: "p2", alive: true, conns: 1}
	p3 := &mockConnPeer{id: "p3", alive: true, conns: 3}

	peers := []*mockConnPeer{p1, p2, p3}
	lc := NewLeastConnections[*mockConnPeer]()

	if lc.Name() != "Least-Connections" {
		t.Errorf("expected name Least-Connections, got %s", lc.Name())
	}

	// Should select p2 (1 conn)
	selected := lc.SelectNextBackend(peers)
	if selected != p2 {
		t.Fatalf("expected p2, got %v", selected)
	}

	// Update p2 connections to 6 -> now p3 (3 conns) is the least loaded
	p2.setConns(6)
	selected = lc.SelectNextBackend(peers)
	if selected != p3 {
		t.Fatalf("expected p3, got %v", selected)
	}
}

func TestLeastConnectionsTieBreaker(t *testing.T) {
	p1 := &mockConnPeer{id: "p1", alive: true, conns: 2}
	p2 := &mockConnPeer{id: "p2", alive: true, conns: 2}

	peers := []*mockConnPeer{p1, p2}
	lc := NewLeastConnections[*mockConnPeer]()

	// Across multiple calls with tied connections, should round-robin between p1 and p2
	s1 := lc.SelectNextBackend(peers)
	s2 := lc.SelectNextBackend(peers)

	if s1 == s2 {
		t.Errorf("expected round-robin tie breaker to alternate, got %s and %s", s1.id, s2.id)
	}
}

func TestLeastConnectionsSkipDead(t *testing.T) {
	p1 := &mockConnPeer{id: "p1", alive: false, conns: 0}
	p2 := &mockConnPeer{id: "p2", alive: true, conns: 5}

	peers := []*mockConnPeer{p1, p2}
	lc := NewLeastConnections[*mockConnPeer]()

	selected := lc.SelectNextBackend(peers)
	if selected != p2 {
		t.Fatalf("expected p2 since p1 is dead despite 0 connections, got %v", selected)
	}
}

func TestLeastConnectionsEmptyOrAllDead(t *testing.T) {
	lc := NewLeastConnections[*mockConnPeer]()

	if res := lc.SelectNextBackend([]*mockConnPeer{}); res != nil {
		t.Errorf("expected nil for empty peers, got %v", res)
	}

	p1 := &mockConnPeer{id: "p1", alive: false, conns: 1}
	if res := lc.SelectNextBackend([]*mockConnPeer{p1}); res != nil {
		t.Errorf("expected nil for all dead peers, got %v", res)
	}
}
