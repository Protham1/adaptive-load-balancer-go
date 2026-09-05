# Adaptive Load Balancer in Go

A high-performance, modular HTTP reverse-proxy load balancer built with Go's standard library (`net/http`, `net/http/httputil`, `sync`, `sync/atomic`).

---

## 🚀 Architecture Overview

```
                        ┌────────────────────────┐
                        │   Client HTTP Request  │
                        │    (e.g., :8080/api)   │
                        └───────────┬────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                          Load Balancer Core                            │
│                                                                        │
│   [lb.go]                                                              │
│      └─► lbHandler receives HTTP Request                               │
│            │                                                           │
│            ▼                                                           │
│   [backend.go]                                                         │
│      └─► ServerPool.GetNextPeer()                                      │
│            │                                                           │
│            ▼                                                           │
│   [strategy/round_robin.go]                                            │
│      └─► SelectionStrategy[T Peer] (Round-Robin with atomic rotation)  │
│            │                                                           │
│            ▼                                                           │
│   Returns selected healthy *Backend                                    │
│            │                                                           │
│            ▼                                                           │
│   [Backend.ReverseProxy] forwards request to upstream backend          │
└────────────────────────────────────────────────────────────────────────┘
                 │                      │                     │
                 ▼                      ▼                     ▼
        ┌─────────────────┐    ┌─────────────────┐   ┌─────────────────┐
        │    Backend 1    │    │    Backend 2    │   │    Backend 3    │
        │   (Port 8081)   │    │   (Port 8082)   │   │   (Port 8083)   │
        └─────────────────┘    └─────────────────┘   └─────────────────┘
                 ▲                      ▲                     ▲
                 │                      │                     │
                 └──────────────┬───────┴─────────────────────┘
                                │
                 ┌──────────────┴────────────────────────┐
                 │    Concurrent Active Health Checker   │
                 │   (sync.WaitGroup background worker)  │
                 └───────────────────────────────────────┘
```

---

## 📁 Repository Structure

```text
adaptive-load/
├── strategy/
│   ├── strategy.go         # SelectionStrategy[T Peer] generic interface & Peer contract
│   ├── round_robin.go      # Atomic Round-Robin algorithm with dead-backend skipping
│   └── round_robin_test.go # Isolated unit tests for round-robin logic
├── backend.go              # Backend struct, thread-safe state, ServerPool, and HealthChecker
├── dummy_backends.go       # Lightweight test HTTP servers (ports 8081, 8082, 8083)
├── lb.go                   # HTTP reverse proxy request handler
├── lb_test.go              # End-to-end load balancer integration tests
├── main.go                 # Application bootstrap and entrypoint
└── go.mod                  # Go module definition
```

---

## 🔑 Key Components & Design Decisions

### 1. Pluggable Selection Strategies (`strategy/`)
- Algorithms are decoupled from the load balancer using the generic **`SelectionStrategy[T Peer]`** interface:
  ```go
  type Peer interface {
      IsAlive() bool
  }

  type SelectionStrategy[T Peer] interface {
      SelectNextBackend(peers []T) T
      Name() string
  }
  ```
- **Why `Peer`?** Prevents circular dependencies (`strategy` does not need to import `main`), decouples algorithm math from HTTP internals, and makes algorithms easily testable with mock peers.
- **Dynamic Switching:** Strategies can be hot-swapped at runtime via `serverPool.SetStrategy(...)`.

### 2. Concurrent Active Health Checking (`backend.go`)
- Background worker executes periodic health checks every 10 seconds.
- Uses **`sync.WaitGroup`** to probe all backends **in parallel** via TCP dial rather than sequentially, reducing health check latency from $O(N \times \text{timeout})$ to $\approx \max(\text{dial timeout})$.
- Status updates (`SetAlive` / `IsAlive`) are protected with `sync.RWMutex`.

### 3. Lock-Free Atomic Indexing (`strategy/round_robin.go`)
- Uses `sync/atomic.AddUint64` for round-robin index advancement, avoiding lock contention on the hot request path.

---

## 🛠️ Getting Started

### Prerequisites
- Go 1.22+ installed

### Running the Load Balancer
Start the dummy backend servers and the load balancer:
```bash
go run .
```

You should see logs indicating backends are initialized on `:8081`, `:8082`, `:8083`, and the load balancer is listening on `:8080`.

### Testing Traffic Distribution
Send consecutive requests to the load balancer:
```bash
# In another terminal or browser:
curl http://localhost:8080
curl http://localhost:8080
curl http://localhost:8080
```
Responses will rotate evenly across `Backend-1`, `Backend-2`, and `Backend-3`.

### Running Tests
Execute all unit and integration tests:
```bash
go test -v ./...
```
