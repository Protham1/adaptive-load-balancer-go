package strategy

import (
	"testing"
)

type mockPeer struct {
	id    string
	alive bool
}

func (m *mockPeer) IsAlive() bool {
	return m.alive
}

func TestRoundRobinStrategy(t *testing.T) {
	p1 := &mockPeer{id: "p1", alive: true}
	p2 := &mockPeer{id: "p2", alive: true}
	p3 := &mockPeer{id: "p3", alive: true}

	peers := []*mockPeer{p1, p2, p3}
	rr := NewRoundRobin[*mockPeer]()

	if rr.Name() != "Round-Robin" {
		t.Errorf("expected name Round-Robin, got %s", rr.Name())
	}

	expected := []*mockPeer{p2, p3, p1, p2, p3, p1}
	for i, exp := range expected {
		selected := rr.SelectNextBackend(peers)
		if selected != exp {
			t.Errorf("Iteration %d: expected %s, got %s", i, exp.id, selected.id)
		}
	}
}

func TestRoundRobinSkipDead(t *testing.T) {
	p1 := &mockPeer{id: "p1", alive: true}
	p2 := &mockPeer{id: "p2", alive: false}
	p3 := &mockPeer{id: "p3", alive: true}

	peers := []*mockPeer{p1, p2, p3}
	rr := NewRoundRobin[*mockPeer]()

	for i := 0; i < 4; i++ {
		selected := rr.SelectNextBackend(peers)
		if selected == p2 {
			t.Errorf("Iteration %d: dead peer p2 was selected", i)
		}
	}
}

func TestRoundRobinEmptyOrAllDead(t *testing.T) {
	rr := NewRoundRobin[*mockPeer]()

	if res := rr.SelectNextBackend([]*mockPeer{}); res != nil {
		t.Errorf("expected nil for empty peers, got %v", res)
	}

	p1 := &mockPeer{id: "p1", alive: false}
	if res := rr.SelectNextBackend([]*mockPeer{p1}); res != nil {
		t.Errorf("expected nil for all dead peers, got %v", res)
	}
}
