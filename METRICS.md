# Metrics Contract Specification

**Phase 1 Deliverable — Define the Metrics Contract**

This document specifies the metrics contract, schemas, package layout, and quality rules implemented for the Adaptive Load Balancer in Go.

---

## 1. Go Package Layout

The `metrics/` package encapsulates all telemetry, histograms, sliding windows, and normalization:

```text
metrics/
├── types.go               # Metric schemas, metric groups, units, and OptionalFloat
├── collector.go           # Central MetricsCollector coordinating all metric groups
├── request_tracker.go     # Hot-path atomic counters, error classification, and in-flight tracking
├── latency_histogram.go   # Bounded circular reservoir + log-spaced histogram for p50/p90/p95/p99
├── window.go              # Sliding time window aggregator and monotonic rate tracker
├── host_metrics.go        # Host and process runtime memory, GC pauses, and CPU pressure
├── backend_probe.go       # Active health check probe latencies and success/failure tracking
├── normalize.go           # MinMax and Z-Score normalization, feature vector generation for AI
└── metrics_test.go        # Unit, concurrency, and serialization test suite
```

---

## 2. Metric Groups

### Group 1: Per-Backend Runtime Metrics
* **Purpose**: Observe load and service quality per upstream node.
* **Fields**:
  * `backend_id` (`string`): Unique backend identifier.
  * `url` (`string`): Upstream HTTP URL.
  * `healthy` (`bool`): Active liveness state.
  * `active_connections` (`int64`): Atomic counter of currently in-flight requests.
  * `total_requests` (`uint64`): Monotonic count of all processed requests.
  * `total_errors` (`uint64`): Monotonic count of failed requests.
  * `errors_by_category` (`object`): Breakdown by root cause (`http_5xx`, `transport`, `timeout`, `cancellation`).
  * `rps` (`float64`): Current throughput (requests/second) computed across the sliding window.
  * `error_rate` (`float64`): Failures per second across the sliding window.
  * `latency` (`object`): Percentiles (`p50_ms`, `p90_ms`, `p95_ms`, `p99_ms`, `min_ms`, `max_ms`, `avg_ms`) and sample count.
  * `window_seconds` (`float64`): Aggregation window length (default: 10s).

### Group 2: Host & Resource Metrics
* **Purpose**: Observe CPU and memory resource pressure.
* **Fields**:
  * `cpu_utilization` (`OptionalFloat`): Process CPU pressure. Returns `null` if not yet sampled (never silently 0.0).
  * `memory_used_bytes` (`uint64`): Current allocated memory (`runtime.MemStats.Alloc`).
  * `memory_total_bytes` (`uint64`): Total memory obtained from OS (`runtime.MemStats.Sys`).
  * `memory_percent` (`OptionalFloat`): Ratio of allocated to total OS memory.
  * `goroutine_count` (`int`): Active goroutines in the process.
  * `gc_num` (`uint32`): Total garbage collection cycles completed.
  * `gc_pause_total_ns` (`uint64`): Cumulative stop-the-world GC pause time in nanoseconds.
  * `source` (`string`): `"process_runtime"`.
  * `limitation_note` (`string`): Notes that host metrics reflect the load balancer; backends on the same host must expose individual measurements.

### Group 3: Global Traffic Metrics
* **Purpose**: Describe incoming workload across all endpoints.
* **Fields**:
  * `total_requests` (`uint64`): Monotonic count of all incoming requests.
  * `total_errors` (`uint64`): Monotonic count of all failures.
  * `current_rps` (`float64`): Requests per second across the active window.
  * `error_rate` (`float64`): Errors per second across the active window.
  * `active_requests` (`int64`): In-flight requests currently being served.
  * `bytes_received` (`uint64`): Total payload bytes received (`Content-Length`).
  * `bytes_sent` (`uint64`): Total response bytes written.
  * `duration_stats` (`LatencyStats`): Overall latency distribution percentiles.
  * `method_mix` (`map[string]uint64`): Breakdown by HTTP method (`GET`, `POST`, etc.).
  * `route_mix` (`map[string]uint64`): Breakdown by URL path.

### Group 4: Capacity & Configuration
* **Purpose**: Describe heterogeneous backend server hardware and limits.
* **Fields**:
  * `backend_id` (`string`): Unique backend identifier.
  * `url` (`string`): Backend endpoint.
  * `static_weight` (`int`): Capacity weight configured for weighted algorithms.
  * `cpu_cores` (`int`): Dedicated or provisioned CPU core limit.
  * `memory_limit_bytes` (`uint64`): Provisioned memory ceiling.
  * `connection_limit` (`int64`): Hard concurrency threshold.

### Group 5: Controller Metrics
* **Purpose**: Measure adaptive controller behavior, policy decisions, and overhead.
* **Fields**:
  * `current_strategy` (`string`): Active algorithm (`Round-Robin`, `Least-Connections`, `Smooth-Weighted-Round-Robin`, `Consistent-Hash`).
  * `strategy_age_seconds` (`float64`): Time elapsed since the last algorithm swap.
  * `last_decision_microseconds` (`float64`): Duration of the last strategy selection decision.
  * `policy_version` (`string`): Active controller version (`1.0.0-baseline`).
  * `switch_count` (`uint64`): Total number of algorithm transitions executed.
  * `last_switch_at` (`time.Time`): Timestamp of the most recent algorithm transition.

