package metrics

import (
	"sync"
	"time"
)

// RollingWindow aggregates counts over a sliding time window divided into sub-buckets.
// Ensures rates are calculated by difference over a known time interval.
type RollingWindow struct {
	mu             sync.RWMutex
	windowDuration time.Duration
	bucketDuration time.Duration
	numBuckets     int
	buckets        []windowBucket
	lastRotate     time.Time
}

type windowBucket struct {
	timestamp time.Time
	count     uint64
}

// NewRollingWindow constructs a sliding window.
// Default: 10-second window divided into 10 buckets of 1-second each.
func NewRollingWindow(windowDuration time.Duration, numBuckets int) *RollingWindow {
	if windowDuration <= 0 {
		windowDuration = 10 * time.Second
	}
	if numBuckets <= 0 {
		numBuckets = 10
	}
	bucketDur := windowDuration / time.Duration(numBuckets)
	now := time.Now()

	buckets := make([]windowBucket, numBuckets)
	for i := range buckets {
		buckets[i] = windowBucket{timestamp: now}
	}

	return &RollingWindow{
		windowDuration: windowDuration,
		bucketDuration: bucketDur,
		numBuckets:     numBuckets,
		buckets:        buckets,
		lastRotate:     now,
	}
}

// Add increments the count in the current bucket by delta.
func (w *RollingWindow) Add(delta uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.rotate(time.Now())
	currIdx := w.currentBucketIndex(time.Now())
	w.buckets[currIdx].count += delta
}

// Sum returns total counts across active buckets within the window.
func (w *RollingWindow) Sum() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-w.windowDuration)

	var total uint64
	for _, b := range w.buckets {
		if b.timestamp.After(cutoff) {
			total += b.count
		}
	}
	return total
}

// Rate calculates the rate of events per second over the active window.
func (w *RollingWindow) Rate() float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-w.windowDuration)

	var total uint64
	var oldestActive time.Time

	for _, b := range w.buckets {
		if b.timestamp.After(cutoff) && b.count > 0 {
			total += b.count
			if oldestActive.IsZero() || b.timestamp.Before(oldestActive) {
				oldestActive = b.timestamp
			}
		}
	}

	if total == 0 {
		return 0.0
	}

	elapsed := w.windowDuration.Seconds()
	if elapsed <= 0 {
		return 0.0
	}
	return float64(total) / elapsed
}

func (w *RollingWindow) rotate(now time.Time) {
	currIdx := w.currentBucketIndex(now)
	if now.Sub(w.buckets[currIdx].timestamp) >= w.bucketDuration {
		w.buckets[currIdx] = windowBucket{
			timestamp: now,
			count:     0,
		}
		w.lastRotate = now
	}
}

func (w *RollingWindow) currentBucketIndex(now time.Time) int {
	sec := now.UnixNano() / int64(w.bucketDuration)
	return int(sec % int64(w.numBuckets))
}

// MonotonicRateTracker calculates rates from a strictly monotonic counter
// by tracking value differences over interval elapsed time.
type MonotonicRateTracker struct {
	mu        sync.Mutex
	lastValue uint64
	lastTime  time.Time
}

// NewMonotonicRateTracker initializes the rate tracker.
func NewMonotonicRateTracker(initial uint64) *MonotonicRateTracker {
	return &MonotonicRateTracker{
		lastValue: initial,
		lastTime:  time.Now(),
	}
}

// Update computes the rate (units per second) since the previous update.
func (m *MonotonicRateTracker) Update(currentTotal uint64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if m.lastTime.IsZero() {
		m.lastValue = currentTotal
		m.lastTime = now
		return 0.0
	}

	dt := now.Sub(m.lastTime).Seconds()
	if dt <= 0 {
		return 0.0
	}

	var delta uint64
	if currentTotal >= m.lastValue {
		delta = currentTotal - m.lastValue
	}

	m.lastValue = currentTotal
	m.lastTime = now

	return float64(delta) / dt
}
