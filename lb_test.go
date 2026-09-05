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

