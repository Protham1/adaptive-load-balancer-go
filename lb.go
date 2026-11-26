package main

import (
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var reqCounter uint64

// maxBackendWeight bounds an operator-set capacity weight (negative, zero and absurd weights are rejected).
const maxBackendWeight = 1000

// quietRequestLog (LB_QUIET=1) suppresses the per-request log line so that
// high-rate experiments do not spend CPU on logging.
var quietRequestLog = os.Getenv("LB_QUIET") == "1"

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

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten uint64
}

func (s *statusCapturingResponseWriter) WriteHeader(code int) {
	s.statusCode = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if s.statusCode == 0 {
		s.statusCode = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytesWritten += uint64(n)
	return n, err
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
		sec := pool.Security()
		if sec == nil {
			enableCORS(w) // unhardened mode (unit tests only); main always installs a SecurityConfig
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
		} else if r.Method == http.MethodOptions {
			sec.preflight(w, r)
			return
		}

		// API Routing
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if sec != nil && !sec.checkAPI(w, r) {
				return
			}
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

		if !quietRequestLog {
			// the request path can carry identifiers: it is only logged when explicitly enabled
			if sec != nil && sec.LogPath {
				log.Printf("[LoadBalancer] [%s] Forwarding %s %s -> %s (active: %d)", reqID, r.Method, r.URL.Path, peer.Name, activeConns)
			} else {
				log.Printf("[LoadBalancer] [%s] Forwarding %s -> %s (active: %d)", reqID, r.Method, peer.Name, activeConns)
			}
		}

		// Broadcast start event
		eventBus.Publish(RequestEvent{
			ID:          reqID,
			Type:        "start",
			ClientIP:    sec.displayIP(clientIP),
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: activeConns,
			Timestamp:   time.Now().UnixMilli(),
		})

		isCompute := r.Header.Get("X-Simulated-Delay") != "" || r.URL.Query().Get("delay") != "" || strings.Contains(r.URL.Path, "compute")
		col := pool.GetCollector()
		var activeReq *metrics.ActiveRequest
		if col != nil {
			activeReq = col.RecordRequestStart(peer.Name, r.Method, r.URL.Path, isCompute)
		}

		inBytes := uint64(0)
		if r.ContentLength > 0 {
			inBytes = uint64(r.ContentLength)
		}
		capturedWriter := &statusCapturingResponseWriter{ResponseWriter: w}

		start := time.Now()
		defer func() {
			conns := peer.DecConns()
			duration := time.Since(start).Milliseconds()

			status := capturedWriter.statusCode
			if status == 0 {
				status = http.StatusOK
			}
			var reqErr error
			if status >= 500 {
				reqErr = fmt.Errorf("http error %d", status)
			}
			if col != nil {
				col.RecordRequestEnd(peer.Name, activeReq, status, reqErr, inBytes, capturedWriter.bytesWritten, isCompute)
			}

			// Broadcast complete event
			eventBus.Publish(RequestEvent{
				ID:          reqID,
				Type:        "complete",
				ClientIP:    sec.displayIP(clientIP),
				BackendURL:  peer.URL.String(),
				BackendName: peer.Name,
				Strategy:    stratName,
				ActiveConns: conns,
				DurationMs:  duration,
				Status:      status,
				Timestamp:   time.Now().UnixMilli(),
			})
		}()

		if col != nil {
			col.MarkProxyStart(activeReq)
		}
		peer.ReverseProxy.ServeHTTP(capturedWriter, r)
	}
}

