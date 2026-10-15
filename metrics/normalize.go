package metrics

import (
	"math"
	"time"
)

// MinMaxNormalize scales a raw value to the range [0.0, 1.0] based on known bounds.
// Clamps values outside [min, max] and safely handles min >= max.
func MinMaxNormalize(val, min, max float64) float64 {
	if max <= min {
		return 0.0
	}
	if val <= min {
		return 0.0
	}
	if val >= max {
		return 1.0
	}
	return (val - min) / (max - min)
}

// ZScoreNormalize scales a value using mean and standard deviation.
// Safely returns 0.0 when standard deviation is zero.
func ZScoreNormalize(val, mean, stdDev float64) float64 {
	if stdDev <= 0.0 || math.IsNaN(stdDev) {
		return 0.0
	}
	return (val - mean) / stdDev
}

// RatioToPercent converts a fraction [0.0, 1.0] to percentage [0.0, 100.0].
func RatioToPercent(ratio float64) float64 {
	return ratio * 100.0
}

// PercentToRatio converts percentage [0.0, 100.0] to a fraction [0.0, 1.0].
func PercentToRatio(pct float64) float64 {
	return pct / 100.0
}

// DurationToSeconds converts time.Duration to fractional seconds.
func DurationToSeconds(d time.Duration) float64 {
	return d.Seconds()
}

// DurationToMilliseconds converts time.Duration to fractional milliseconds.
func DurationToMilliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// BackendFeatureVector produces a normalized numerical slice for machine learning
// or heuristic adaptive controllers.
// Feature vector components:
// [0] Healthy (1.0 or 0.0)
// [1] Active Connections normalized (0 to 100 max)
// [2] Normalized RPS (0 to 1000 req/s max)
// [3] Normalized Error Rate (0 to 100 err/s max)
// [4] Normalized Latency p95 in ms (0 to 500ms max)
// [5] Static Capacity Weight normalized (1 to 10 max)
func BackendFeatureVector(b *BackendRuntimeMetrics, cap *BackendCapacityConfig) []float64 {
	vec := make([]float64, 6)

	if b.Healthy {
		vec[0] = 1.0
	} else {
		vec[0] = 0.0
	}

	vec[1] = MinMaxNormalize(float64(b.ActiveConns), 0, 100)
	vec[2] = MinMaxNormalize(b.RPS, 0, 1000)
	vec[3] = MinMaxNormalize(b.ErrorRate, 0, 100)
	vec[4] = MinMaxNormalize(b.Latency.P95Ms, 0, 500)

	weight := 1
	if cap != nil && cap.StaticWeight > 0 {
		weight = cap.StaticWeight
	}
	vec[5] = MinMaxNormalize(float64(weight), 1, 10)

	return vec
}
