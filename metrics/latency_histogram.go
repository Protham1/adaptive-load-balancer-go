package metrics

import (
	"sort"
	"sync"
	"time"
)

const defaultReservoirSize = 2048

// LatencyHistogram maintains bounded samples and bucket counters to calculate
// percentiles (p50, p90, p95, p99) accurately without averaging raw request latencies.
type LatencyHistogram struct {
	mu        sync.RWMutex
	samples   []time.Duration
	head      int
	count     uint64
	min       time.Duration
	max       time.Duration
	sum       time.Duration
	capacity  int

	// Log-spaced bucket boundaries: <100µs, 250µs, 500µs, 1ms, 2.5ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, >5s
	buckets [16]uint64
}

// NewLatencyHistogram creates a thread-safe histogram with a bounded reservoir size.
func NewLatencyHistogram(size ...int) *LatencyHistogram {
	capSize := defaultReservoirSize
	if len(size) > 0 && size[0] > 0 {
		capSize = size[0]
	}
	return &LatencyHistogram{
		samples:  make([]time.Duration, 0, capSize),
		capacity: capSize,
	}
}

// Record inserts a duration measurement in a thread-safe manner.
func (h *LatencyHistogram) Record(d time.Duration) {
	if d < 0 {
		d = 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.count++
	h.sum += d

	if h.count == 1 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}

	// Update bucket count
	bIdx := durationBucketIndex(d)
	h.buckets[bIdx]++

	// Update bounded circular reservoir
	if len(h.samples) < h.capacity {
		h.samples = append(h.samples, d)
	} else {
		h.samples[h.head] = d
		h.head = (h.head + 1) % h.capacity
	}
}

// Snapshot computes percentile distributions and returns LatencyStats.
func (h *LatencyHistogram) Snapshot() LatencyStats {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.samples) == 0 {
		return LatencyStats{}
	}

	// Make a copy of current reservoir samples to sort
	copied := make([]time.Duration, len(h.samples))
	copy(copied, h.samples)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i] < copied[j]
	})

	n := len(copied)
	p50 := copied[int(float64(n)*0.50)]
	p90 := copied[int(float64(n)*0.90)]
	p95 := copied[int(float64(n)*0.95)]
	p99 := copied[int(float64(n)*0.99)]

	avg := time.Duration(0)
	if h.count > 0 {
		avg = time.Duration(int64(h.sum) / int64(h.count))
	}

	stats := LatencyStats{
		SampleCount: h.count,
		P50:         p50,
		P90:         p90,
		P95:         p95,
		P99:         p99,
		Min:         h.min,
		Max:         h.max,
		Avg:         avg,
	}
	stats.PopulateSerializationFields()
	return stats
}

// Reset clears recorded samples and statistics.
func (h *LatencyHistogram) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.samples = h.samples[:0]
	h.head = 0
	h.count = 0
	h.min = 0
	h.max = 0
	h.sum = 0
	for i := range h.buckets {
		h.buckets[i] = 0
	}
}

func durationBucketIndex(d time.Duration) int {
	switch {
	case d < 100*time.Microsecond:
		return 0
	case d < 250*time.Microsecond:
		return 1
	case d < 500*time.Microsecond:
		return 2
	case d < 1*time.Millisecond:
		return 3
	case d < 2500*time.Microsecond:
		return 4
	case d < 5*time.Millisecond:
		return 5
	case d < 10*time.Millisecond:
		return 6
	case d < 25*time.Millisecond:
		return 7
	case d < 50*time.Millisecond:
		return 8
	case d < 100*time.Millisecond:
		return 9
	case d < 250*time.Millisecond:
		return 10
	case d < 500*time.Millisecond:
		return 11
	case d < 1*time.Second:
		return 12
	case d < 2500*time.Millisecond:
		return 13
	case d < 5*time.Second:
		return 14
	default:
		return 15
	}
}
