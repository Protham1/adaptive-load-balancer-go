package main

import (
	"adaptive-load/strategy"
	"net/url"
	"testing"
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

