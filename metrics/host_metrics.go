package metrics

import (
	"runtime"
	"sync"
	"time"
)

// HostMetricsCollector collects runtime process memory and CPU pressure.
// It explicitly represents missing measurements when hardware counters are unavailable.
type HostMetricsCollector struct {
	mu           sync.Mutex
	lastSample   time.Time
	lastGCPause  uint64
	cpuSampled   bool
	cachedCPU    OptionalFloat
}

// NewHostMetricsCollector initializes the host resource metrics collector.
func NewHostMetricsCollector() *HostMetricsCollector {
	return &HostMetricsCollector{
		lastSample: time.Now(),
		cachedCPU:  MissingFloat(), // explicitly missing until valid sample collected
	}
}

// Sample collects a point-in-time HostResourceMetrics snapshot.
func (c *HostMetricsCollector) Sample() HostResourceMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	now := time.Now()
	goroutines := runtime.NumGoroutine()

	// Calculate GC activity
	gcPauseDelta := uint64(0)
	if m.PauseTotalNs >= c.lastGCPause && c.lastGCPause > 0 {
		gcPauseDelta = m.PauseTotalNs - c.lastGCPause
	}
	c.lastGCPause = m.PauseTotalNs

	// Estimate process CPU pressure based on GC pause and sampling interval
	dt := now.Sub(c.lastSample)
	if dt >= 500*time.Millisecond {
		c.lastSample = now
		// If GC pause was measured, calculate fraction of CPU spent in stop-the-world
		fraction := float64(gcPauseDelta) / float64(dt.Nanoseconds())
		if fraction > 1.0 {
			fraction = 1.0
		}
		// Scale to percentage
		c.cachedCPU = NewOptionalFloat(fraction * 100.0)
		c.cpuSampled = true
	}

	// Memory utilization
	usedBytes := m.Alloc
	totalBytes := m.Sys
	memPercent := MissingFloat()
	if totalBytes > 0 {
		memPercent = NewOptionalFloat((float64(usedBytes) / float64(totalBytes)) * 100.0)
	}

	cpuMetric := c.cachedCPU
	if !c.cpuSampled {
		cpuMetric = MissingFloat() // explicitly missing, not silently 0.0
	}

	return HostResourceMetrics{
		Timestamp:        now,
		CPUUtilization:   cpuMetric,
		MemoryUsedBytes:  usedBytes,
		MemoryTotalBytes: totalBytes,
		MemoryPercent:    memPercent,
		GoroutineCount:   goroutines,
		GCNum:            m.NumGC,
		GCPauseTotalNs:   m.PauseTotalNs,
		Source:           "process_runtime",
		LimitationNote:   "Host/process metrics represent the load balancer process. Backend processes running on the same host must expose their own process measurements for per-backend resource attribution.",
	}
}
