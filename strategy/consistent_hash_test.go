package strategy

import (
	"fmt"
	"testing"
)

type mockHashPeer struct {
	id    string
	alive bool
}

func (m *mockHashPeer) IsAlive() bool {
	return m.alive
}

func (m *mockHashPeer) GetID() string {
	return m.id
}

func TestConsistentHashAffinity(t *testing.T) {
	p1 := &mockHashPeer{id: "server-1", alive: true}
	p2 := &mockHashPeer{id: "server-2", alive: true}
	p3 := &mockHashPeer{id: "server-3", alive: true}

	peers := []*mockHashPeer{p1, p2, p3}
	ch := NewConsistentHash[*mockHashPeer](150)

	if ch.Name() != "Consistent-Hash" {
		t.Errorf("expected name Consistent-Hash, got %s", ch.Name())
	}

	// Repeated requests with the exact same client IP must always hit the exact same server
	clientIP := "192.168.1.105"
	initialTarget := ch.SelectNextBackendWithKey(peers, clientIP)
	if initialTarget == nil {
		t.Fatal("expected non-nil peer")
	}

	for i := 0; i < 50; i++ {
		target := ch.SelectNextBackendWithKey(peers, clientIP)
		if target != initialTarget {
			t.Fatalf("iteration %d: affinity broken! expected %s, got %s", i, initialTarget.id, target.id)
		}
	}
}

func TestConsistentHashDistribution(t *testing.T) {
	p1 := &mockHashPeer{id: "server-1", alive: true}
	p2 := &mockHashPeer{id: "server-2", alive: true}
	p3 := &mockHashPeer{id: "server-3", alive: true}

	peers := []*mockHashPeer{p1, p2, p3}
	ch := NewConsistentHash[*mockHashPeer](100)

	hitCounts := make(map[string]int)
	for i := 0; i < 300; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		peer := ch.SelectNextBackendWithKey(peers, ip)
		if peer == nil {
			t.Fatal("unexpected nil peer")
		}
		hitCounts[peer.id]++
	}

	// All 3 servers should receive traffic
	for _, p := range peers {
		if hitCounts[p.id] == 0 {
			t.Errorf("server %s received 0 hits in distribution test", p.id)
		}
	}
}

func TestConsistentHashDeadPeerFailover(t *testing.T) {
	p1 := &mockHashPeer{id: "server-1", alive: true}
	p2 := &mockHashPeer{id: "server-2", alive: true}
	p3 := &mockHashPeer{id: "server-3", alive: true}

	peers := []*mockHashPeer{p1, p2, p3}
	ch := NewConsistentHash[*mockHashPeer](100)

	clientIP := "192.168.1.50"
	initialTarget := ch.SelectNextBackendWithKey(peers, clientIP)

	// Kill the assigned server
	initialTarget.alive = false

	// Next request for the same key should fail over to another alive server
	failoverTarget := ch.SelectNextBackendWithKey(peers, clientIP)
	if failoverTarget == nil {
		t.Fatal("expected failover to an alive peer, got nil")
	}
	if failoverTarget == initialTarget {
		t.Fatalf("selected dead peer %s during failover", initialTarget.id)
	}

	// Restore initial server
	initialTarget.alive = true
	restoredTarget := ch.SelectNextBackendWithKey(peers, clientIP)
	if restoredTarget != initialTarget {
		t.Fatalf("expected restored affinity to %s, got %s", initialTarget.id, restoredTarget.id)
	}
}

func TestConsistentHashFallbackNoKey(t *testing.T) {
	p1 := &mockHashPeer{id: "server-1", alive: true}
	p2 := &mockHashPeer{id: "server-2", alive: true}

	peers := []*mockHashPeer{p1, p2}
	ch := NewConsistentHash[*mockHashPeer](50)

	peer := ch.SelectNextBackend(peers)
	if peer == nil {
		t.Fatal("expected non-nil peer when calling SelectNextBackend without key")
	}
}

func TestConsistentHashEmptyOrAllDead(t *testing.T) {
	ch := NewConsistentHash[*mockHashPeer](50)

	if res := ch.SelectNextBackendWithKey([]*mockHashPeer{}, "test-key"); res != nil {
		t.Errorf("expected nil for empty peers, got %v", res)
	}

	p1 := &mockHashPeer{id: "server-1", alive: false}
	if res := ch.SelectNextBackendWithKey([]*mockHashPeer{p1}, "test-key"); res != nil {
		t.Errorf("expected nil for all dead peers, got %v", res)
	}
}
