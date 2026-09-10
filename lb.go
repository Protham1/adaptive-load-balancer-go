package main

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// getClientIP extracts the client IP address from proxy headers or remote address
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// lbHandler creates an HTTP handler func that balances load across the server pool
func lbHandler(pool *ServerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		peer := pool.GetNextPeerWithKey(clientIP)
		if peer != nil {
			peer.IncConns()
			defer peer.DecConns()

			log.Printf("[LoadBalancer] Forwarding %s %s -> %s (active conns: %d)",
				r.Method, r.URL.Path, peer.URL, peer.GetActiveConnections())
			peer.ReverseProxy.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Service Unavailable: No healthy backends available", http.StatusServiceUnavailable)
	}
}
