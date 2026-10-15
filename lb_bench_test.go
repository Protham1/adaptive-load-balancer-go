package main

import (
	"adaptive-load/strategy"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func createBenchmarkServerPool(strat strategy.SelectionStrategy[*Backend]) (*ServerPool, func()) {
	const numServers = 3
	servers := make([]*httptest.Server, numServers)
	backends := make([]*Backend, numServers)

	for i := 0; i < numServers; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("resp-%d", idx)))
		}))
		u, _ := url.Parse(servers[i].URL)
		backends[i] = NewBackendNamedWithWeight(u, fmt.Sprintf("srv-%d", idx), idx+1)
	}

	oldOutput := log.Writer()
	log.SetOutput(io.Discard)

	pool := NewServerPool(strat)
	for _, b := range backends {
		pool.AddBackend(b)
	}

	cleanup := func() {
		log.SetOutput(oldOutput)
		for _, s := range servers {
			s.Close()
		}
	}
	return pool, cleanup
}

func BenchmarkLoadBalancer_RoundRobin(b *testing.B) {
	pool, cleanup := createBenchmarkServerPool(strategy.NewRoundRobin[*Backend]())
	defer cleanup()

	handler := lbHandler(pool)
	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/bench", nil)
		for pb.Next() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}
	})
}

func BenchmarkLoadBalancer_LeastConnections(b *testing.B) {
	pool, cleanup := createBenchmarkServerPool(strategy.NewLeastConnections[*Backend]())
	defer cleanup()

	handler := lbHandler(pool)
	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/bench", nil)
		for pb.Next() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}
	})
}

func BenchmarkLoadBalancer_SmoothWeightedRoundRobin(b *testing.B) {
	pool, cleanup := createBenchmarkServerPool(strategy.NewSmoothWeightedRoundRobin[*Backend]())
	defer cleanup()

	handler := lbHandler(pool)
	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/bench", nil)
		for pb.Next() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}
	})
}

func BenchmarkLoadBalancer_ConsistentHash(b *testing.B) {
	pool, cleanup := createBenchmarkServerPool(strategy.NewConsistentHash[*Backend](100))
	defer cleanup()

	handler := lbHandler(pool)
	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			req := httptest.NewRequest(http.MethodGet, "/bench", nil)
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i%250))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}
	})
}
