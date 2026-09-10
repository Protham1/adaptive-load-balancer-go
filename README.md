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
│   ├── strategy.go                 # Generic SelectionStrategy & Peer sub-interfaces
│   ├── round_robin.go              # Atomic Round-Robin algorithm
│   ├── round_robin_test.go         # Round-Robin unit tests
│   ├── least_connections.go       # Least Connections with round-robin tie-breaking
│   ├── least_connections_test.go  # Least Connections unit tests
│   ├── weighted_round_robin.go     # Nginx Smooth Weighted Round-Robin
│   ├── weighted_round_robin_test.go# SWRR unit tests
│   ├── consistent_hash.go          # Ketama Hash Ring with virtual nodes (IP affinity)
│   └── consistent_hash_test.go     # Consistent Hash unit tests
├── backend.go                      # Backend struct, active connections, weights, ServerPool, health checker
├── dummy_backends.go               # Lightweight test HTTP servers (ports 8081, 8082, 8083)
├── lb.go                           # HTTP reverse proxy request handler with connection tracking & IP routing
├── lb_test.go                      # End-to-end load balancer integration tests
├── main.go                         # Application bootstrap and entrypoint
└── go.mod                          # Go module definition
```

---

## 🔑 Available Load Balancing Algorithms

| Strategy | Type | Description | Best For |
| :--- | :--- | :--- | :--- |
| **Round-Robin** | Static | Cycles sequentially through all alive backends using lock-free atomic counters. | Uniform hardware, stateless workloads. |
| **Least-Connections** | Dynamic | Routes traffic to the backend with the fewest active in-flight requests. Ties broken via round-robin. | Long-running queries, WebSockets, file downloads. |
| **Smooth-Weighted-Round-Robin** | Static | Nginx smooth algorithm that interleaves requests based on node weights without burst clusters. | Heterogeneous server hardware (e.g., 8-CPU vs 2-CPU). |
| **Consistent-Hash** | Stateful | 32-bit FNV-1a hash ring with virtual nodes (default 100). Maps Client IP consistently to a backend. | Session affinity, cache hit maximization. |

### Switching Algorithms Dynamically

Strategies can be chosen at initialization or swapped dynamically at runtime:

```go
// Round-Robin (Default)
serverPool := NewServerPool(strategy.NewRoundRobin[*Backend]())

// Least Connections
serverPool.SetStrategy(strategy.NewLeastConnections[*Backend]())

// Smooth Weighted Round-Robin
serverPool.SetStrategy(strategy.NewSmoothWeightedRoundRobin[*Backend]())

// Consistent Hashing (IP affinity with 100 virtual replicas)
serverPool.SetStrategy(strategy.NewConsistentHash[*Backend](100))
```

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

### Running Tests
Execute all unit and integration tests:
```bash
go test -v ./...
```
