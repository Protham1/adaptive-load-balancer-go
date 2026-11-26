package policy

import (
	"math"
	"sync"
	"testing"
)

func cons() Constraints {
	return Constraints{
		Backends:          []BackendInfo{{"a", 3}, {"b", 2}, {"c", 1}},
		AdaptiveAvailable: true,
	}
}

func TestActionIDsAreStable(t *testing.T) {
	want := map[ActionID]StrategyType{
		0: RoundRobin, 1: LeastConnections, 2: StaticSWRR, 3: ConsistentHash, 4: AdaptiveSWRR,
	}
	for id, st := range want {
		got, ok := id.Strategy()
		if !ok || got != st {
			t.Fatalf("action %d: got %q ok=%v want %q", id, got, ok, st)
		}
		back, ok := ActionForStrategy(st)
		if !ok || back != id {
			t.Fatalf("inverse mapping broken for %q", st)
		}
	}
	if _, ok := ActionID(5).Strategy(); ok {
		t.Fatal("action 5 must not exist")
	}
	if _, ok := ActionID(-1).Strategy(); ok {
		t.Fatal("action -1 must not exist")
	}
}

func TestValidate(t *testing.T) {
	c := cons()
	for _, a := range []ActionID{ActionRoundRobin, ActionLeastConnections, ActionStaticSWRR, ActionRuleAdaptive} {
		if err := Validate(a, c); err != nil {
			t.Errorf("action %d should be valid: %v", a, err)
		}
	}
	if Validate(ActionConsistentHash, c) == nil {
		t.Error("consistent hash must be invalid without affinity")
	}
	c.AffinityRequired = true
	if err := Validate(ActionConsistentHash, c); err != nil {
		t.Errorf("consistent hash must be valid with affinity: %v", err)
	}
	c.AdaptiveAvailable = false
	if Validate(ActionRuleAdaptive, c) == nil {
		t.Error("adaptive must be invalid without controller")
	}
	c.Backends[1].StaticWeight = 0
	if Validate(ActionStaticSWRR, c) == nil {
		t.Error("static SWRR must be invalid with a zero weight")
	}
	if Validate(ActionID(9), c) == nil {
		t.Error("unknown action must be invalid")
	}
	if Validate(ActionRoundRobin, Constraints{}) == nil {
		t.Error("every action must be invalid on an empty pool")
	}
}

func TestDescribeAndMask(t *testing.T) {
	c := cons()
	d := Describe(c)
	m := ActionMask(c)
	if len(d) != NumActions || len(m) != NumActions {
		t.Fatalf("full action set expected")
	}
	if m[ActionConsistentHash] || d[ActionConsistentHash].Reason == "" {
		t.Error("CH should be masked with a reason")
	}
	if !m[ActionRoundRobin] {
		t.Error("RR should be unmasked")
	}
}

func TestNormalizeStaticWeights(t *testing.T) {
	w, err := NormalizeStaticWeights(cons().Backends)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, v := range w {
		sum += v
	}
	if math.Abs(sum-1) > 1e-12 || math.Abs(w[0]-0.5) > 1e-12 {
		t.Fatalf("bad normalization %v", w)
	}
	if _, err := NormalizeStaticWeights(nil); err == nil {
		t.Error("empty must fail")
	}
}

func TestPublishCopiesAndIsImmutable(t *testing.T) {
	s := NewStore(RoundRobin)
	if s.Current().Version != 1 {
		t.Fatal("initial version should be 1")
	}
	ids := []string{"a", "b"}
	ws := []float64{0.7, 0.3}
	p, err := s.Publish(AdaptiveSWRR, ids, ws)
	if err != nil {
		t.Fatal(err)
	}
	ws[0], ids[0] = 99, "zzz" // caller mutation must not leak into the snapshot
	if p.Weights[0] != 0.7 || p.BackendIDs[0] != "a" {
		t.Fatal("published policy aliases caller slices")
	}
	if w, ok := p.WeightFor("a"); !ok || w != 0.7 {
		t.Fatal("WeightFor wrong")
	}
	if _, ok := p.WeightFor("missing"); ok {
		t.Fatal("missing id must not resolve")
	}
	p2, _ := s.Publish(RoundRobin, nil, nil)
	if p2.Version != p.Version+1 || s.Current() != p2 {
		t.Fatal("versions must increase and Current must track latest")
	}
	if p.Weights[0] != 0.7 {
		t.Fatal("old snapshot changed")
	}
}

func TestPublishRejectsInvalid(t *testing.T) {
	s := NewStore(RoundRobin)
	bad := [][]float64{{math.NaN(), 1}, {math.Inf(1), 1}, {-1, 1}, {1}}
	for _, w := range bad {
		if _, err := s.Publish(AdaptiveSWRR, []string{"a", "b"}, w); err == nil {
			t.Errorf("weights %v should be rejected", w)
		}
	}
	if _, err := s.Publish(AdaptiveSWRR, []string{"a", "a"}, []float64{1, 1}); err == nil {
		t.Error("duplicate ids should be rejected")
	}
	if _, err := s.Publish("Bogus", nil, nil); err == nil {
		t.Error("unknown strategy should be rejected")
	}
	if s.Current().Version != 1 {
		t.Error("failed publishes must not advance the version")
	}
}

func TestPublishWeightsIf(t *testing.T) {
	s := NewStore(RoundRobin)
	if _, ok, _ := s.PublishWeightsIf(AdaptiveSWRR, []string{"a"}, []float64{1}); ok {
		t.Fatal("must not publish while another strategy is active")
	}
	_, _ = s.Publish(AdaptiveSWRR, nil, nil)
	if _, ok, err := s.PublishWeightsIf(AdaptiveSWRR, []string{"a"}, []float64{1}); !ok || err != nil {
		t.Fatalf("should publish: ok=%v err=%v", ok, err)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	s := NewStore(AdaptiveSWRR)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_, _, _ = s.PublishWeightsIf(AdaptiveSWRR, []string{"a", "b"}, []float64{0.4, 0.6})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				p := s.Current()
				if len(p.Weights) == 2 {
					if p.Weights[0] != 0.4 || p.Weights[1] != 0.6 {
						t.Error("torn snapshot")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
