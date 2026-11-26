package metrics

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// MetricsCollector unifies all metric groups and produces consistent, validated snapshots.
type MetricsCollector struct {
	mu                  sync.RWMutex
	windowDuration      time.Duration
	minSamplesThreshold uint64
	hostCollector       *HostMetricsCollector
	probeTracker        *BackendProbeTracker
	globalTracker       *RequestTracker

	// Per-backend tracking
	backendTrackers map[string]*RequestTracker
	backendHealth   map[string]bool
	backendURLs     map[string]string
	capacityConfigs map[string]*BackendCapacityConfig

	// Controller tracking
	controllerMu     sync.RWMutex
	currentStrategy  string
	lastSwitchAt     time.Time
	lastDecisionTime time.Duration
	policyVersion    string
	switchCount      uint64

	// State scaler configuration (Phase 4)
	scalerMu     sync.RWMutex
	scalerConfig StateScalerConfig
}

// NewMetricsCollector initializes a unified collector with the specified aggregation window.
func NewMetricsCollector(windowDuration time.Duration) *MetricsCollector {
	if windowDuration <= 0 {
		windowDuration = 10 * time.Second
	}
	now := time.Now()

	return &MetricsCollector{
		windowDuration:      windowDuration,
		minSamplesThreshold: 10,
		hostCollector:       NewHostMetricsCollector(),
		probeTracker:        NewBackendProbeTracker(),
		globalTracker:       NewRequestTracker(windowDuration),
		backendTrackers:     make(map[string]*RequestTracker),
		backendHealth:       make(map[string]bool),
		backendURLs:         make(map[string]string),
		capacityConfigs:     make(map[string]*BackendCapacityConfig),
		currentStrategy:     "Round-Robin",
		lastSwitchAt:        now,
		policyVersion:       "1.0.0-baseline",
		scalerConfig:        DefaultStateScalerConfig(),
	}
}

// RegisterBackend adds a backend to tracking with its capacity specifications and default benchmark expectations.
func (c *MetricsCollector) RegisterBackend(id, url string, weight, cpuCores int, memLimit uint64, connLimit int64) {
	class := "standard"
	if cpuCores >= 8 || weight >= 3 {
		class = "large"
	} else if cpuCores >= 4 || weight >= 2 {
		class = "medium"
	} else {
		class = "small"
	}
	benchmarkRPS := float64(weight) * 10000.0
	benchmarkP95Ms := 2.5
	c.RegisterBackendWithCapacity(id, url, weight, cpuCores, memLimit, connLimit, class, benchmarkRPS, benchmarkP95Ms)
}

// RegisterBackendWithCapacity adds a backend with declared capacity class and empirical benchmark validations.
func (c *MetricsCollector) RegisterBackendWithCapacity(id, url string, weight, cpuCores int, memLimit uint64, connLimit int64, class string, benchmarkRPS, benchmarkP95Ms float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.backendTrackers[id]; !exists {
		c.backendTrackers[id] = NewRequestTracker(c.windowDuration)
	}
	c.backendHealth[id] = true
	c.backendURLs[id] = url
	c.capacityConfigs[id] = &BackendCapacityConfig{
		BackendID:                id,
		URL:                      url,
		StaticWeight:             weight,
		DeclaredCapacityClass:    class,
		CPUCores:                 cpuCores,
		MemoryLimitBytes:         memLimit,
		ConnectionLimit:          connLimit,
		IsEligible:               true,
		BenchmarkRPS:             benchmarkRPS,
		BenchmarkP95Ms:           benchmarkP95Ms,
		ObservedVsBenchmarkRatio: 0.0,
		HardwareVsEmpiricalScore: 1.0,
	}
}

// UnregisterBackend removes a backend from tracking.
func (c *MetricsCollector) UnregisterBackend(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.backendTrackers, id)
	delete(c.backendHealth, id)
	delete(c.backendURLs, id)
	delete(c.capacityConfigs, id)
}

// SetBackendHealth updates the liveness state of a backend.
func (c *MetricsCollector) SetBackendHealth(id string, healthy bool) {
	c.mu.Lock()
	c.backendHealth[id] = healthy
	c.mu.Unlock()
}

// RecordProbe delegates active probe results to the internal probe tracker.
func (c *MetricsCollector) RecordProbe(id string, healthy bool, duration time.Duration, err error) {
	c.SetBackendHealth(id, healthy)
	c.probeTracker.RecordProbe(id, healthy, duration, err)
}

// RecordRequestStart begins tracking a request globally and for the specific backend.
func (c *MetricsCollector) RecordRequestStart(backendID, method, route string, isComputeHeavy ...bool) *ActiveRequest {
	c.globalTracker.StartRequest(method, route, isComputeHeavy...)

	c.mu.RLock()
	tracker, exists := c.backendTrackers[backendID]
	c.mu.RUnlock()

	if exists && tracker != nil {
		return tracker.StartRequest(method, route, isComputeHeavy...)
	}
	return &ActiveRequest{StartTime: time.Now(), Method: CanonicalizeMethod(method), Route: CanonicalizeRoute(route)}
}

