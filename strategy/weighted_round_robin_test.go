package strategy

import (
	"testing"
)

type mockWeightedPeer struct {
	id     string
	alive  bool
	weight int
}

func (m *mockWeightedPeer) IsAlive() bool {
	return m.alive
}

func (m *mockWeightedPeer) GetWeight() int {
	return m.weight
}

func TestSmoothWeightedDistribution(t *testing.T) {
	pA := &mockWeightedPeer{id: "A", alive: true, weight: 5}
	pB := &mockWeightedPeer{id: "B", alive: true, weight: 1}
	pC := &mockWeightedPeer{id: "C", alive: true, weight: 1}

	peers := []*mockWeightedPeer{pA, pB, pC}
	swrr := NewSmoothWeightedRoundRobin[*mockWeightedPeer]()

	if swrr.Name() != "Smooth-Weighted-Round-Robin" {
		t.Errorf("expected name Smooth-Weighted-Round-Robin, got %s", swrr.Name())
	}

	// Expected smooth sequence over 7 requests: A, A, B, A, C, A, A
	expectedSeq := []string{"A", "A", "B", "A", "C", "A", "A"}
	counts := make(map[string]int)

	for i, expID := range expectedSeq {
		selected := swrr.SelectNextBackend(peers)
		if selected == nil {
			t.Fatalf("iteration %d: selected nil peer", i)
		}
		if selected.id != expID {
			t.Errorf("iteration %d: expected %s, got %s", i, expID, selected.id)
		}
		counts[selected.id]++
	}

	if counts["A"] != 5 || counts["B"] != 1 || counts["C"] != 1 {
		t.Errorf("expected counts A:5, B:1, C:1, got A:%d, B:%d, C:%d", counts["A"], counts["B"], counts["C"])
	}
}

func TestSmoothWeightedSkipDead(t *testing.T) {
	pA := &mockWeightedPeer{id: "A", alive: false, weight: 10}
	pB := &mockWeightedPeer{id: "B", alive: true, weight: 1}
	pC := &mockWeightedPeer{id: "C", alive: true, weight: 1}

	peers := []*mockWeightedPeer{pA, pB, pC}
	swrr := NewSmoothWeightedRoundRobin[*mockWeightedPeer]()

	// With pA dead, pB and pC should alternate evenly
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		selected := swrr.SelectNextBackend(peers)
		if selected == pA {
			t.Fatalf("iteration %d: dead peer pA was selected", i)
		}
		counts[selected.id]++
	}

	if counts["B"] != 3 || counts["C"] != 3 {
		t.Errorf("expected B:3, C:3, got B:%d, C:%d", counts["B"], counts["C"])
	}
}

func TestSmoothWeightedEmptyOrAllDead(t *testing.T) {
	swrr := NewSmoothWeightedRoundRobin[*mockWeightedPeer]()

	if res := swrr.SelectNextBackend([]*mockWeightedPeer{}); res != nil {
		t.Errorf("expected nil for empty peers, got %v", res)
	}

	p1 := &mockWeightedPeer{id: "p1", alive: false, weight: 5}
	if res := swrr.SelectNextBackend([]*mockWeightedPeer{p1}); res != nil {
		t.Errorf("expected nil for all dead peers, got %v", res)
	}
}
