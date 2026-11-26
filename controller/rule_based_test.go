package controller

import (
	"adaptive-load/metrics"
	"math"
	"sort"
	"testing"
)

type bs struct {
	id      string
	healthy float64
	conns   float64
	p95     float64
	errs    float64
	cap     float64
}

func state(bks ...bs) *metrics.StateRepresentation {
	sort.Slice(bks, func(i, j int) bool { return bks[i].id < bks[j].id })
	st := &metrics.StateRepresentation{}
	for _, b := range bks {
		st.Backends = append(st.Backends, metrics.BackendStateFeatures{
			BackendID: b.id, Healthy: b.healthy, ActiveConnectionsNormalized: b.conns,
			P95LatencyNormalized: b.p95, ErrorRate: b.errs, CapacityRatio: b.cap,
			CPUIsMissing: true, MemoryIsMissing: true,
		})
		st.OrderedBackendIDs = append(st.OrderedBackendIDs, b.id)
	}
	return st
}

func even() *metrics.StateRepresentation {
	return state(bs{"a", 1, 0.1, 0.1, 0, 0.5}, bs{"b", 1, 0.1, 0.1, 0, 0.5})
}

func TestIdleBalancedStaysStatic(t *testing.T) {
	c := New(DefaultConfig())
	d := c.Update(even())
	if d.Updated {
		t.Fatalf("balanced idle state must not change policy: %+v", d)
	}
	if math.Abs(d.Weights[0]-0.5) > 1e-9 || math.Abs(d.Weights[1]-0.5) > 1e-9 {
		t.Fatalf("weights should stay at static capacity shares: %v", d.Weights)
	}
}

func TestShiftsWeightAwayFromLoadedBackend(t *testing.T) {
	c := New(DefaultConfig())
	loaded := state(bs{"a", 1, 0.9, 0.9, 0.1, 0.5}, bs{"b", 1, 0.1, 0.1, 0, 0.5})
	var d Decision
	for i := 0; i < 10; i++ {
		d = c.Update(loaded)
	}
	if d.Weights[0] >= d.Weights[1] {
		t.Fatalf("loaded backend a should have lower weight: %v", d.Weights)
	}
	for _, w := range d.Weights {
		if w < c.Config().MinWeight-1e-9 || w > c.Config().MaxWeight+1e-9 {
			t.Fatalf("weight %v out of bounds", w)
		}
	}
}

func TestStepLimitPerWindow(t *testing.T) {
	cfg := DefaultConfig()
	c := New(cfg)
	loaded := state(bs{"a", 1, 1, 1, 1, 0.5}, bs{"b", 1, 0, 0, 0, 0.5})
	prev := []float64{0.5, 0.5}
	for i := 0; i < 15; i++ {
		d := c.Update(loaded)
		for j := range d.Weights {
			if math.Abs(d.Weights[j]-prev[j]) > cfg.MaxStepPerWindow+1e-9 {
				t.Fatalf("window %d: step %v exceeds limit", i, d.Weights[j]-prev[j])
			}
		}
		prev = d.Weights
	}
}

func TestHysteresisSuppressesSmallChanges(t *testing.T) {
	c := New(DefaultConfig())
	slight := state(bs{"a", 1, 0.12, 0.1, 0, 0.5}, bs{"b", 1, 0.10, 0.1, 0, 0.5})
	d := c.Update(slight)
	if d.Updated || d.HoldReason == "" {
		t.Fatalf("tiny difference should be held: %+v", d)
	}
}

func TestUnhealthyBackendFrozenAndExcluded(t *testing.T) {
	c := New(DefaultConfig())
	down := state(bs{"a", 0, 0, 0, 0, 0.5}, bs{"b", 1, 0.8, 0.5, 0, 0.5})
	d := c.Update(down)
	// a unhealthy: keeps its static weight; b is the only healthy backend and is bounded.
	if d.Weights[0] != 0.5 {
		t.Fatalf("unhealthy weight must be frozen, got %v", d.Weights[0])
	}
	for i := 0; i < 20; i++ {
		d = c.Update(down)
	}
	if d.Weights[0] != 0.5 {
		t.Fatalf("unhealthy weight changed: %v", d.Weights[0])
	}
}