---

## 3. Metric Quality Rules & Implementation

| Quality Rule | Implementation |
| :--- | :--- |
| **Monotonic Counters & Interval Rates** | Monotonic counters strictly use atomic increments (`atomic.AddUint64`). Rates are calculated exclusively by differences over time buckets using [`RollingWindow`](./metrics/window.go) and [`MonotonicRateTracker`](./metrics/window.go). |
| **Percentile Distributions (No Averaging)** | [`LatencyHistogram`](./metrics/latency_histogram.go) uses a bounded 2,048-sample circular reservoir combined with 16 log-spaced buckets to compute exact p50, p90, p95, and p99 percentiles without averaging raw latencies. |
| **Consistent Failure Classification** | [`ClassifyFailure()`](./metrics/request_tracker.go) categorizes errors into `http_5xx`, `transport` (connection refused/reset), `timeout` (deadline exceeded, net timeouts), and `cancellation` (client context canceled). |
| **Strict Duration Units** | All internal functions pass and record durations as `time.Duration`. Conversion to float64 milliseconds (`P50Ms`, `AvgMs`, etc.) is performed exclusively at JSON serialization boundaries. |
| **Hot-Path Lock-Free Updates** | Hot-path updates in [`RequestTracker`](./metrics/request_tracker.go) execute using lock-free atomic primitives for active connections, total requests, error counters, and byte counts. |
| **Explicit Missing Metric Representation** | Handled via [`OptionalFloat`](./metrics/types.go). When a hardware reading is unavailable or not yet sampled, it serializes to JSON `null` rather than silently defaulting to `0.0`. |

---

## 4. Telemetry Endpoint: `GET /api/metrics`

The Load Balancer exposes the unified metrics contract at `http://localhost:8080/api/metrics`:

```json
{
  "timestamp": "2026-10-15T23:58:24.123+05:30",
  "window_seconds": 10.0,
  "host": {
    "timestamp": "2026-10-15T23:58:24.123+05:30",
    "cpu_utilization": 2.45,
    "memory_used_bytes": 14285760,
    "memory_total_bytes": 35651584,
    "memory_percent": 40.07,
    "goroutine_count": 18,
    "gc_num": 12,
    "gc_pause_total_ns": 450200,
    "source": "process_runtime",
    "limitation_note": "Host/process metrics represent the load balancer process..."
  },
  "global_traffic": {
    "total_requests": 1542,
    "total_errors": 0,
    "current_rps": 154.2,
    "error_rate": 0.0,
    "active_requests": 3,
    "bytes_received": 197376,
    "bytes_sent": 508860,
    "duration_stats": {
      "sample_count": 1542,
      "p50_ms": 0.28,
      "p90_ms": 0.52,
      "p95_ms": 1.15,
      "p99_ms": 3.42,
      "min_ms": 0.05,
      "max_ms": 12.10,
      "avg_ms": 0.41
    },
    "method_mix": { "GET": 1500, "POST": 42 },
    "route_mix": { "/api/data": 1200, "/": 342 }
  },
  "backends": {
    "Backend-1": {
      "backend_id": "Backend-1",
      "url": "http://localhost:8081",
      "healthy": true,
      "active_connections": 1,
      "total_requests": 771,
      "total_errors": 0,
      "errors_by_category": {
        "http_5xx": 0,
        "transport": 0,
        "timeout": 0,
        "cancellation": 0
      },
      "rps": 77.1,
      "error_rate": 0.0,
      "latency": {
        "sample_count": 771,
        "p50_ms": 0.25,
        "p90_ms": 0.48,
        "p95_ms": 0.95,
        "p99_ms": 2.80,
        "min_ms": 0.05,
        "max_ms": 8.50,
        "avg_ms": 0.38
      },
      "window_seconds": 10.0,
      "last_updated": "2026-10-15T23:58:24.123+05:30"
    }
  },
  "capacity_configs": {
    "Backend-1": {
      "backend_id": "Backend-1",
      "url": "http://localhost:8081",
      "static_weight": 3,
      "cpu_cores": 4,
      "memory_limit_bytes": 536870912,
      "connection_limit": 100
    }
  },
  "controller": {
    "current_strategy": "Round-Robin",
    "strategy_age_seconds": 124.5,
    "last_decision_microseconds": 12.0,
    "policy_version": "1.0.0-baseline",
    "switch_count": 0,
    "last_switch_at": "2026-10-15T23:56:19.623+05:30"
  }
}
```

---

## 5. Machine Learning & Controller Normalization (`metrics/normalize.go`)

The [`normalize.go`](./metrics/normalize.go) module transforms metrics into normalized numerical feature vectors for AI/reinforcement learning controllers:

```go
// Generates a 6-dimensional normalized state vector [0.0, 1.0]
vector := metrics.BackendFeatureVector(backendMetrics, capacityConfig)
```
* `[0]` Liveness state (`1.0` or `0.0`)
* `[1]` Active connections normalized (`0 to 100 max`)
* `[2]` Backend RPS normalized (`0 to 1000 req/s max`)
* `[3]` Error rate normalized (`0 to 100 err/s max`)
* `[4]` Latency p95 in milliseconds normalized (`0 to 500ms max`)
* `[5]` Static capacity weight normalized (`1 to 10 max`)
