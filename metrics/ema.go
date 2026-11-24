package metrics

import (
	"sync"
)

// DefaultAlphas specifies the standard EMA smoothing coefficients:
// 0.1 (slow/smooth), 0.3 (moderate), 0.5 (responsive).
var DefaultAlphas = []float64{0.1, 0.3, 0.5}

// SmoothedValue preserves both the raw observation and exponential moving averages
// across different alpha coefficients, enabling direct comparison for experiments and controllers.
type SmoothedValue struct {
	Raw   float64 `json:"raw"`
	EMA01 float64 `json:"ema_01"` // alpha = 0.1
	EMA03 float64 `json:"ema_03"` // alpha = 0.3
	EMA05 float64 `json:"ema_05"` // alpha = 0.5
}

// SingleEMA implements a single-variable exponential moving average:
// EMA(t) = alpha * x(t) + (1 - alpha) * EMA(t-1)
type SingleEMA struct {
	alpha       float64
	value       float64
	initialized bool
}

// NewSingleEMA constructs an EMA tracker for a specific alpha coefficient.
func NewSingleEMA(alpha float64) *SingleEMA {
	if alpha <= 0.0 {
		alpha = 0.1
	}
	if alpha > 1.0 {
		alpha = 1.0
	}
	return &SingleEMA{alpha: alpha}
}

// Update incorporates a new observation x into the moving average.
func (e *SingleEMA) Update(x float64) float64 {
	if !e.initialized {
		e.value = x
		e.initialized = true
		return e.value
	}
	e.value = e.alpha*x + (1.0-e.alpha)*e.value
	return e.value
}

// Value returns the current smoothed value.
func (e *SingleEMA) Value() float64 {
	return e.value
}

// Initialized reports whether at least one observation has been recorded.
func (e *SingleEMA) Initialized() bool {
	return e.initialized
}

// Reset clears the state of the EMA.
func (e *SingleEMA) Reset() {
	e.value = 0
	e.initialized = false
}

// MultiEMA maintains multiple SingleEMA trackers (alpha = 0.1, 0.3, 0.5)
// along with the latest raw value in a thread-safe container.
type MultiEMA struct {
	mu    sync.RWMutex
	raw   float64
	ema01 *SingleEMA
	ema03 *SingleEMA
	ema05 *SingleEMA
}

// NewMultiEMA creates a thread-safe multi-coefficient EMA tracker.
func NewMultiEMA() *MultiEMA {
	return &MultiEMA{
		ema01: NewSingleEMA(0.1),
		ema03: NewSingleEMA(0.3),
		ema05: NewSingleEMA(0.5),
	}
}

// Update records a new observation and updates all EMA values.
func (m *MultiEMA) Update(raw float64) SmoothedValue {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.raw = raw
	v01 := m.ema01.Update(raw)
	v03 := m.ema03.Update(raw)
	v05 := m.ema05.Update(raw)

	return SmoothedValue{
		Raw:   raw,
		EMA01: v01,
		EMA03: v03,
		EMA05: v05,
	}
}

// Value returns the current raw and smoothed values.
func (m *MultiEMA) Value() SmoothedValue {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return SmoothedValue{
		Raw:   m.raw,
		EMA01: m.ema01.Value(),
		EMA03: m.ema03.Value(),
		EMA05: m.ema05.Value(),
	}
}

// Reset clears all moving averages.
func (m *MultiEMA) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.raw = 0
	m.ema01.Reset()
	m.ema03.Reset()
	m.ema05.Reset()
}