func TestRecoveryResetsSmoothing(t *testing.T) {
	c := New(DefaultConfig())
	for i := 0; i < 5; i++ {
		c.Update(state(bs{"a", 0, 0, 0, 0, 0.5}, bs{"b", 1, 0.2, 0.2, 0, 0.5}))
	}
	d := c.Update(even())
	if len(d.Weights) != 2 {
		t.Fatal("weights missing after recovery")
	}
	if got := c.ema["a"]; got == nil || !got.Initialized() {
		t.Fatal("recovered backend should have fresh EMA state")
	}
}

func TestHoldsWhenTelemetryStaleOrInvalid(t *testing.T) {
	c := New(DefaultConfig())
	st := state(bs{"a", 1, 1, 1, 1, 0.5}, bs{"b", 1, 0, 0, 0, 0.5})
	st.SafePolicyRetainRecommended = true
	st.StalenessReason = "snapshot too old"
	d := c.Update(st)
	if d.Updated || d.HoldReason == "" {
		t.Fatalf("must hold on stale state: %+v", d)
	}
	st.SafePolicyRetainRecommended, st.IsStale = false, true
	if d := c.Update(st); d.Updated {
		t.Fatal("must hold when IsStale")
	}
}

func TestEmptyAndNilState(t *testing.T) {
	c := New(DefaultConfig())
	if d := c.Update(nil); d.Updated {
		t.Fatal("nil state must hold")
	}
	if d := c.Update(&metrics.StateRepresentation{}); d.Updated {
		t.Fatal("empty state must hold")
	}
	if d := c.Update(state(bs{"a", 0, 0, 0, 0, 1})); d.Updated || d.HoldReason != "no healthy backends" {
		t.Fatalf("all-down must hold: %+v", d)
	}
}

func TestSingleHealthyBackendAllowed(t *testing.T) {
	c := New(DefaultConfig())
	d := Decision{}
	for i := 0; i < 20; i++ {
		d = c.Update(state(bs{"a", 1, 0.5, 0.5, 0, 0.5}, bs{"b", 0, 0, 0, 0, 0.5}))
	}
	if d.Weights[0] <= 0.5 {
		t.Fatalf("sole healthy backend should be allowed to grow: %v", d.Weights)
	}
}

func TestMissingCPUExcludedFromScore(t *testing.T) {
	c := New(DefaultConfig())
	b := metrics.BackendStateFeatures{CPUIsMissing: true, CPUUtilization: 0.5, MemoryIsMissing: true}
	if got := c.loadScore(b); got != 0 {
		t.Fatalf("missing CPU/mem must not count as load, got %v", got)
	}
	b.CPUIsMissing = false
	b.CPUUtilization = 1
	if got := c.loadScore(b); got <= 0 {
		t.Fatalf("present CPU should count, got %v", got)
	}
}

func TestBoundShares(t *testing.T) {
	out := boundShares([]float64{0.97, 0.02, 0.01}, []bool{true, true, true}, 0.05, 0.80)
	sum := 0.0
	for _, v := range out {
		sum += v
		if v < 0.05-1e-9 || v > 0.80+1e-9 {
			t.Fatalf("out of bounds %v", out)
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("sum should stay 1, got %v (%v)", sum, out)
	}
}

func TestCurrentWeightsReturnsCopies(t *testing.T) {
	c := New(DefaultConfig())
	c.Update(even())
	ids, ws := c.CurrentWeights()
	ws[0] = 42
	_, ws2 := c.CurrentWeights()
	if len(ids) != 2 || ws2[0] == 42 {
		t.Fatal("CurrentWeights must return copies")
	}
}
