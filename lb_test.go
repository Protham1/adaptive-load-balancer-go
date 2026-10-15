package main

import (
	"adaptive-load/strategy"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestRoundRobinSelection(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")
	u3, _ := url.Parse("http://localhost:8083")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)
	b3 := NewBackend(u3)

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)
	pool.AddBackend(b3)

	// Verify sequential round-robin distribution
	expected := []*Backend{b2, b3, b1, b2, b3, b1}
	for i, exp := range expected {
		peer := pool.GetNextPeer()
		if peer != exp {
			t.Errorf("Iteration %d: expected %s, got %s", i, exp.URL, peer.URL)
		}
	}
}

func TestSkipDeadBackend(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")
	u3, _ := url.Parse("http://localhost:8083")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)
	b3 := NewBackend(u3)

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)
	pool.AddBackend(b3)

	// Mark Backend 2 as DOWN
	b2.SetAlive(false)

	// Verify that b2 is skipped in selection
	for i := 0; i < 4; i++ {
		peer := pool.GetNextPeer()
		if peer == b2 {
			t.Errorf("Iteration %d: dead backend b2 was unexpectedly selected", i)
		}
	}
}

// customFirstAliveStrategy is a mock strategy to verify pluggable strategies
type customFirstAliveStrategy struct{}

func (c *customFirstAliveStrategy) Name() string { return "FirstAlive" }
func (c *customFirstAliveStrategy) SelectNextBackend(peers []*Backend) *Backend {
	for _, p := range peers {
		if p.IsAlive() {
			return p
		}
	}
	return nil
}

func TestDynamicStrategySwitching(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)

	if pool.GetStrategy().Name() != "Round-Robin" {
		t.Errorf("expected Round-Robin, got %s", pool.GetStrategy().Name())
	}

	// Switch strategy dynamically to custom strategy
	pool.SetStrategy(&customFirstAliveStrategy{})
	if pool.GetStrategy().Name() != "FirstAlive" {
		t.Errorf("expected FirstAlive, got %s", pool.GetStrategy().Name())
	}

	// First alive should always return b1
	for i := 0; i < 3; i++ {
		peer := pool.GetNextPeer()
		if peer != b1 {
			t.Errorf("expected b1, got %v", peer)
		}
	}
}

func TestLeastConnectionsIntegration(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)

	b1.IncConns()
	b1.IncConns() // b1 has 2 conns
	b2.IncConns() // b2 has 1 conn

	pool := NewServerPool(strategy.NewLeastConnections[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)

	peer := pool.GetNextPeer()
	if peer != b2 {
		t.Errorf("expected b2 (least loaded with 1 conn), got %s", peer.URL)
	}

	// b2 finishes request
	b2.DecConns() // b2 has 0 conns
	peer = pool.GetNextPeer()
	if peer != b2 {
		t.Errorf("expected b2 (0 conns), got %s", peer.URL)
	}
}

func TestSmoothWeightedRoundRobinIntegration(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")

	b1 := NewBackendWithWeight(u1, 3)
	b2 := NewBackendWithWeight(u2, 1)

	pool := NewServerPool(strategy.NewSmoothWeightedRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)

	counts := make(map[string]int)
	for i := 0; i < 4; i++ {
		peer := pool.GetNextPeer()
		counts[peer.URL.String()]++
	}

	if counts[u1.String()] != 3 || counts[u2.String()] != 1 {
		t.Errorf("expected ratio 3:1, got %v", counts)
	}
}

func TestConsistentHashIntegration(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)

	pool := NewServerPool(strategy.NewConsistentHash[*Backend](100))
	pool.AddBackend(b1)
	pool.AddBackend(b2)

	clientIP := "203.0.113.195"
	target := pool.GetNextPeerWithKey(clientIP)

	for i := 0; i < 20; i++ {
		repeat := pool.GetNextPeerWithKey(clientIP)
		if repeat != target {
			t.Fatalf("iteration %d: IP affinity failed, expected %s, got %s", i, target.URL, repeat.URL)
		}
	}
}

func TestConnectionCount_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	b := NewBackend(u)
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b)

	handler := lbHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if conns := b.GetActiveConnections(); conns != 0 {
		t.Errorf("expected 0 active connections after success, got %d", conns)
	}
	if reqs := b.GetTotalRequests(); reqs != 1 {
		t.Errorf("expected 1 total request, got %d", reqs)
	}
}

func TestConnectionCount_HttpError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	b := NewBackend(u)
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b)

	handler := lbHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if conns := b.GetActiveConnections(); conns != 0 {
		t.Errorf("expected 0 active connections after http error, got %d", conns)
	}
	if reqs := b.GetTotalRequests(); reqs != 1 {
		t.Errorf("expected 1 total request, got %d", reqs)
	}
}

