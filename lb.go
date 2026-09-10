package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var reqCounter uint64

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

// enableCORS sets CORS headers for frontend communication
func enableCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Forwarded-For, X-Simulated-Delay, X-Real-IP")
}

// statusResponse represents the server pool status
type statusResponse struct {
	ActiveStrategy string           `json:"active_strategy"`
	Backends       []backendPayload `json:"backends"`
}

type backendPayload struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Alive         bool   `json:"alive"`
	Weight        int    `json:"weight"`
	ActiveConns   int64  `json:"active_conns"`
	TotalRequests uint64 `json:"total_requests"`
}

// lbHandler creates an HTTP handler func that balances load and exposes management/SSE APIs
func lbHandler(pool *ServerPool, bus ...*EventBus) http.HandlerFunc {
	var eventBus *EventBus
	if len(bus) > 0 && bus[0] != nil {
		eventBus = bus[0]
	} else {
		eventBus = NewEventBus()
	}

	distDir := filepath.Join(".", "frontend", "dist")
	var fileServer http.Handler
	if _, err := os.Stat(distDir); err == nil {
		fileServer = http.FileServer(http.Dir(distDir))
	}

	return func(w http.ResponseWriter, r *http.Request) {
		enableCORS(w)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// API Routing
		if strings.HasPrefix(r.URL.Path, "/api/") {
			handleAPI(w, r, pool, eventBus)
			return
		}

		// If frontend dist is built and request is for static asset, serve frontend
		if fileServer != nil && (r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/vite.svg") {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Normal Reverse Proxy Balancing
		clientIP := getClientIP(r)
		peer := pool.GetNextPeerWithKey(clientIP)
		if peer == nil {
			http.Error(w, "Service Unavailable: No healthy backends available", http.StatusServiceUnavailable)
			return
		}

		reqID := fmt.Sprintf("req-%d", atomic.AddUint64(&reqCounter, 1))
		activeConns := peer.IncConns()
		peer.IncTotalRequests()
		stratName := pool.GetStrategy().Name()

		log.Printf("[LoadBalancer] [%s] Forwarding %s %s -> %s (%s) (active: %d)",
			reqID, r.Method, r.URL.Path, peer.Name, peer.URL, activeConns)

		// Broadcast start event
		eventBus.Publish(RequestEvent{
			ID:          reqID,
			Type:        "start",
			ClientIP:    clientIP,
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: activeConns,
			Timestamp:   time.Now().UnixMilli(),
		})

		start := time.Now()
		peer.ReverseProxy.ServeHTTP(w, r)
		duration := time.Since(start).Milliseconds()
		peer.DecConns()

		// Broadcast complete event
		eventBus.Publish(RequestEvent{
			ID:          reqID,
			Type:        "complete",
			ClientIP:    clientIP,
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: peer.GetActiveConnections(),
			DurationMs:  duration,
			Status:      http.StatusOK,
			Timestamp:   time.Now().UnixMilli(),
		})
	}
}

// handleAPI routes internal management and telemetry requests
func handleAPI(w http.ResponseWriter, r *http.Request, pool *ServerPool, eventBus *EventBus) {
	switch {
	case r.URL.Path == "/api/status" && r.Method == http.MethodGet:
		backends := pool.GetAllBackends()
		payloads := make([]backendPayload, len(backends))
		for i, b := range backends {
			payloads[i] = backendPayload{
				Name:          b.Name,
				URL:           b.URL.String(),
				Alive:         b.IsAlive(),
				Weight:        b.GetWeight(),
				ActiveConns:   b.GetActiveConnections(),
				TotalRequests: b.GetTotalRequests(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(statusResponse{
			ActiveStrategy: pool.GetStrategy().Name(),
			Backends:       payloads,
		})

	case r.URL.Path == "/api/strategy" && r.Method == http.MethodPost:
		var body struct {
			Strategy string `json:"strategy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := pool.SetStrategyByName(body.Strategy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[LoadBalancer] Switched strategy to: %s", body.Strategy)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "strategy": pool.GetStrategy().Name()})

	case r.URL.Path == "/api/backends/toggle" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		b := pool.GetBackendByName(body.Name)
		if b == nil {
			http.Error(w, "backend not found", http.StatusNotFound)
			return
		}
		newStatus := !b.IsAlive()
		b.SetAlive(newStatus)
		log.Printf("[LoadBalancer] Toggled %s alive -> %v", b.Name, newStatus)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": b.Name, "alive": newStatus})

	case r.URL.Path == "/api/backends/weight" && r.Method == http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Weight int    `json:"weight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		b := pool.GetBackendByName(body.Name)
		if b == nil {
			http.Error(w, "backend not found", http.StatusNotFound)
			return
		}
		b.SetWeight(body.Weight)
		log.Printf("[LoadBalancer] Updated %s weight -> %d", b.Name, b.GetWeight())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": b.Name, "weight": b.GetWeight()})

	case r.URL.Path == "/api/events" && r.Method == http.MethodGet:
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := eventBus.Subscribe()
		defer eventBus.Unsubscribe(ch)

		// Send initial heartbeat
		fmt.Fprintf(w, ": heartbeat\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case data := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}

	case r.URL.Path == "/api/send-request" && r.Method == http.MethodPost:
		var body struct {
			ClientIP string `json:"client_ip"`
			Delay    string `json:"delay"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		clientIP := body.ClientIP
		if clientIP == "" {
			clientIP = getClientIP(r)
		}

		peer := pool.GetNextPeerWithKey(clientIP)
		if peer == nil {
			http.Error(w, "No healthy backends available", http.StatusServiceUnavailable)
			return
		}

		reqID := fmt.Sprintf("req-%d", atomic.AddUint64(&reqCounter, 1))
		activeConns := peer.IncConns()
		peer.IncTotalRequests()
		stratName := pool.GetStrategy().Name()

		eventBus.Publish(RequestEvent{
			ID:          reqID,
			Type:        "start",
			ClientIP:    clientIP,
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: activeConns,
			Timestamp:   time.Now().UnixMilli(),
		})

		start := time.Now()
		// If custom delay requested, forward to backend with X-Simulated-Delay
		targetURL := peer.URL.String()
		if body.Delay != "" {
			targetURL += "/?delay=" + body.Delay
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
		if err == nil {
			if body.Delay != "" {
				req.Header.Set("X-Simulated-Delay", body.Delay)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}

		duration := time.Since(start).Milliseconds()
		peer.DecConns()

		eventBus.Publish(RequestEvent{
			ID:          reqID,
			Type:        "complete",
			ClientIP:    clientIP,
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: peer.GetActiveConnections(),
			DurationMs:  duration,
			Status:      http.StatusOK,
			Timestamp:   time.Now().UnixMilli(),
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           reqID,
			"backend_name": peer.Name,
			"backend_url":  peer.URL.String(),
			"duration_ms":  duration,
			"strategy":     stratName,
		})

	default:
		http.NotFound(w, r)
	}
}
