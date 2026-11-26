// Package controller implements the transparent, non-AI adaptive baseline
// (Phase 5). It turns State v1 snapshots into bounded dynamic SWRR weights.
//
// Pipeline per decision window:
//
//	state -> load score per backend -> capacity-weighted target share
//	      -> EMA smoothing -> per-window step limit -> hysteresis deadband
//
// It is NOT the proposed AI: it is the comparison point for testing whether a
// learned policy beats a reasonable heuristic. Backend health is decided by the
// existing health mechanism; the controller only reads it.
package controller

import (
	"adaptive-load/metrics"
	"math"
	"sync"
	"time"
)

// ScoreWeights are the relative importance of each load signal. They are
// normalized over the signals that are actually available (a missing CPU
// reading is excluded, never treated as zero).
type ScoreWeights struct {
	CPU         float64 `json:"cpu"`
	Memory      float64 `json:"memory"`
	Connections float64 `json:"connections"`
	Latency     float64 `json:"latency"`
	Errors      float64 `json:"errors"`
}

// Config holds every tunable of the baseline. All values are documented in
// DOCUMENTATION.md; DefaultConfig is the frozen baseline setting.
type Config struct {
	Scores           ScoreWeights `json:"scores"`
	EMAAlpha         float64      `json:"ema_alpha"`           // smoothing of target shares
	MaxStepPerWindow float64      `json:"max_step_per_window"` // max |weight change| per decision
	MinWeight        float64      `json:"min_weight"`          // lower bound of a healthy backend weight
	MaxWeight        float64      `json:"max_weight"`          // upper bound of a healthy backend weight
	HeadroomFloor    float64      `json:"headroom_floor"`      // a saturated backend keeps this headroom
	Deadband         float64      `json:"deadband"`            // hysteresis: ignore max change below this
}

// DefaultConfig returns the baseline configuration.
func DefaultConfig() Config {
	return Config{
		Scores:           ScoreWeights{CPU: 0.10, Memory: 0.10, Connections: 0.30, Latency: 0.30, Errors: 0.20},
		EMAAlpha:         0.3,
		MaxStepPerWindow: 0.10,
		MinWeight:        0.05,
		MaxWeight:        0.80,
		HeadroomFloor:    0.05,
		Deadband:         0.02,
	}
}

func (c Config) sanitized() Config {
	d := DefaultConfig()
	if c.Scores == (ScoreWeights{}) {
		c.Scores = d.Scores
	}
	if c.EMAAlpha <= 0 || c.EMAAlpha > 1 {
		c.EMAAlpha = d.EMAAlpha
	}
	if c.MaxStepPerWindow <= 0 {
		c.MaxStepPerWindow = d.MaxStepPerWindow
	}
	if c.MinWeight <= 0 {
		c.MinWeight = d.MinWeight
	}
	if c.MaxWeight <= c.MinWeight || c.MaxWeight > 1 {
		c.MaxWeight = d.MaxWeight
	}
	if c.HeadroomFloor <= 0 {
		c.HeadroomFloor = d.HeadroomFloor
	}
	if c.Deadband < 0 {
		c.Deadband = d.Deadband
	}
	return c
}

// BackendDecision explains the controller's view of one backend.
type BackendDecision struct {
	BackendID string  `json:"backend_id"`
	Healthy   bool    `json:"healthy"`
	LoadScore float64 `json:"load_score"` // [0,1], higher = more loaded
	Target    float64 `json:"target"`     // raw capacity-weighted target share
	Smoothed  float64 `json:"smoothed"`   // EMA of target
	Weight    float64 `json:"weight"`     // weight in force after this decision
}

// Decision is the outcome of one controller update.
type Decision struct {
	Timestamp  time.Time         `json:"timestamp"`
	Updated    bool              `json:"updated"`     // new weights should be published
	HoldReason string            `json:"hold_reason"` // why weights were retained
	MaxDelta   float64           `json:"max_delta"`   // largest proposed weight change
	BackendIDs []string          `json:"backend_ids"`
	Weights    []float64         `json:"weights"` // aligned with BackendIDs
	Backends   []BackendDecision `json:"backends"`
	DurationUs float64           `json:"duration_us"`
	Windows    uint64            `json:"windows"` // number of updates processed
}

// Controller is the rule-based baseline. Safe for concurrent use.
type Controller struct {
	mu      sync.Mutex
	cfg     Config
	ema     map[string]*metrics.SingleEMA
	wasUp   map[string]bool
	current map[string]float64 // weights in force (last published)
	last    Decision
	windows uint64
}