// handleAPI routes internal management and telemetry requests
func handleAPI(w http.ResponseWriter, r *http.Request, pool *ServerPool, eventBus *EventBus) {
	sec := pool.Security()
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
		pool.Events().Add("health", b.Name, map[bool]string{true: "UP (manual toggle)", false: "DOWN (manual toggle)"}[newStatus])
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
		if body.Weight < 1 || body.Weight > maxBackendWeight {
			http.Error(w, fmt.Sprintf("weight must be an integer in [1,%d]", maxBackendWeight), http.StatusBadRequest)
			return
		}
		b.SetWeight(body.Weight)
		log.Printf("[LoadBalancer] Updated %s weight -> %d", b.Name, b.GetWeight())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": b.Name, "weight": b.GetWeight()})

	case r.URL.Path == "/api/metrics" && r.Method == http.MethodGet:
		col := pool.GetCollector()
		if col == nil {
			http.Error(w, `{"error":"metrics collector not configured"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(col.CollectSnapshot())

	case r.URL.Path == "/api/policy" && r.Method == http.MethodGet:
		cons := pool.Constraints()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"policy":      pool.Policies().Current(),
			"actions":     policy.Describe(cons),
			"action_mask": policy.ActionMask(cons),
		})

	case r.URL.Path == "/api/policy" && r.Method == http.MethodPost:
		var body struct {
			Action     *int   `json:"action"`
			DecisionID string `json:"decision_id"` // optional: pairs the action with a learner decision
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Action == nil {
			http.Error(w, `invalid request body: expected {"action": <0..4>}`, http.StatusBadRequest)
			return
		}
		if err := pool.ApplyAction(policy.ActionID(*body.Action)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[LoadBalancer] Applied action %d (decision %q) -> policy v%d", *body.Action, body.DecisionID, pool.Policies().Current().Version)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "decision_id": body.DecisionID, "policy": pool.Policies().Current()})

	case r.URL.Path == "/api/dashboard" && r.Method == http.MethodGet:
		col := pool.GetCollector()
		if col == nil {
			http.Error(w, `{"error":"metrics collector not configured"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(buildDashboard(pool, col.CollectSnapshot(), time.Now()))

	case r.URL.Path == "/api/history" && r.Method == http.MethodGet:
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"points": pool.History().Since(since), "interval_ms": 2000})

	case r.URL.Path == "/api/events/log" && r.Method == http.MethodGet:
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		evs, last := pool.Events().Since(since)
		if evs == nil {
			evs = []Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": evs, "last_id": last})

	case r.URL.Path == "/api/events/marker" && r.Method == http.MethodPost:
		var body struct {
			Kind    string `json:"kind"`
			Backend string `json:"backend"`
			Detail  string `json:"detail"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !markerKinds[body.Kind] {
			http.Error(w, `invalid marker: expected {"kind":"fault|note|experiment","backend":"","detail":""}`, http.StatusBadRequest)
			return
		}
		if len(body.Backend) > 64 || len(body.Detail) > maxEventDetail {
			http.Error(w, "backend or detail too long", http.StatusBadRequest)
			return
		}
		if body.Backend != "" && pool.GetBackendByName(body.Backend) == nil {
			http.Error(w, "unknown backend", http.StatusBadRequest)
			return
		}
		e := pool.Events().Add(body.Kind, body.Backend, body.Detail)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e)

	case r.URL.Path == "/api/agent/learner" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		ag := pool.GetAgent()
		if ag == nil {
			_, _ = w.Write([]byte(`{"available":false,"error":"agent not configured"}`))
			return
		}
		in, ok := ag.introspector()
		if !ok {
			_, _ = w.Write([]byte(`{"available":false,"error":"agent does not support introspection"}`))
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		data, err := in.Learner(r.Context(), limit)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"available": false, "error": err.Error()})
			return
		}
		_, _ = w.Write(data)

	case r.URL.Path == "/api/agent/reset" && r.Method == http.MethodPost:
		ag := pool.GetAgent()
		if ag == nil {
			http.Error(w, `{"error":"agent not configured"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Archive *bool `json:"archive"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
		archive := body.Archive == nil || *body.Archive
		now := time.Now()
		if err := ag.mgr.ResetToBaseline(now); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ag.ResetPending(now)
		out := map[string]any{"status": "ok", "policy": pool.Policies().Current(), "model_reset": false}
		if in, ok := ag.introspector(); ok {
			if data, err := in.Reset(r.Context(), archive, "reset-to-baseline"); err != nil {
				out["agent_error"] = err.Error() // the policy is already at baseline; report that the model was not reset
			} else {
				out["model_reset"] = true
				var raw any
				_ = json.Unmarshal(data, &raw)
				out["agent"] = raw
			}
		}
		log.Printf("[LoadBalancer] Reset to baseline (model_reset=%v)", out["model_reset"])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)

	case r.URL.Path == "/api/agent" && r.Method == http.MethodGet:
		ag := pool.GetAgent()
		w.Header().Set("Content-Type", "application/json")
		if ag == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
			return
		}
		_ = json.NewEncoder(w).Encode(ag.Status())

	case r.URL.Path == "/api/agent/kill" && r.Method == http.MethodPost:
		ag := pool.GetAgent()
		if ag == nil {
			http.Error(w, `{"error":"agent not configured"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Kill           *bool `json:"kill"`
			BaselineAction *int  `json:"baseline_action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kill == nil {
			http.Error(w, `invalid request body: expected {"kill": true|false, "baseline_action": <0..4, optional>}`, http.StatusBadRequest)
			return
		}
		var base *policy.ActionID
		if body.BaselineAction != nil {
			a := policy.ActionID(*body.BaselineAction)
			base = &a
		}
		if err := ag.mgr.Kill(*body.Kill, base, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[LoadBalancer] AI kill switch set to %v", *body.Kill)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "killed": ag.mgr.IsKilled(), "policy": pool.Policies().Current()})

	case r.URL.Path == "/api/controller" && r.Method == http.MethodGet:
		ctrl := pool.GetController()
		if ctrl == nil {
			http.Error(w, `{"error":"rule-based controller not configured"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": ctrl.Config(),
			"last":   ctrl.Last(),
			"policy": pool.Policies().Current(),
			"shadow": pool.Policies().Current().Strategy != policy.AdaptiveSWRR,
		})

	case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
		col := pool.GetCollector()
		if col == nil {
			http.Error(w, `{"error":"metrics collector not configured"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(col.GetStateRepresentation())

	case r.URL.Path == "/api/state/schema" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(metrics.GetStateSchema(metrics.CurrentStateVersion))

	case r.URL.Path == "/api/events" && r.Method == http.MethodGet:
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		maxSSE := 0
		if s := pool.Security(); s != nil {
			maxSSE = s.MaxSSE
		}
		ch, ok := eventBus.TrySubscribe(maxSSE)
		if !ok {
			http.Error(w, "too many event stream subscribers", http.StatusServiceUnavailable)
			return
		}
		defer eventBus.Unsubscribe(ch)

		// Send initial heartbeat
		fmt.Fprintf(w, ": heartbeat\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case data, open := <-ch:
				if !open { // the bus was closed (shutdown)
					return
				}
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
			ClientIP:    sec.displayIP(clientIP),
			BackendURL:  peer.URL.String(),
			BackendName: peer.Name,
			Strategy:    stratName,
			ActiveConns: activeConns,
			Timestamp:   time.Now().UnixMilli(),
		})

		// If custom delay requested, forward to backend with X-Simulated-Delay
		targetURL := peer.URL.String()
		if body.Delay != "" {
			targetURL += "/?delay=" + body.Delay
		}

		isCompute := body.Delay != ""
		col := pool.GetCollector()
		var activeReq *metrics.ActiveRequest
		if col != nil {
			activeReq = col.RecordRequestStart(peer.Name, http.MethodGet, targetURL, isCompute)
		}

		start := time.Now()
		respStatusCode := http.StatusOK
		var respErr error
		bytesWritten := uint64(0)

		defer func() {
			conns := peer.DecConns()
			if col != nil {
				col.RecordRequestEnd(peer.Name, activeReq, respStatusCode, respErr, 0, bytesWritten, isCompute)
			}
			eventBus.Publish(RequestEvent{
				ID:          reqID,
				Type:        "complete",
				ClientIP:    sec.displayIP(clientIP),
				BackendURL:  peer.URL.String(),
				BackendName: peer.Name,
				Strategy:    stratName,
				ActiveConns: conns,
				DurationMs:  time.Since(start).Milliseconds(),
				Status:      respStatusCode,
				Timestamp:   time.Now().UnixMilli(),
			})
		}()

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
		if err == nil {
			if body.Delay != "" {
				req.Header.Set("X-Simulated-Delay", body.Delay)
			}
			if col != nil {
				col.MarkProxyStart(activeReq)
			}
			resp, doErr := http.DefaultClient.Do(req)
			if doErr != nil {
				respErr = doErr
				respStatusCode = http.StatusBadGateway
			} else {
				respStatusCode = resp.StatusCode
				_ = resp.Body.Close()
			}
		} else {
			respErr = err
			respStatusCode = http.StatusBadRequest
		}

		duration := time.Since(start).Milliseconds()
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
