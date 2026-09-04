package main

import (
	"log"
	"net/http"
)

// lbHandler creates an HTTP handler func that balances load across the server pool
func lbHandler(pool *ServerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer := pool.GetNextPeer()
		if peer != nil {
			log.Printf("[LoadBalancer] Forwarding %s %s -> %s", r.Method, r.URL.Path, peer.URL)
			peer.ReverseProxy.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Service Unavailable: No healthy backends available", http.StatusServiceUnavailable)
	}
}
