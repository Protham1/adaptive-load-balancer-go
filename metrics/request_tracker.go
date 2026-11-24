package metrics

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// RequestTracker manages monotonic counters, in-flight connections, categorized errors,
// and latency distributions for hot-path request execution.
type RequestTracker struct {
	activeConns   int64
	totalRequests uint64
	totalErrors   uint64
	bytesReceived uint64
	bytesSent     uint64

	// Monotonic categorized error counters
	errHTTP5xx      uint64
	errTransport    uint64
	errTimeout      uint64
	errCancellation uint64

	// Sliding windows for rate calculation
	reqWindow       *RollingWindow
	errWindow       *RollingWindow
	completedWindow *RollingWindow

	// Latency histograms: end-to-end total, upstream backend, load-balancer overhead
	histogram         *LatencyHistogram
	backendHistogram  *LatencyHistogram
	overheadHistogram *LatencyHistogram

	// Multi-alpha EMA smoothing (alpha = 0.1, 0.3, 0.5)
	rpsEMA        *MultiEMA
	errRateEMA    *MultiEMA
	p50EMA        *MultiEMA
	p95EMA        *MultiEMA
	p99EMA        *MultiEMA
	lastEMAUpdate time.Time
	emaMu         sync.RWMutex

	// Method and route mix tracking
	mixMu        sync.RWMutex
	methodCounts map[string]uint64
	routeCounts  map[string]uint64
}

// ActiveRequest captures metadata for an in-flight request, including timestamps
// for overhead and upstream backend latency separation.
type ActiveRequest struct {
	StartTime      time.Time
	ProxyStartTime time.Time
	Method         string
	Route          string
}

// NewRequestTracker constructs a high-performance request tracker.
func NewRequestTracker(windowDuration time.Duration) *RequestTracker {
	if windowDuration <= 0 {
		windowDuration = 10 * time.Second
	}
	return &RequestTracker{
		reqWindow:         NewRollingWindow(windowDuration, 10),
		errWindow:         NewRollingWindow(windowDuration, 10),
		completedWindow:   NewRollingWindow(windowDuration, 10),
		histogram:         NewLatencyHistogram(),
		backendHistogram:  NewLatencyHistogram(),
		overheadHistogram: NewLatencyHistogram(),
		rpsEMA:            NewMultiEMA(),
		errRateEMA:        NewMultiEMA(),
		p50EMA:            NewMultiEMA(),
		p95EMA:            NewMultiEMA(),
		p99EMA:            NewMultiEMA(),
		methodCounts:      make(map[string]uint64),
		routeCounts:       make(map[string]uint64),
	}
}

// StartRequest atomically increments active in-flight connections and monotonic counter.
func (t *RequestTracker) StartRequest(method, route string) *ActiveRequest {
	atomic.AddInt64(&t.activeConns, 1)
	atomic.AddUint64(&t.totalRequests, 1)
	t.reqWindow.Add(1)

	// Record method & route under read/write lock
	t.mixMu.Lock()
	if method != "" {
		t.methodCounts[method]++
	}
	if route != "" {
		t.routeCounts[route]++
	}
	t.mixMu.Unlock()

	return &ActiveRequest{
		StartTime: time.Now(),
		Method:    method,
		Route:     route,
	}
}

// MarkProxyStart records the moment the proxy forwards the request to the upstream backend,
// separating internal load-balancer overhead from end-to-end backend latency.
func (t *RequestTracker) MarkProxyStart(req *ActiveRequest) {
	if req != nil {
		req.ProxyStartTime = time.Now()
	}
}

