package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestOptionalFloatJSON(t *testing.T) {
	// 1. Valid OptionalFloat
	v := NewOptionalFloat(75.5)
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal valid float: %v", err)
	}
	if string(data) != "75.5" {
		t.Errorf("expected 75.5, got %s", string(data))
	}

	var decodedVal OptionalFloat
	if err := json.Unmarshal(data, &decodedVal); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if !decodedVal.Valid || decodedVal.Value != 75.5 {
		t.Errorf("expected valid 75.5, got %+v", decodedVal)
	}

	// 2. Missing OptionalFloat must serialize to null, not 0.0
	missing := MissingFloat()
	dataMissing, err := json.Marshal(missing)
	if err != nil {
		t.Fatalf("failed to marshal missing float: %v", err)
	}
	if string(dataMissing) != "null" {
		t.Errorf("expected null for missing float, got %s", string(dataMissing))
	}

	var decodedMissing OptionalFloat
	if err := json.Unmarshal([]byte("null"), &decodedMissing); err != nil {
		t.Fatalf("failed to unmarshal null: %v", err)
	}
	if decodedMissing.Valid {
		t.Errorf("expected invalid for null JSON, got valid: %+v", decodedMissing)
	}
}

func TestLatencyHistogramPercentiles(t *testing.T) {
	h := NewLatencyHistogram(1000)

	// Feed 100 durations from 1ms to 100ms
	for i := 1; i <= 100; i++ {
		h.Record(time.Duration(i) * time.Millisecond)
	}

	stats := h.Snapshot()
	if stats.SampleCount != 100 {
		t.Errorf("expected 100 samples, got %d", stats.SampleCount)
	}
	if stats.Min != 1*time.Millisecond {
		t.Errorf("expected min 1ms, got %v", stats.Min)
	}
	if stats.Max != 100*time.Millisecond {
		t.Errorf("expected max 100ms, got %v", stats.Max)
	}

	// p50 should be around 51ms, p95 around 96ms, p99 around 100ms
	if stats.P50 < 49*time.Millisecond || stats.P50 > 52*time.Millisecond {
		t.Errorf("unexpected p50: %v", stats.P50)
	}
	if stats.P95 < 94*time.Millisecond || stats.P95 > 97*time.Millisecond {
		t.Errorf("unexpected p95: %v", stats.P95)
	}
	if stats.P99 < 98*time.Millisecond || stats.P99 > 100*time.Millisecond {
		t.Errorf("unexpected p99: %v", stats.P99)
	}

	if stats.P50Ms <= 0 || stats.P95Ms <= 0 || stats.P99Ms <= 0 {
		t.Errorf("serialization millisecond fields not populated: %+v", stats)
	}
}

func TestLatencyHistogramConcurrency(t *testing.T) {
	h := NewLatencyHistogram(500)
	const routines = 20
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(routines)
	for i := 0; i < routines; i++ {
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				h.Record(time.Duration(worker*10+j) * time.Microsecond)
			}
		}(i)
	}
	wg.Wait()

	stats := h.Snapshot()
	if stats.SampleCount != routines*iterations {
		t.Errorf("expected %d samples, got %d", routines*iterations, stats.SampleCount)
	}
}

func TestRollingWindowRate(t *testing.T) {
	w := NewRollingWindow(2*time.Second, 4)

	// Add 100 events
	w.Add(100)
	sum := w.Sum()
	if sum != 100 {
		t.Errorf("expected sum 100, got %d", sum)
	}

	rate := w.Rate()
	// Rate over 2s window should be 100 / 2 = 50 req/s
	if rate < 45 || rate > 55 {
		t.Errorf("expected rate ~50 req/s, got %.2f", rate)
	}
}

func TestMonotonicRateTracker(t *testing.T) {
	tracker := NewMonotonicRateTracker(100)
	time.Sleep(50 * time.Millisecond)

	rate := tracker.Update(200) // delta 100 over ~0.05s => ~2000/s
	if rate <= 0 {
		t.Errorf("expected positive rate, got %f", rate)
	}
}