// RecordRequestEnd completes tracking a request, updating error classifications and latencies.
func (c *MetricsCollector) RecordRequestEnd(backendID string, req *ActiveRequest, statusCode int, err error, bytesIn, bytesOut uint64, isComputeHeavy ...bool) time.Duration {
	c.globalTracker.EndRequest(req, statusCode, err, bytesIn, bytesOut, isComputeHeavy...)

	c.mu.RLock()
	tracker, exists := c.backendTrackers[backendID]
	c.mu.RUnlock()

	if exists && tracker != nil {
		return tracker.EndRequest(req, statusCode, err, bytesIn, bytesOut, isComputeHeavy...)
	}

	if req != nil && !req.StartTime.IsZero() {
		return time.Since(req.StartTime)
	}
	return 0
}

// SetMinSamplesThreshold sets the minimum completed request count required for valid observation windows.
func (c *MetricsCollector) SetMinSamplesThreshold(threshold uint64) {
	atomic.StoreUint64(&c.minSamplesThreshold, threshold)
}

// GetMinSamplesThreshold returns the minimum sample count threshold for window validity.
func (c *MetricsCollector) GetMinSamplesThreshold() uint64 {
	return atomic.LoadUint64(&c.minSamplesThreshold)
}

// MarkProxyStart delegates proxy start timing to separate LB overhead from backend latency.
func (c *MetricsCollector) MarkProxyStart(req *ActiveRequest) {
	if req != nil {
		req.ProxyStartTime = time.Now()
	}
}

// RecordStrategySwitch tracks adaptive controller strategy modifications.
func (c *MetricsCollector) RecordStrategySwitch(newStrategy string, decisionDuration time.Duration) {
	c.controllerMu.Lock()
	defer c.controllerMu.Unlock()

	c.currentStrategy = newStrategy
	c.lastSwitchAt = time.Now()
	c.lastDecisionTime = decisionDuration
	atomic.AddUint64(&c.switchCount, 1)
}

// SetPolicyVersion records the identifier of the active policy snapshot.
func (c *MetricsCollector) SetPolicyVersion(v string) {
	c.controllerMu.Lock()
	c.policyVersion = v
	c.controllerMu.Unlock()
}