// EndRequest atomically decrements active connections and records latency, throughput, and errors.
func (t *RequestTracker) EndRequest(req *ActiveRequest, statusCode int, err error, bytesIn, bytesOut uint64) time.Duration {
	atomic.AddInt64(&t.activeConns, -1)
	t.completedWindow.Add(1)

	duration := time.Duration(0)
	backendDuration := time.Duration(0)
	overheadDuration := time.Duration(0)

	if req != nil && !req.StartTime.IsZero() {
		duration = time.Since(req.StartTime)
		if !req.ProxyStartTime.IsZero() {
			backendDuration = time.Since(req.ProxyStartTime)
			if backendDuration > duration {
				backendDuration = duration
			}
			overheadDuration = duration - backendDuration
		} else {
			backendDuration = duration
			overheadDuration = 0
		}
		t.histogram.Record(duration)
		t.backendHistogram.Record(backendDuration)
		t.overheadHistogram.Record(overheadDuration)
	}

	if bytesIn > 0 {
		atomic.AddUint64(&t.bytesReceived, bytesIn)
	}
	if bytesOut > 0 {
		atomic.AddUint64(&t.bytesSent, bytesOut)
	}

	// Categorize failures consistently
	category, hasError := ClassifyFailure(statusCode, err)
	if hasError {
		atomic.AddUint64(&t.totalErrors, 1)
		t.errWindow.Add(1)

		switch category {
		case ErrCategoryHTTP5xx:
			atomic.AddUint64(&t.errHTTP5xx, 1)
		case ErrCategoryTransport:
			atomic.AddUint64(&t.errTransport, 1)
		case ErrCategoryTimeout:
			atomic.AddUint64(&t.errTimeout, 1)
		case ErrCategoryCancellation:
			atomic.AddUint64(&t.errCancellation, 1)
		}
	}

	return duration
}

// ClassifyFailure determines the error category based on HTTP status and Go error types.
func ClassifyFailure(statusCode int, err error) (ErrorCategory, bool) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ErrCategoryCancellation, true
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrCategoryTimeout, true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return ErrCategoryTimeout, true
		}
		var syscallErr *os.SyscallError
		if errors.As(err, &syscallErr) {
			if errors.Is(syscallErr.Err, syscall.ECONNREFUSED) || errors.Is(syscallErr.Err, syscall.ECONNRESET) {
				return ErrCategoryTransport, true
			}
		}
		return ErrCategoryTransport, true
	}

	if statusCode >= 500 && statusCode <= 599 {
		return ErrCategoryHTTP5xx, true
	}

	return "", false
}

// GetActiveConnections returns current in-flight connections.
func (t *RequestTracker) GetActiveConnections() int64 {
	return atomic.LoadInt64(&t.activeConns)
}

// GetTotalRequests returns total monotonic requests.
func (t *RequestTracker) GetTotalRequests() uint64 {
	return atomic.LoadUint64(&t.totalRequests)
}

// GetTotalErrors returns total monotonic failures.
func (t *RequestTracker) GetTotalErrors() uint64 {
	return atomic.LoadUint64(&t.totalErrors)
}

// GetErrorBreakdown returns categorized error numbers.
func (t *RequestTracker) GetErrorBreakdown() ErrorBreakdown {
	return ErrorBreakdown{
		HTTP5xx:      atomic.LoadUint64(&t.errHTTP5xx),
		Transport:    atomic.LoadUint64(&t.errTransport),
		Timeout:      atomic.LoadUint64(&t.errTimeout),
		Cancellation: atomic.LoadUint64(&t.errCancellation),
	}
}

// GetRPS returns current requests per second calculated across the active window.
func (t *RequestTracker) GetRPS() float64 {
	return t.reqWindow.Rate()
}

// GetErrorRate returns current errors per second across the active window.
func (t *RequestTracker) GetErrorRate() float64 {
	return t.errWindow.Rate()
}

// GetLatencyStats returns current total end-to-end latency distribution.
func (t *RequestTracker) GetLatencyStats() LatencyStats {
	return t.histogram.Snapshot()
}

// GetBackendLatencyStats returns upstream backend latency distribution.
func (t *RequestTracker) GetBackendLatencyStats() LatencyStats {
	return t.backendHistogram.Snapshot()
}

// GetOverheadLatencyStats returns load-balancer internal overhead latency distribution.
func (t *RequestTracker) GetOverheadLatencyStats() LatencyStats {
	return t.overheadHistogram.Snapshot()
}

// GetWindowMetadata extracts window timing and evaluates sample count against threshold.
func (t *RequestTracker) GetWindowMetadata(threshold uint64) WindowMetadata {
	return t.completedWindow.GetWindowMetadata(threshold)
}