func TestFailureClassification(t *testing.T) {
	// 1. Cancellation
	cat, ok := ClassifyFailure(0, context.Canceled)
	if !ok || cat != ErrCategoryCancellation {
		t.Errorf("expected cancellation, got %s, ok=%v", cat, ok)
	}

	// 2. Timeout
	cat, ok = ClassifyFailure(0, context.DeadlineExceeded)
	if !ok || cat != ErrCategoryTimeout {
		t.Errorf("expected timeout, got %s, ok=%v", cat, ok)
	}

	// 3. Transport error
	cat, ok = ClassifyFailure(0, &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if !ok || cat != ErrCategoryTransport {
		t.Errorf("expected transport, got %s, ok=%v", cat, ok)
	}

	// 4. HTTP 5xx
	cat, ok = ClassifyFailure(502, nil)
	if !ok || cat != ErrCategoryHTTP5xx {
		t.Errorf("expected http_5xx, got %s, ok=%v", cat, ok)
	}

	// 5. Success
	cat, ok = ClassifyFailure(200, nil)
	if ok || cat != "" {
		t.Errorf("expected no error for 200, got %s, ok=%v", cat, ok)
	}
}

func TestRequestTrackerLifecycle(t *testing.T) {
	tracker := NewRequestTracker(5 * time.Second)

	req1 := tracker.StartRequest("GET", "/api/items")
	if conns := tracker.GetActiveConnections(); conns != 1 {
		t.Errorf("expected 1 active connection, got %d", conns)
	}

	time.Sleep(5 * time.Millisecond)
	dur := tracker.EndRequest(req1, 200, nil, 128, 512)
	if dur <= 0 {
		t.Errorf("expected positive duration, got %v", dur)
	}
	if conns := tracker.GetActiveConnections(); conns != 0 {
		t.Errorf("expected 0 active connections after completion, got %d", conns)
	}

	// Failed request (503)
	req2 := tracker.StartRequest("POST", "/api/order")
	tracker.EndRequest(req2, 503, nil, 64, 32)

	if tracker.GetTotalRequests() != 2 {
		t.Errorf("expected 2 total requests, got %d", tracker.GetTotalRequests())
	}
	if tracker.GetTotalErrors() != 1 {
		t.Errorf("expected 1 total error, got %d", tracker.GetTotalErrors())
	}

	errors := tracker.GetErrorBreakdown()
	if errors.HTTP5xx != 1 {
		t.Errorf("expected 1 http_5xx error, got %d", errors.HTTP5xx)
	}

	methods := tracker.GetMethodMix()
	if methods["GET"] != 1 || methods["POST"] != 1 {
		t.Errorf("unexpected method mix: %+v", methods)
	}
}

func TestHostMetricsCollector(t *testing.T) {
	c := NewHostMetricsCollector()
	metrics := c.Sample()

	if metrics.MemoryUsedBytes == 0 {
		t.Errorf("expected non-zero memory used bytes")
	}
	if metrics.Source != "process_runtime" {
		t.Errorf("expected process_runtime source, got %s", metrics.Source)
	}
	if metrics.LimitationNote == "" {
		t.Errorf("expected limitation note")
	}
}

func TestBackendProbeTracker(t *testing.T) {
	pt := NewBackendProbeTracker()

	pt.RecordProbe("b1", true, 2*time.Millisecond, nil)
	st, found := pt.GetProbeStatus("b1")
	if !found || !st.Healthy || st.ConsecutiveSuccesses != 1 {
		t.Fatalf("unexpected probe status: %+v", st)
	}

	// Fail probe
	pt.RecordProbe("b1", false, 5*time.Millisecond, errors.New("timeout"))
	st, _ = pt.GetProbeStatus("b1")
	if st.Healthy || st.ConsecutiveFailures != 1 || st.ConsecutiveSuccesses != 0 {
		t.Fatalf("expected failure recorded: %+v", st)
	}
}

func TestNormalizeUtilities(t *testing.T) {
	// MinMax
	if val := MinMaxNormalize(50, 0, 100); val != 0.5 {
		t.Errorf("expected 0.5, got %f", val)
	}
	if val := MinMaxNormalize(150, 0, 100); val != 1.0 {
		t.Errorf("expected 1.0 for clamped upper bound, got %f", val)
	}
	if val := MinMaxNormalize(-10, 0, 100); val != 0.0 {
		t.Errorf("expected 0.0 for clamped lower bound, got %f", val)
	}

	// ZScore
	if val := ZScoreNormalize(10, 10, 2); val != 0.0 {
		t.Errorf("expected 0.0 for mean, got %f", val)
	}
	if val := ZScoreNormalize(14, 10, 2); val != 2.0 {
		t.Errorf("expected 2.0 for 2 stdDev, got %f", val)
	}

	// Vectorize
	b := &BackendRuntimeMetrics{
		Healthy:     true,
		ActiveConns: 50,
		RPS:         500,
		ErrorRate:   10,
		Latency:     LatencyStats{P95Ms: 250},
	}
	cap := &BackendCapacityConfig{StaticWeight: 5}

	vec := BackendFeatureVector(b, cap)
	if len(vec) != 6 {
		t.Fatalf("expected 6-feature vector, got %d", len(vec))
	}
	if vec[0] != 1.0 || vec[1] != 0.5 {
		t.Errorf("unexpected vector values: %v", vec)
	}
}

func TestMetricsCollectorIntegration(t *testing.T) {
	col := NewMetricsCollector(5 * time.Second)

	// Register backends
	col.RegisterBackend("backend-1", "http://localhost:8081", 3, 4, 1024*1024*512, 100)
	col.RegisterBackend("backend-2", "http://localhost:8082", 1, 2, 1024*1024*256, 50)

	// Track requests
	req1 := col.RecordRequestStart("backend-1", "GET", "/items")
	col.RecordRequestEnd("backend-1", req1, 200, nil, 100, 200)

	req2 := col.RecordRequestStart("backend-2", "POST", "/items")
	col.RecordRequestEnd("backend-2", req2, 500, errors.New("upstream failed"), 50, 0)

	// Switch strategy
	col.RecordStrategySwitch("Least-Connections", 15*time.Microsecond)

	// Verify snapshot
	snap := col.CollectSnapshot()
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}

	if snap.GlobalTraffic.TotalRequests != 2 {
		t.Errorf("expected 2 global requests, got %d", snap.GlobalTraffic.TotalRequests)
	}
	if snap.GlobalTraffic.TotalErrors != 1 {
		t.Errorf("expected 1 global error, got %d", snap.GlobalTraffic.TotalErrors)
	}

	b1 := snap.Backends["backend-1"]
	if b1 == nil || b1.TotalRequests != 1 || b1.TotalErrors != 0 {
		t.Errorf("unexpected backend-1 metrics: %+v", b1)
	}

	b2 := snap.Backends["backend-2"]
	if b2 == nil || b2.TotalRequests != 1 || b2.TotalErrors != 1 {
		t.Errorf("unexpected backend-2 metrics: %+v", b2)
	}

	if snap.Controller.CurrentStrategy != "Least-Connections" {
		t.Errorf("expected Least-Connections, got %s", snap.Controller.CurrentStrategy)
	}
	if snap.Controller.SwitchCount != 1 {
		t.Errorf("expected 1 switch, got %d", snap.Controller.SwitchCount)
	}
}

func TestMetricsCollectorConcurrency(t *testing.T) {
	col := NewMetricsCollector(5 * time.Second)
	for i := 0; i < 3; i++ {
		col.RegisterBackend(fmt.Sprintf("b-%d", i), fmt.Sprintf("http://localhost:808%d", i), 1, 2, 1000, 50)
	}

	const workers = 20
	const requestsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			bID := fmt.Sprintf("b-%d", workerID%3)
			for i := 0; i < requestsPerWorker; i++ {
				req := col.RecordRequestStart(bID, "GET", "/test")
				col.RecordRequestEnd(bID, req, 200, nil, 10, 20)
			}
		}(w)
	}

	wg.Wait()

	snap := col.CollectSnapshot()
	expectedTotal := uint64(workers * requestsPerWorker)
	if snap.GlobalTraffic.TotalRequests != expectedTotal {
		t.Errorf("expected %d total requests, got %d", expectedTotal, snap.GlobalTraffic.TotalRequests)
	}
	if snap.GlobalTraffic.ActiveRequests != 0 {
		t.Errorf("expected 0 active requests, got %d", snap.GlobalTraffic.ActiveRequests)
	}
}
