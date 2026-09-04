# Phase 1 Requirements: HTTP Load Balancer in Go (Round-Robin)

## Objective
Build a fundamental HTTP Reverse-Proxy Load Balancer in Go using the standard library (`net/http`, `net/http/httputil`, `sync/atomic`).

---

### Step 1: Define Backend Server & ServerPool Data Structures
- Define `Backend` struct holding:
  - `URL`: `*url.URL`
  - `Alive`: `bool`
  - `mux`: `sync.RWMutex`
  - `ReverseProxy`: `*httputil.ReverseProxy`
- Define `ServerPool` struct holding:
  - `backends`: `[]*Backend`
  - `current`: `uint64` (atomic counter for round-robin)

---

### Step 2: Implement Round-Robin Backend Selection Logic
- Create `GetNextPeer()` on `ServerPool`.
- Increment `current` using `atomic.AddUint64`.
- Calculate target backend index via `(current - 1) % len(backends)`.
- Skip backends where `Alive == false`.

---

### Step 3: Implement Reverse Proxy Load Balancer Handler
- Define HTTP handler for Load Balancer listening on `:8080`.
- For each incoming request, retrieve the next available backend via `GetNextPeer()`.
- Proxy the request using `ReverseProxy.ServeHTTP()`.

---

### Step 4: Implement 3 Dummy HTTP Backend Servers
- Spin up 3 dummy backend HTTP servers:
  - Backend 1: `http://localhost:8081`
  - Backend 2: `http://localhost:8082`
  - Backend 3: `http://localhost:8083`
- Each backend responds with a clear identifying payload (e.g. `Response from Backend 1 (Port 8081)`).

---

### Step 5: Implement Active Health Checking
- Add a periodic background routine (`HealthCheck()`) running every few seconds.
- Ping `/health` or establish a connection to each backend.
- Dynamically update backend `Alive` status when backends go offline or recover.

---

### Step 6: Integration & Verification
- Wire up `main.go` to start the 3 dummy backends and launch the Load Balancer.
- Send consecutive HTTP requests to `http://localhost:8080` to verify responses cycle evenly across ports `8081`, `8082`, and `8083`.