// UpdateSmoothedValues synchronizes all MultiEMA smoothers with the current window observations.
func (t *RequestTracker) UpdateSmoothedValues() {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	rawRPS := t.GetRPS()
	rawErrRate := t.GetErrorRate()
	stats := t.histogram.Snapshot()

	t.rpsEMA.Update(rawRPS)
	t.errRateEMA.Update(rawErrRate)
	t.p50EMA.Update(stats.P50Ms)
	t.p95EMA.Update(stats.P95Ms)
	t.p99EMA.Update(stats.P99Ms)
	t.lastEMAUpdate = time.Now()
}

// GetSmoothedRPS returns the RPS smoothed values across alphas (0.1, 0.3, 0.5) alongside raw RPS.
func (t *RequestTracker) GetSmoothedRPS() SmoothedValue {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	raw := t.GetRPS()
	if !t.rpsEMA.ema01.Initialized() || time.Since(t.lastEMAUpdate) >= 100*time.Millisecond {
		t.lastEMAUpdate = time.Now()
		return t.rpsEMA.Update(raw)
	}
	v := t.rpsEMA.Value()
	v.Raw = raw
	return v
}

// GetSmoothedErrorRate returns smoothed error rates across alphas (0.1, 0.3, 0.5) alongside raw error rate.
func (t *RequestTracker) GetSmoothedErrorRate() SmoothedValue {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	raw := t.GetErrorRate()
	if !t.errRateEMA.ema01.Initialized() || time.Since(t.lastEMAUpdate) >= 100*time.Millisecond {
		t.lastEMAUpdate = time.Now()
		return t.errRateEMA.Update(raw)
	}
	v := t.errRateEMA.Value()
	v.Raw = raw
	return v
}

// GetSmoothedP50 returns smoothed p50 latency across alphas alongside raw p50.
func (t *RequestTracker) GetSmoothedP50() SmoothedValue {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	stats := t.histogram.Snapshot()
	if !t.p50EMA.ema01.Initialized() || time.Since(t.lastEMAUpdate) >= 100*time.Millisecond {
		t.lastEMAUpdate = time.Now()
		return t.p50EMA.Update(stats.P50Ms)
	}
	v := t.p50EMA.Value()
	v.Raw = stats.P50Ms
	return v
}

// GetSmoothedP95 returns smoothed p95 latency across alphas alongside raw p95.
func (t *RequestTracker) GetSmoothedP95() SmoothedValue {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	stats := t.histogram.Snapshot()
	if !t.p95EMA.ema01.Initialized() || time.Since(t.lastEMAUpdate) >= 100*time.Millisecond {
		t.lastEMAUpdate = time.Now()
		return t.p95EMA.Update(stats.P95Ms)
	}
	v := t.p95EMA.Value()
	v.Raw = stats.P95Ms
	return v
}

// GetSmoothedP99 returns smoothed p99 latency across alphas alongside raw p99.
func (t *RequestTracker) GetSmoothedP99() SmoothedValue {
	t.emaMu.Lock()
	defer t.emaMu.Unlock()

	stats := t.histogram.Snapshot()
	if !t.p99EMA.ema01.Initialized() || time.Since(t.lastEMAUpdate) >= 100*time.Millisecond {
		t.lastEMAUpdate = time.Now()
		return t.p99EMA.Update(stats.P99Ms)
	}
	v := t.p99EMA.Value()
	v.Raw = stats.P99Ms
	return v
}

// GetMethodMix returns a snapshot of requests broken down by HTTP method.
func (t *RequestTracker) GetMethodMix() map[string]uint64 {
	t.mixMu.RLock()
	defer t.mixMu.RUnlock()
	res := make(map[string]uint64, len(t.methodCounts))
	for k, v := range t.methodCounts {
		res[k] = v
	}
	return res
}

// GetRouteMix returns a snapshot of requests broken down by route.
func (t *RequestTracker) GetRouteMix() map[string]uint64 {
	t.mixMu.RLock()
	defer t.mixMu.RUnlock()
	res := make(map[string]uint64, len(t.routeCounts))
	for k, v := range t.routeCounts {
		res[k] = v
	}
	return res
}

// GetBytes returns monotonic bytes received and sent.
func (t *RequestTracker) GetBytes() (in uint64, out uint64) {
	return atomic.LoadUint64(&t.bytesReceived), atomic.LoadUint64(&t.bytesSent)
}
