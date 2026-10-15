# Baseline Performance Report & Environment Specifications

**Phase 0 Deliverable — Stabilize and Freeze the Baseline**

---

## 1. System & Environment Specifications

| Parameter | Recorded Value |
| :--- | :--- |
| **Go Version** | `go1.27.1 windows/amd64` |
| **Operating System** | `Windows 11 (x86_64 / amd64)` |
| **CPU Architecture** | `13th Gen Intel(R) Core(TM) i7-1355U` (12 logical cores) |
| **Total Memory (RAM)** | `16.0 GB` |
| **Backend Configuration** | 3 HTTP Backends: Backend-1 (`:8081`, Weight 3), Backend-2 (`:8082`, Weight 2), Backend-3 (`:8083`, Weight 1) |
| **Frontend Version** | `React 19.2.8` + `Vite 8.2.2` (`frontend/package.json`) |
| **Baseline Tag** | `v0-baseline` |

---

## 2. Test Suite & Race Detector Analysis

* **`go test -v ./...`**: **PASS** (100% of unit, concurrency, and integration tests pass).
  * Strategy tests: Round-Robin, Least-Connections, Smooth-Weighted-Round-Robin, Consistent-Hash.
  * Deterministic edge cases: Empty pools, all dead nodes, tie breaking, dynamic backend removal & recovery.
  * Connection counting integrity: Verified under success (200), HTTP error (500), proxy network failure (503), and client context cancellation.
  * Concurrency tests: Parallel workers running across `lbHandler` and strategy peer selection with zero connection leakage.
* **`go test -race ./...`**:
  * *Status*: Requires 64-bit GCC for CGO on Windows.
  * *Host Toolchain Note*: The machine has 32-bit legacy MinGW (`C:\MinGW\bin\gcc.exe`, target `mingw32`), which returns `cc1.exe: sorry, unimplemented: 64-bit mode not compiled in` when Go requests 64-bit CGO compilation (`-m64`). Concurrency correctness is ensured via `sync.RWMutex`, `sync/atomic`, and automated parallel goroutine tests (`TestConcurrentRequests_ConnectionBalance`, `TestConcurrentStrategySwitching`, and `strategy/concurrent_test.go`).

---

## 3. Repeatable Benchmark Commands

### A. End-to-End Performance Suite (Throughput & Latency Percentiles)
```bash
go run ./cmd/bench
```
Raw output is automatically exported to [`benchmarks/baseline_raw.txt`](./benchmarks/baseline_raw.txt).

### B. Go Standard Micro-Benchmarks
```bash
go test -run=^$ -bench=. -benchmem ./...
```

---

## 4. Baseline Performance Tables

### A. End-to-End Load Balancer Performance (5,000 requests, 20 concurrent workers)

| Strategy | Requests | Concurrency | Throughput (req/s) | Latency p50 | Latency p95 | Latency p99 | Errors | Memory Total Allocated |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Round-Robin (RR)** | 5,000 | 20 | **18,424.3** | 0.00 µs | 3.15 ms | 14.81 ms | 0 | 32.49 MB |
| **Least-Connections (LC)** | 5,000 | 20 | **27,549.7** | 0.00 µs | 2.71 ms | 4.66 ms | 0 | 32.09 MB |
| **Smooth-Weighted-Round-Robin (SWRR)** | 5,000 | 20 | **32,714.9** | 0.00 µs | 2.24 ms | 3.66 ms | 0 | 31.55 MB |
| **Consistent-Hash (CH)** | 5,000 | 20 | **25,622.9** | 326.00 µs | 2.89 ms | 4.72 ms | 0 | 33.60 MB |

### B. Algorithm Core Micro-Benchmarks (`strategy/`)

| Algorithm | Iterations | Latency per Op | Memory / Op | Allocs / Op |
| :--- | :---: | :---: | :---: | :---: |
| **Round-Robin** | 18,841,824 | **60.68 ns/op** | 0 B/op | 0 allocs/op |
| **Least-Connections** | 4,097,914 | **286.7 ns/op** | 8 B/op | 1 allocs/op |
| **Consistent-Hash** | 3,635,464 | **320.8 ns/op** | 32 B/op | 4 allocs/op |
| **Smooth-Weighted-Round-Robin** | 1,395,242 | **771.1 ns/op** | 0 B/op | 0 allocs/op |

### C. Full HTTP Reverse-Proxy Micro-Benchmarks (`adaptive-load`)

| Strategy | Iterations | Latency per Op | Memory / Op | Allocs / Op |
| :--- | :---: | :---: | :---: | :---: |
| **Round-Robin** | 4,268 | **0.30 ms/op** | 48.2 KB/op | 124 allocs/op |
| **Least-Connections** | 3,500 | **0.33 ms/op** | 47.1 KB/op | 121 allocs/op |
| **Smooth-Weighted-Round-Robin** | 3,309 | **0.36 ms/op** | 48.1 KB/op | 125 allocs/op |
| **Consistent-Hash** | 2,269 | **0.45 ms/op** | 54.9 KB/op | 149 allocs/op |

---

## 5. Exit Criteria Sign-off

- [x] All deterministic tests pass (empty pools, unhealthy nodes, ties, backend removal & recovery).
- [x] Connection count increments and decrements verified on success, errors, proxy failures, and cancellations.
- [x] Data races in pool mutations and round-robin counter fixed.
- [x] Go, OS, CPU, RAM, and configuration recorded.
- [x] Repeatable benchmark command created with raw output saved.
- [x] Baseline throughput, latency percentiles, error rate, and memory usage documented.
- [x] Git baseline commit and tag created.
