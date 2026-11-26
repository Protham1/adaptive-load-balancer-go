package strategy

import (
	"sync/atomic"
	"testing"
)

type adPeer struct {
	name   string
	alive  atomic.Bool
	weight int
}

func newAdPeer(name string, w int) *adPeer {
	p := &adPeer{name: name, weight: w}
	p.alive.Store(true)
	return p
}
func (p *adPeer) IsAlive() bool   { return p.alive.Load() }
func (p *adPeer) GetWeight() int  { return p.weight }
func (p *adPeer) GetName() string { return p.name }

type mapLookup map[string]float64

func (m mapLookup) WeightFor(id string) (float64, bool) { w, ok := m[id]; return w, ok }

func distribution(s *AdaptiveWeightedRoundRobin[*adPeer], peers []*adPeer, n int) map[string]int {
	out := map[string]int{}
	for i := 0; i < n; i++ {
		if p := s.SelectNextBackend(peers); p != nil {
			out[p.name]++
		}
	}
	return out
}

func TestAdaptiveFollowsPolicyWeights(t *testing.T) {
	peers := []*adPeer{newAdPeer("a", 1), newAdPeer("b", 1)}
	s := NewAdaptiveWeightedRoundRobin[*adPeer](func() WeightLookup { return mapLookup{"a": 0.75, "b": 0.25} })
	d := distribution(s, peers, 400)
	if d["a"] != 300 || d["b"] != 100 {
		t.Fatalf("expected 300/100, got %v", d)
	}
}

func TestAdaptiveFallsBackToStaticWeights(t *testing.T) {
	peers := []*adPeer{newAdPeer("a", 3), newAdPeer("b", 1)}
	s := NewAdaptiveWeightedRoundRobin[*adPeer](nil)
	d := distribution(s, peers, 400)
	if d["a"] != 300 || d["b"] != 100 {
		t.Fatalf("static fallback expected 300/100, got %v", d)
	}
}

func TestAdaptiveSkipsUnhealthyAndRecovers(t *testing.T) {
	peers := []*adPeer{newAdPeer("a", 1), newAdPeer("b", 1), newAdPeer("c", 1)}
	s := NewAdaptiveWeightedRoundRobin[*adPeer](func() WeightLookup { return mapLookup{"a": 0.5, "b": 0.3, "c": 0.2} })
	peers[0].alive.Store(false)
	d := distribution(s, peers, 100)
	if d["a"] != 0 || d["b"]+d["c"] != 100 {
		t.Fatalf("unhealthy backend must get no traffic: %v", d)
	}
	peers[0].alive.Store(true)
	d = distribution(s, peers, 1000)
	if d["a"] < 450 || d["a"] > 550 {
		t.Fatalf("recovered backend should get ~50%%: %v", d)
	}
}

func TestAdaptiveEdgeCases(t *testing.T) {
	s := NewAdaptiveWeightedRoundRobin[*adPeer](nil)
	if s.SelectNextBackend(nil) != nil {
		t.Error("empty pool must return nil")
	}
	peers := []*adPeer{newAdPeer("a", 1)}
	peers[0].alive.Store(false)
	if s.SelectNextBackend(peers) != nil {
		t.Error("all-unhealthy pool must return nil")
	}
	// A zero weight must not starve a healthy backend entirely.
	peers[0].alive.Store(true)
	peers = append(peers, newAdPeer("b", 1))
	s = NewAdaptiveWeightedRoundRobin[*adPeer](func() WeightLookup { return mapLookup{"a": 0, "b": 1} })
	if d := distribution(s, peers, 5000); d["a"] == 0 {
		t.Errorf("zero-weight healthy backend starved: %v", d)
	}
}

func TestAdaptiveWeightsSwapMidStream(t *testing.T) {
	peers := []*adPeer{newAdPeer("a", 1), newAdPeer("b", 1)}
	var cur atomic.Value
	cur.Store(mapLookup{"a": 0.9, "b": 0.1})
	s := NewAdaptiveWeightedRoundRobin[*adPeer](func() WeightLookup { return cur.Load().(mapLookup) })
	_ = distribution(s, peers, 100)
	cur.Store(mapLookup{"a": 0.1, "b": 0.9})
	d := distribution(s, peers, 2000)
	if d["b"] < 1600 {
		t.Fatalf("new weights should take over: %v", d)
	}
}
