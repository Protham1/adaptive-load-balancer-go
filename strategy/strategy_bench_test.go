package strategy

import (
	"fmt"
	"testing"
)

func BenchmarkStrategy_RoundRobin(b *testing.B) {
	peers := []*mockConcurrentPeer{
		{id: "p1", alive: true},
		{id: "p2", alive: true},
		{id: "p3", alive: true},
	}
	rr := NewRoundRobin[*mockConcurrentPeer]()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = rr.SelectNextBackend(peers)
		}
	})
}

func BenchmarkStrategy_LeastConnections(b *testing.B) {
	peers := []*mockConcurrentPeer{
		{id: "p1", alive: true, conns: 2},
		{id: "p2", alive: true, conns: 5},
		{id: "p3", alive: true, conns: 1},
	}
	lc := NewLeastConnections[*mockConcurrentPeer]()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = lc.SelectNextBackend(peers)
		}
	})
}

func BenchmarkStrategy_SmoothWeightedRoundRobin(b *testing.B) {
	peers := []*mockConcurrentPeer{
		{id: "p1", alive: true, w: 5},
		{id: "p2", alive: true, w: 2},
		{id: "p3", alive: true, w: 1},
	}
	swrr := NewSmoothWeightedRoundRobin[*mockConcurrentPeer]()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = swrr.SelectNextBackend(peers)
		}
	})
}

func BenchmarkStrategy_ConsistentHash(b *testing.B) {
	peers := []*mockConcurrentPeer{
		{id: "p1", alive: true},
		{id: "p2", alive: true},
		{id: "p3", alive: true},
	}
	ch := NewConsistentHash[*mockConcurrentPeer](100)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			key := fmt.Sprintf("192.168.1.%d", i%256)
			_ = ch.SelectNextBackendWithKey(peers, key)
		}
	})
}