// CollectSnapshot returns an immutable point-in-time snapshot of all metric groups.
func (c *MetricsCollector) CollectSnapshot() *MetricsSnapshot {
	now := time.Now()
	threshold := c.GetMinSamplesThreshold()

	// 1. Host Resources
	host := c.hostCollector.Sample()

	// 2. Global Traffic
	c.globalTracker.UpdateSmoothedValues()
	inBytes, outBytes := c.globalTracker.GetBytes()
	globalWindow := c.globalTracker.GetWindowMetadata(threshold)
	globalDurationStats := c.globalTracker.GetLatencyStats()
	globalBackendStats := c.globalTracker.GetBackendLatencyStats()
	globalOverheadStats := c.globalTracker.GetOverheadLatencyStats()

	globalTraffic := GlobalTrafficMetrics{
		Timestamp:             now,
		TotalRequests:         c.globalTracker.GetTotalRequests(),
		TotalErrors:           c.globalTracker.GetTotalErrors(),
		CurrentRPS:            c.globalTracker.GetSmoothedRPS(),
		ErrorRate:             c.globalTracker.GetSmoothedErrorRate(),
		ActiveRequests:        c.globalTracker.GetActiveConnections(),
		BytesReceived:         inBytes,
		BytesSent:             outBytes,
		DurationStats:         globalDurationStats,
		BackendDurationStats:  globalBackendStats,
		OverheadDurationStats: globalOverheadStats,
		P50Latency:            c.globalTracker.GetSmoothedP50(),
		P95Latency:            c.globalTracker.GetSmoothedP95(),
		P99Latency:            c.globalTracker.GetSmoothedP99(),
		Window:                globalWindow,
		MethodMix:             c.globalTracker.GetMethodMix(),
		RouteMix:              c.globalTracker.GetRouteMix(),
		Workload:              c.globalTracker.GetWorkloadFeatures(),
		WindowSeconds:         c.windowDuration.Seconds(),
	}

	// 3. Per-Backend Runtime Metrics & Capacity
	c.mu.RLock()
	backends := make(map[string]*BackendRuntimeMetrics, len(c.backendTrackers))
	capacity := make(map[string]*BackendCapacityConfig, len(c.capacityConfigs))

	for id, cfg := range c.capacityConfigs {
		cp := *cfg
		capacity[id] = &cp
	}

	for id, tracker := range c.backendTrackers {
		u := c.backendURLs[id]
		healthy := c.backendHealth[id]

		tracker.UpdateSmoothedValues()
		bWindow := tracker.GetWindowMetadata(threshold)
		bLatency := tracker.GetLatencyStats()
		bBackendLatency := tracker.GetBackendLatencyStats()
		bOverhead := tracker.GetOverheadLatencyStats()
		activeConns := tracker.GetActiveConnections()
		currentRPS := tracker.GetRPS()

		// Evaluate eligibility: healthy AND connection limit not breached
		eligible := healthy
		capCfg := capacity[id]
		if capCfg != nil {
			if capCfg.ConnectionLimit > 0 && activeConns >= capCfg.ConnectionLimit {
				eligible = false
			}
			capCfg.IsEligible = eligible

			// Benchmark validation: ratio of runtime RPS to benchmarked RPS
			if capCfg.BenchmarkRPS > 0 {
				capCfg.ObservedVsBenchmarkRatio = currentRPS / capCfg.BenchmarkRPS
			}

			// Hardware vs Empirical score
			if capCfg.BenchmarkRPS > 0 && capCfg.StaticWeight > 0 {
				expectedRPSPerWeight := capCfg.BenchmarkRPS / float64(capCfg.StaticWeight)
				if expectedRPSPerWeight > 0 {
					actualRPSPerWeight := currentRPS / float64(capCfg.StaticWeight)
					score := actualRPSPerWeight / expectedRPSPerWeight
					if math.IsNaN(score) || math.IsInf(score, 0) {
						score = 1.0
					}
					capCfg.HardwareVsEmpiricalScore = score
				}
			}
		}

		backends[id] = &BackendRuntimeMetrics{
			BackendID:        id,
			URL:              u,
			Healthy:          healthy,
			Eligible:         eligible,
			ActiveConns:      activeConns,
			TotalRequests:    tracker.GetTotalRequests(),
			TotalErrors:      tracker.GetTotalErrors(),
			ErrorsByCategory: tracker.GetErrorBreakdown(),
			Window:           bWindow,
			RPS:              tracker.GetSmoothedRPS(),
			ErrorRate:        tracker.GetSmoothedErrorRate(),
			Latency:          bLatency,
			BackendLatency:   bBackendLatency,
			Overhead:         bOverhead,
			LatencyP50:       tracker.GetSmoothedP50(),
			LatencyP95:       tracker.GetSmoothedP95(),
			LatencyP99:       tracker.GetSmoothedP99(),
			Capacity:         capCfg,
			WindowSeconds:    c.windowDuration.Seconds(),
			LastUpdated:      now,
		}
	}
	c.mu.RUnlock()

	// 4. Controller Metrics
	c.controllerMu.RLock()
	stratAge := now.Sub(c.lastSwitchAt)
	controller := ControllerMetrics{
		CurrentStrategy:  c.currentStrategy,
		StrategyAge:      stratAge,
		StrategyAgeSec:   stratAge.Seconds(),
		LastDecisionTime: c.lastDecisionTime,
		LastDecisionUs:   float64(c.lastDecisionTime.Microseconds()),
		PolicyVersion:    c.policyVersion,
		SwitchCount:      atomic.LoadUint64(&c.switchCount),
		LastSwitchAt:     c.lastSwitchAt,
	}
	c.controllerMu.RUnlock()

	return &MetricsSnapshot{
		Timestamp:           now,
		WindowSeconds:       c.windowDuration.Seconds(),
		Window:              globalWindow,
		IsValid:             globalWindow.IsValid,
		FallbackRecommended: globalWindow.FallbackRecommended,
		ReliabilityReason:   globalWindow.ReliabilityReason,
		Host:                host,
		GlobalTraffic:       globalTraffic,
		Backends:            backends,
		CapacityConfigs:     capacity,
		Controller:          controller,
	}
}

// SetStateScalerConfig configures the normalization constants for state vector generation.
func (c *MetricsCollector) SetStateScalerConfig(cfg StateScalerConfig) {
	c.scalerMu.Lock()
	defer c.scalerMu.Unlock()
	c.scalerConfig = cfg
}

// GetStateScalerConfig returns the active normalization configuration.
func (c *MetricsCollector) GetStateScalerConfig() StateScalerConfig {
	c.scalerMu.RLock()
	defer c.scalerMu.RUnlock()
	return c.scalerConfig
}

// GetStateRepresentation returns a fixed, versioned feature vector with deterministic backend ordering.
func (c *MetricsCollector) GetStateRepresentation(maxBackends ...int) *StateRepresentation {
	snap := c.CollectSnapshot()
	cfg := c.GetStateScalerConfig()
	return BuildStateRepresentation(snap, cfg, maxBackends...)
}
