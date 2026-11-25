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

// LogScaleNormalize scales a positive value x in [0, max] to [0.0, 1.0] using log scaling:
// log(1 + x) / log(1 + max). Useful for wide dynamic range signals like RPS or request counts.
func LogScaleNormalize(val, max float64) float64 {
	if val <= 0.0 {
		return 0.0
	}
	if max <= 0.0 {
		return 0.0
	}
	if val >= max {
		return 1.0
	}
	return math.Log(1.0+val) / math.Log(1.0+max)
}

// LatencyScaleNormalize scales a duration or millisecond latency against a documented reference scale,
// clipping the result strictly to [0.0, 1.0].
func LatencyScaleNormalize(latencyMs, scaleMs float64) float64 {
	if latencyMs <= 0.0 || scaleMs <= 0.0 {
		return 0.0
	}
	ratio := latencyMs / scaleMs
	if ratio >= 1.0 {
		return 1.0
	}
	return ratio
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
// or heuristic adaptive controllers incorporating runtime performance and capacity validation.
// Feature vector components:
// [0] Eligibility and Health (1.0 or 0.0)
// [1] Active Connections normalized (0 to connection limit or 100 max)
// [2] Normalized RPS (0 to 1000 req/s max)
// [3] Normalized Error Rate (0 to 100 err/s max)
// [4] Normalized Latency p95 in ms (0 to 500ms max)
// [5] Static Capacity Weight normalized (1 to 10 max)
// [6] Observed vs Benchmark Ratio (0.0 to 2.0 max, validates empirical vs assumed capacity)
// [7] CPU Cores normalized (1 to 16 cores max)
func BackendFeatureVector(b *BackendRuntimeMetrics, cap *BackendCapacityConfig) []float64 {
	vec := make([]float64, 8)

	if b.Healthy && b.Eligible {
		vec[0] = 1.0
	} else if b.Healthy {
		vec[0] = 0.5
	} else {
		vec[0] = 0.0
	}

	maxConns := 100.0
	if cap != nil && cap.ConnectionLimit > 0 {
		maxConns = float64(cap.ConnectionLimit)
	}
	vec[1] = MinMaxNormalize(float64(b.ActiveConns), 0, maxConns)
	vec[2] = MinMaxNormalize(b.RPS.Raw, 0, 1000)
	vec[3] = MinMaxNormalize(b.ErrorRate.Raw, 0, 100)
	vec[4] = MinMaxNormalize(b.Latency.P95Ms, 0, 500)

	weight := 1
	cpuCores := 1
	obsRatio := 0.0
	if cap != nil {
		if cap.StaticWeight > 0 {
			weight = cap.StaticWeight
		}
		if cap.CPUCores > 0 {
			cpuCores = cap.CPUCores
		}
		obsRatio = cap.ObservedVsBenchmarkRatio
	}
	vec[5] = MinMaxNormalize(float64(weight), 1, 10)
	vec[6] = MinMaxNormalize(obsRatio, 0.0, 2.0)
	vec[7] = MinMaxNormalize(float64(cpuCores), 1, 16)

	return vec
}

// WorkloadFeatureVector transforms incoming traffic characteristics into a normalized vector
// for controller strategy evaluation, strictly omitting sensitive client data and client IPs.
// Feature vector components:
// [0] Current RPS normalized (0 to 2000 req/s max)
// [1] RPS Trend / Derivative normalized (-1.0 to 1.0)
// [2] Average Latency in ms normalized (0 to 200ms max)
// [3] P95 Latency in ms normalized (0 to 500ms max)
// [4] Long-request fraction [0.0, 1.0]
// [5] Short-request fraction [0.0, 1.0]
// [6] Compute-heavy workload fraction [0.0, 1.0]
// [7] Average Request payload size normalized (0 to 64KB max)
// [8] Average Response payload size normalized (0 to 64KB max)
func WorkloadFeatureVector(g *GlobalTrafficMetrics) []float64 {
	vec := make([]float64, 9)

	vec[0] = MinMaxNormalize(g.CurrentRPS.Raw, 0, 2000)
	vec[1] = MinMaxNormalize(g.Workload.RPSDerivative, -1.0, 1.0)
	vec[2] = MinMaxNormalize(g.DurationStats.AvgMs, 0, 200)
	vec[3] = MinMaxNormalize(g.DurationStats.P95Ms, 0, 500)
	vec[4] = math.Max(0.0, math.Min(1.0, g.Workload.LongRequestFraction))

	shortFrac := 0.0
	computeFrac := 0.0
	if g.Workload.WorkloadClassFractions != nil {
		shortFrac = g.Workload.WorkloadClassFractions["short"]
		computeFrac = g.Workload.WorkloadClassFractions["compute-heavy"]
	}
	vec[5] = math.Max(0.0, math.Min(1.0, shortFrac))
	vec[6] = math.Max(0.0, math.Min(1.0, computeFrac))

	const maxPayloadBytes = 64 * 1024.0 // 64 KB
	vec[7] = MinMaxNormalize(g.Workload.PayloadSizes.AvgRequestSizeBytes, 0, maxPayloadBytes)
	vec[8] = MinMaxNormalize(g.Workload.PayloadSizes.AvgResponseSizeBytes, 0, maxPayloadBytes)

	return vec
}
