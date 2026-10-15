package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// MetricsCollector unifies all metric groups and produces consistent, validated snapshots.
type MetricsCollector struct {
	mu              sync.RWMutex
	windowDuration  time.Duration
	hostCollector   *HostMetricsCollector
	probeTracker    *BackendProbeTracker
	globalTracker   *RequestTracker

	// Per-backend tracking
	backendTrackers map[string]*RequestTracker
	backendHealth   map[string]bool
	backendURLs     map[string]string
	capacityConfigs map[string]*BackendCapacityConfig

	// Controller tracking
	controllerMu      sync.RWMutex
	currentStrategy   string
	lastSwitchAt      time.Time
	lastDecisionTime  time.Duration
	policyVersion     string
	switchCount       uint64
}

// NewMetricsCollector initializes a unified collector with the specified aggregation window.
func NewMetricsCollector(windowDuration time.Duration) *MetricsCollector {
	if windowDuration <= 0 {
		windowDuration = 10 * time.Second
	}
	now := time.Now()

	return &MetricsCollector{
		windowDuration:  windowDuration,
		hostCollector:   NewHostMetricsCollector(),
		probeTracker:    NewBackendProbeTracker(),
		globalTracker:   NewRequestTracker(windowDuration),
		backendTrackers: make(map[string]*RequestTracker),
		backendHealth:   make(map[string]bool),
		backendURLs:     make(map[string]string),
		capacityConfigs: make(map[string]*BackendCapacityConfig),
		currentStrategy: "Round-Robin",
		lastSwitchAt:    now,
		policyVersion:   "1.0.0-baseline",
	}
}

// RegisterBackend adds a backend to tracking with its capacity specifications.
func (c *MetricsCollector) RegisterBackend(id, url string, weight, cpuCores int, memLimit uint64, connLimit int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.backendTrackers[id]; !exists {
		c.backendTrackers[id] = NewRequestTracker(c.windowDuration)
	}
	c.backendHealth[id] = true
	c.backendURLs[id] = url
	c.capacityConfigs[id] = &BackendCapacityConfig{
		BackendID:        id,
		URL:              url,
		StaticWeight:     weight,
		CPUCores:         cpuCores,
		MemoryLimitBytes: memLimit,
		ConnectionLimit:  connLimit,
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
func (c *MetricsCollector) RecordRequestStart(backendID, method, route string) *ActiveRequest {
	c.globalTracker.StartRequest(method, route)

	c.mu.RLock()
	tracker, exists := c.backendTrackers[backendID]
	c.mu.RUnlock()

	if exists && tracker != nil {
		return tracker.StartRequest(method, route)
	}
	return &ActiveRequest{StartTime: time.Now(), Method: method, Route: route}
}

// RecordRequestEnd completes tracking a request, updating error classifications and latencies.
func (c *MetricsCollector) RecordRequestEnd(backendID string, req *ActiveRequest, statusCode int, err error, bytesIn, bytesOut uint64) time.Duration {
	c.globalTracker.EndRequest(req, statusCode, err, bytesIn, bytesOut)

	c.mu.RLock()
	tracker, exists := c.backendTrackers[backendID]
	c.mu.RUnlock()

	if exists && tracker != nil {
		return tracker.EndRequest(req, statusCode, err, bytesIn, bytesOut)
	}

	if req != nil && !req.StartTime.IsZero() {
		return time.Since(req.StartTime)
	}
	return 0
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

// CollectSnapshot returns an immutable point-in-time snapshot of all metric groups.
func (c *MetricsCollector) CollectSnapshot() *MetricsSnapshot {
	now := time.Now()

	// 1. Host Resources
	host := c.hostCollector.Sample()

	// 2. Global Traffic
	inBytes, outBytes := c.globalTracker.GetBytes()
	globalTraffic := GlobalTrafficMetrics{
		Timestamp:      now,
		TotalRequests:  c.globalTracker.GetTotalRequests(),
		TotalErrors:    c.globalTracker.GetTotalErrors(),
		CurrentRPS:     c.globalTracker.GetRPS(),
		ErrorRate:      c.globalTracker.GetErrorRate(),
		ActiveRequests: c.globalTracker.GetActiveConnections(),
		BytesReceived:  inBytes,
		BytesSent:      outBytes,
		DurationStats:  c.globalTracker.GetLatencyStats(),
		MethodMix:      c.globalTracker.GetMethodMix(),
		RouteMix:       c.globalTracker.GetRouteMix(),
		WindowSeconds:  c.windowDuration.Seconds(),
	}

	// 3. Per-Backend Runtime Metrics & Capacity
	c.mu.RLock()
	backends := make(map[string]*BackendRuntimeMetrics, len(c.backendTrackers))
	capacity := make(map[string]*BackendCapacityConfig, len(c.capacityConfigs))

	for id, tracker := range c.backendTrackers {
		u := c.backendURLs[id]
		healthy := c.backendHealth[id]

		backends[id] = &BackendRuntimeMetrics{
			BackendID:        id,
			URL:              u,
			Healthy:          healthy,
			ActiveConns:      tracker.GetActiveConnections(),
			TotalRequests:    tracker.GetTotalRequests(),
			TotalErrors:      tracker.GetTotalErrors(),
			ErrorsByCategory: tracker.GetErrorBreakdown(),
			RPS:              tracker.GetRPS(),
			ErrorRate:        tracker.GetErrorRate(),
			Latency:          tracker.GetLatencyStats(),
			WindowSeconds:    c.windowDuration.Seconds(),
			LastUpdated:      now,
		}
	}

	for id, cfg := range c.capacityConfigs {
		cp := *cfg
		capacity[id] = &cp
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
		Timestamp:       now,
		WindowSeconds:   c.windowDuration.Seconds(),
		Host:            host,
		GlobalTraffic:   globalTraffic,
		Backends:        backends,
		CapacityConfigs: capacity,
		Controller:      controller,
	}
}
