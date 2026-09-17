# Phase 2: Telemetry Layer & Adaptive Load Balancer Roadmap

## 1. Objective
Implement a high-performance, modular **Telemetry Layer** for the Go HTTP Load Balancer to track real-time response latency (EWMA & percentiles P50/P95/P99), request rates (RPS), error rates, and HTTP status code distributions. This provides the observability foundation and metric interfaces necessary for subsequent adaptive routing algorithms (Peak EWMA, Least Response Time, and Outlier Ejection).

---

## 2. Telemetry Architecture

```text
Request Lifecycle & Telemetry Pipeline:
[Incoming Request] ──► [lbHandler Middleware] ──► [ServerPool (Selection Strategy)]
                                │
                                ├──► Record Start (Active Connections++)
                                │
                                ├──► [ReverseProxy / Response Recorder]
                                │        │
                                │        └──► Forward to Target Backend
                                │
                                ├──► Record Finish (Duration, HTTP Status, Error)
                                │        ├──► Update EWMA Moving Latency
                                │        ├──► Add to Percentile Window (P50/P95/P99)
                                │        ├──► Increment Status Counters (2xx, 4xx, 5xx)
                                │        └──► Active Connections--
                                │
                                └──► [Telemetry Engine & APIs]
                                         ├──► /api/telemetry (JSON Snapshot)
                                         ├──► /api/events (SSE Stream)
                                         └──► strategy.MetricPeer (Adaptive Routing)
```

---

## 3. Core Components

### A. Dedicated `telemetry/` Package
- **`EWMA` (`telemetry/ewma.go`)**:
  - Thread-safe Exponentially Weighted Moving Average tracker with configurable $\alpha$ decay (default $\alpha = 0.2$).
- **`Histogram` (`telemetry/histogram.go`)**:
  - Sliding time-window reservoir to compute `Min`, `Max`, `Mean`, `P50`, `P90`, `P95`, `P99` response times.
- **`BackendMetrics` (`telemetry/metrics.go`)**:
  - Atomic counters for in-flight requests, total requests, 2xx/3xx/4xx/5xx status codes, and network errors.
  - Throughput / RPS rolling calculation.
  - `Snapshot()` returning an immutable `BackendMetricsSnapshot`.
- **`ClusterMetrics` (`telemetry/metrics.go`)**:
  - Aggregates cluster-wide metrics across all configured backends.

### B. Load Balancer Integration
- **`responseRecorder` in `lb.go`**:
  - Captures status code and error details during reverse proxy execution.
- **`Backend` in `backend.go`**:
  - Integrates `*telemetry.BackendMetrics`.
  - Implements `strategy.MetricPeer` methods (`GetEWMALatency()`, `GetSuccessRate()`, `GetErrorRate()`).

### C. Observability APIs
- **`GET /api/telemetry`**: JSON endpoint returning live cluster and per-backend telemetry snapshots.
- **`GET /api/events`**: Enhanced SSE stream sending complete request stats with latency & status codes.

---

## 4. Implementation Steps
1. **Create `telemetry` package** with EWMA, Histogram, BackendMetrics, and ClusterMetrics.
2. **Add unit tests** in `telemetry/telemetry_test.go` verifying EWMA, percentiles, and concurrency safety.
3. **Extend `strategy.Peer`** with `MetricPeer` in `strategy/strategy.go`.
4. **Integrate `BackendMetrics` into `Backend`** in `backend.go`.
5. **Instrument reverse proxy in `lb.go`** using response recorder and expose `/api/telemetry`.
6. **Verify integration** with test suite (`go test ./...`) and manual traffic probe tests.
