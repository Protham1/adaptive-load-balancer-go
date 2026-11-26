package strategy

import "math"

// WeightLookup resolves the dynamic weight of a backend from one immutable
// policy snapshot. *policy.Policy satisfies it.
type WeightLookup interface {
	WeightFor(id string) (float64, bool)
}

// adaptiveWeightScale converts float shares into the integer weights used by SWRR.
const adaptiveWeightScale = 1000.0

// AdaptiveWeightedRoundRobin is smooth weighted round-robin whose weights come
// from a dynamically published policy instead of the static configuration.
//
// The source function is called once per selection, so every selection sees one
// consistent immutable snapshot even while a controller publishes new ones.
// Backends missing from the snapshot (e.g. just added) fall back to their static
// capacity share. Health is NOT decided here: unhealthy peers are skipped by the
// existing IsAlive mechanism.
type AdaptiveWeightedRoundRobin[T AdaptivePeer] struct {
	core   *swrrCore
	source func() WeightLookup
}

// NewAdaptiveWeightedRoundRobin creates the strategy. source may be nil, in
// which case the strategy behaves like static SWRR.
func NewAdaptiveWeightedRoundRobin[T AdaptivePeer](source func() WeightLookup) *AdaptiveWeightedRoundRobin[T] {
	return &AdaptiveWeightedRoundRobin[T]{core: newSWRRCore(), source: source}
}

// Name returns the strategy identifier
func (a *AdaptiveWeightedRoundRobin[T]) Name() string {
	return "Adaptive-Weighted-Round-Robin"
}

// SelectNextBackend selects the next alive peer using the current dynamic weights.
func (a *AdaptiveWeightedRoundRobin[T]) SelectNextBackend(peers []T) T {
	var lookup WeightLookup
	if a.source != nil {
		lookup = a.source()
	}

	staticTotal := 0.0
	for _, p := range peers {
		if p.IsAlive() {
			staticTotal += float64(p.GetWeight())
		}
	}
	if staticTotal <= 0 {
		staticTotal = 1
	}

	return swrrSelect(a.core, peers, func(p T) int {
		share := float64(p.GetWeight()) / staticTotal
		if lookup != nil {
			if w, ok := lookup.WeightFor(p.GetName()); ok && !math.IsNaN(w) && !math.IsInf(w, 0) {
				share = w
			}
		}
		iw := int(math.Round(share * adaptiveWeightScale))
		if iw < 1 {
			iw = 1 // never fully starve a healthy backend
		}
		return iw
	})
}