// New creates a controller.
func New(cfg Config) *Controller {
	return &Controller{
		cfg:     cfg.sanitized(),
		ema:     make(map[string]*metrics.SingleEMA),
		wasUp:   make(map[string]bool),
		current: make(map[string]float64),
	}
}

// Config returns the effective configuration.
func (c *Controller) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Last returns the most recent decision (zero value before the first update).
func (c *Controller) Last() Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyDecision(c.last)
}

// CurrentWeights returns the weights in force, keyed by BackendID order (sorted
// IDs). It returns nil slices before the first update.
func (c *Controller) CurrentWeights() ([]string, []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() ([]string, []float64) {
	if len(c.last.BackendIDs) == 0 {
		return nil, nil
	}
	ids := append([]string(nil), c.last.BackendIDs...)
	ws := make([]float64, len(ids))
	for i, id := range ids {
		ws[i] = c.current[id]
	}
	return ids, ws
}

func copyDecision(d Decision) Decision {
	d.BackendIDs = append([]string(nil), d.BackendIDs...)
	d.Weights = append([]float64(nil), d.Weights...)
	d.Backends = append([]BackendDecision(nil), d.Backends...)
	return d
}

// Update processes one State v1 snapshot. It never publishes anything itself:
// the caller publishes Decision.Weights when Decision.Updated is true.
func (c *Controller) Update(st *metrics.StateRepresentation) Decision {
	start := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows++

	d := Decision{Timestamp: start, Windows: c.windows}

	if st == nil || len(st.Backends) == 0 {
		d.HoldReason = "no state or no backends"
		return c.finishHoldLocked(d, start)
	}
	if st.SafePolicyRetainRecommended || st.IsStale {
		d.HoldReason = "safe-policy retain: " + st.StalenessReason
		if st.StalenessReason == "" {
			d.HoldReason = "safe-policy retain recommended (telemetry stale or window too small)"
		}
		return c.finishHoldLocked(d, start)
	}

	n := len(st.Backends)
	ids := make([]string, n)
	healthy := make([]bool, n)
	loads := make([]float64, n)
	targets := make([]float64, n)
	smoothed := make([]float64, n)

	// 1. Load scores and capacity-weighted targets (healthy backends only).
	headroomRaw := make([]float64, n)
	rawSum := 0.0
	nHealthy := 0
	for i, b := range st.Backends {
		ids[i] = b.BackendID
		healthy[i] = b.Healthy > 0
		loads[i] = c.loadScore(b)
		if !healthy[i] {
			continue
		}
		nHealthy++
		headroom := math.Max(1.0-loads[i], c.cfg.HeadroomFloor)
		capRatio := b.CapacityRatio
		if capRatio <= 0 {
			capRatio = 1.0 / float64(n)
		}
		headroomRaw[i] = capRatio * headroom
		rawSum += headroomRaw[i]
	}
	if nHealthy == 0 || rawSum <= 0 {
		d.HoldReason = "no healthy backends"
		d.BackendIDs = ids
		return c.finishHoldLocked(d, start)
	}
	for i := range targets {
		if healthy[i] {
			targets[i] = headroomRaw[i] / rawSum
		}
	}
	bounded := boundShares(targets, healthy, c.cfg.MinWeight, c.cfg.MaxWeight)

	// 2. EMA smoothing. A backend returning from an outage restarts its EMA.
	for i, id := range ids {
		if !healthy[i] {
			c.wasUp[id] = false
			continue
		}
		e := c.ema[id]
		if e == nil || !c.wasUp[id] {
			e = metrics.NewSingleEMA(c.cfg.EMAAlpha)
			c.ema[id] = e
		}
		c.wasUp[id] = true
		smoothed[i] = e.Update(bounded[i])
	}

	// 3. Seed unseen backends from their static capacity share, then apply the
	// per-window step limit. Unhealthy backends keep their last weight.
	proposed := make([]float64, n)
	maxDelta := 0.0
	for i, b := range st.Backends {
		id := ids[i]
		cur, seen := c.current[id]
		if !seen {
			cur = math.Min(c.cfg.MaxWeight, math.Max(c.cfg.MinWeight, b.CapacityRatio))
			c.current[id] = cur
		}
		if !healthy[i] {
			proposed[i] = cur
			continue
		}
		step := clamp(smoothed[i]-cur, -c.cfg.MaxStepPerWindow, c.cfg.MaxStepPerWindow)
		proposed[i] = clamp(cur+step, c.cfg.MinWeight, c.cfg.MaxWeight)
		if delta := math.Abs(proposed[i] - cur); delta > maxDelta {
			maxDelta = delta
		}
	}
	d.MaxDelta = maxDelta
	d.BackendIDs = ids

	// 4. Hysteresis: small differences do not change the published policy.
	// The in-force weights only advance when we actually publish.
	if maxDelta < c.cfg.Deadband {
		d.HoldReason = "hysteresis: max change below deadband"
		return c.finishHoldLocked(d, start)
	}

	for i, id := range ids {
		c.current[id] = proposed[i]
	}
	d.Updated = true
	d.Weights = proposed
	d.Backends = c.backendDecisions(ids, healthy, loads, bounded, smoothed)
	d.DurationUs = float64(time.Since(start).Nanoseconds()) / 1000.0
	c.last = d
	return copyDecision(d)
}

// finishHoldLocked records a hold decision carrying the weights currently in force.
func (c *Controller) finishHoldLocked(d Decision, start time.Time) Decision {
	if d.BackendIDs == nil {
		d.BackendIDs = append([]string(nil), c.last.BackendIDs...)
	}
	d.Weights = make([]float64, len(d.BackendIDs))
	for i, id := range d.BackendIDs {
		d.Weights[i] = c.current[id]
	}
	d.Backends = append([]BackendDecision(nil), c.last.Backends...)
	d.DurationUs = float64(time.Since(start).Nanoseconds()) / 1000.0
	c.last = d
	return copyDecision(d)
}

func (c *Controller) backendDecisions(ids []string, healthy []bool, loads, targets, smoothed []float64) []BackendDecision {
	out := make([]BackendDecision, len(ids))
	for i, id := range ids {
		out[i] = BackendDecision{
			BackendID: id, Healthy: healthy[i], LoadScore: loads[i],
			Target: targets[i], Smoothed: smoothed[i], Weight: c.current[id],
		}
	}
	return out
}

// loadScore combines the available signals into [0,1]. Missing signals are
// excluded and the remaining weights renormalized.
func (c *Controller) loadScore(b metrics.BackendStateFeatures) float64 {
	sw := c.cfg.Scores
	num, den := 0.0, 0.0
	add := func(w, v float64) {
		if w > 0 {
			num += w * clamp(v, 0, 1)
			den += w
		}
	}
	if !b.CPUIsMissing {
		add(sw.CPU, b.CPUUtilization)
	}
	if !b.MemoryIsMissing {
		add(sw.Memory, b.MemoryUtilization)
	}
	add(sw.Connections, b.ActiveConnectionsNormalized)
	add(sw.Latency, b.P95LatencyNormalized)
	add(sw.Errors, b.ErrorRate)
	if den == 0 {
		return 0
	}
	return num / den
}

// boundShares scales healthy shares so that, after clamping each into [lo, hi],
// they still sum to 1. It bisects on a scale factor lambda: sum(clamp(s*lambda))
// is monotone in lambda, and a solution exists whenever n*lo <= 1 <= n*hi. When
// the bounds are infeasible for the number of healthy backends they are relaxed
// (e.g. a single healthy backend legitimately gets the whole share).
func boundShares(shares []float64, healthy []bool, lo, hi float64) []float64 {
	n := 0
	for _, h := range healthy {
		if h {
			n++
		}
	}
	out := make([]float64, len(shares))
	if n == 0 {
		return out
	}
	if lo > 1.0/float64(n) {
		lo = 1.0 / float64(n)
	}
	if hi < 1.0/float64(n) {
		hi = 1.0 / float64(n)
	}
	sumAt := func(lambda float64) float64 {
		t := 0.0
		for i, h := range healthy {
			if h {
				t += clamp(shares[i]*lambda, lo, hi)
			}
		}
		return t
	}
	loL, hiL := 0.0, 1.0
	for sumAt(hiL) < 1.0 && hiL < 1e12 {
		hiL *= 2
	}
	for i := 0; i < 80; i++ {
		mid := (loL + hiL) / 2
		if sumAt(mid) < 1.0 {
			loL = mid
		} else {
			hiL = mid
		}
	}
	for i, h := range healthy {
		if h {
			out[i] = clamp(shares[i]*hiL, lo, hi)
		}
	}
	return out
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