func TestConnectionCount_ProxyFailure(t *testing.T) {
	// Point to an unroutable port where no server is listening
	u, _ := url.Parse("http://127.0.0.1:59999")
	b := NewBackend(u)
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b)

	handler := lbHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if conns := b.GetActiveConnections(); conns != 0 {
		t.Errorf("expected 0 active connections after proxy failure, got %d", conns)
	}
	if b.IsAlive() {
		t.Errorf("expected backend to be marked dead after proxy failure")
	}
}

func TestConnectionCount_ContextCancellation(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	b := NewBackend(u)
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b)

	handler := lbHandler(pool)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/test", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	<-started
	// Context cancelled while in-flight
	cancel()
	<-done

	if conns := b.GetActiveConnections(); conns != 0 {
		t.Errorf("expected 0 active connections after context cancellation, got %d", conns)
	}
}

func TestConcurrentRequests_ConnectionBalance(t *testing.T) {
	const numServers = 3
	const totalRequests = 60

	servers := make([]*httptest.Server, numServers)
	backends := make([]*Backend, numServers)
	for i := 0; i < numServers; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("srv-%d", idx)))
		}))
		defer servers[i].Close()
		u, _ := url.Parse(servers[i].URL)
		backends[i] = NewBackendNamedWithWeight(u, fmt.Sprintf("srv-%d", idx), 1)
	}

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	for _, b := range backends {
		pool.AddBackend(b)
	}

	handler := lbHandler(pool)

	var wg sync.WaitGroup
	wg.Add(totalRequests)
	for i := 0; i < totalRequests; i++ {
		go func(reqNum int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/bench-%d", reqNum), nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d failed with status %d", reqNum, rec.Code)
			}
		}(i)
	}
	wg.Wait()

	var totalHandled uint64
	for i, b := range backends {
		if conns := b.GetActiveConnections(); conns != 0 {
			t.Errorf("backend %d leaked connections: active = %d", i, conns)
		}
		totalHandled += b.GetTotalRequests()
	}
	if totalHandled != uint64(totalRequests) {
		t.Errorf("expected %d total requests handled, got %d", totalRequests, totalHandled)
	}
}

func TestDynamicBackendRemovalAndRecovery(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")
	u3, _ := url.Parse("http://localhost:8083")

	b1 := NewBackendNamedWithWeight(u1, "B1", 1)
	b2 := NewBackendNamedWithWeight(u2, "B2", 1)
	b3 := NewBackendNamedWithWeight(u3, "B3", 1)

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)
	pool.AddBackend(b3)

	if len(pool.GetAllBackends()) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(pool.GetAllBackends()))
	}

	// Remove B2
	removed := pool.RemoveBackend("B2")
	if !removed {
		t.Fatalf("failed to remove B2")
	}
	if len(pool.GetAllBackends()) != 2 {
		t.Fatalf("expected 2 backends after removal, got %d", len(pool.GetAllBackends()))
	}

	// Verify B2 is never returned
	for i := 0; i < 10; i++ {
		peer := pool.GetNextPeer()
		if peer.Name == "B2" {
			t.Fatalf("removed backend B2 was selected")
		}
	}

	// Recover / re-add B2
	pool.AddBackend(b2)
	if len(pool.GetAllBackends()) != 3 {
		t.Fatalf("expected 3 backends after recovery, got %d", len(pool.GetAllBackends()))
	}

	b2Found := false
	for i := 0; i < 10; i++ {
		if pool.GetNextPeer().Name == "B2" {
			b2Found = true
			break
		}
	}
	if !b2Found {
		t.Errorf("recovered backend B2 was never selected in 10 iterations")
	}
}

func TestConcurrentStrategySwitching(t *testing.T) {
	u1, _ := url.Parse("http://localhost:8081")
	u2, _ := url.Parse("http://localhost:8082")

	b1 := NewBackend(u1)
	b2 := NewBackend(u2)

	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(b1)
	pool.AddBackend(b2)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Swapper goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		strategies := []string{"Round-Robin", "Least-Connections", "Smooth-Weighted-Round-Robin", "Consistent-Hash"}
		idx := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = pool.SetStrategyByName(strategies[idx%len(strategies)])
				idx++
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Reader goroutines
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				peer := pool.GetNextPeerWithKey(fmt.Sprintf("192.168.1.%d", workerID))
				if peer == nil {
					t.Errorf("expected non-nil peer")
				}
			}
		}(i)
	}

	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

