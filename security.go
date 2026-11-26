package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// SecurityConfig hardens the management API (/api/*). The proxied traffic path is not affected.
//
// Threat model: the API is meant for the local operator and local tooling (dashboard, load generator,
// the Python agent). It must not be reachable or drivable by (a) other machines, (b) a web page the
// operator happens to visit (CSRF / DNS rebinding against localhost), or (c) oversized / malformed bodies.
//
//   - Mutating requests (POST) need EITHER a bearer token (LB_ADMIN_TOKEN) OR a loopback client address.
//   - Every /api request must carry an allowed Host (localhost / 127.0.0.1 / [::1] / LB_ALLOWED_HOSTS):
//     this defeats DNS rebinding. A cross-site Origin is refused unless allow-listed (LB_CORS_ORIGINS).
//   - POST bodies must be Content-Type: application/json (forces a CORS preflight for browsers), at most
//     4 KiB, one JSON object, with only the keys the endpoint defines.
//   - SSE subscribers are capped and client addresses in the event stream are masked (privacy).
type SecurityConfig struct {
	AdminToken     string
	AllowedOrigins map[string]bool
	AllowedHosts   map[string]bool
	MaxBodyBytes   int64
	MaxSSE         int
	MaskClientIP   bool
	LogPath        bool // include the request path in the per-request log line
}

const defaultMaxBodyBytes = 4096

// SecurityFromEnv builds the configuration from LB_* variables with secure defaults.
func SecurityFromEnv() *SecurityConfig {
	s := &SecurityConfig{
		AdminToken:     os.Getenv("LB_ADMIN_TOKEN"),
		AllowedOrigins: map[string]bool{},
		AllowedHosts:   map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true},
		MaxBodyBytes:   defaultMaxBodyBytes,
		MaxSSE:         32,
		MaskClientIP:   os.Getenv("LB_EXPOSE_CLIENT_IP") != "1",
		LogPath:        os.Getenv("LB_LOG_REQUEST_PATH") == "1",
	}
	origins := os.Getenv("LB_CORS_ORIGINS")
	if origins == "" {
		origins = "http://localhost:5173,http://127.0.0.1:5173" // the Vite dev server
	}
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			s.AllowedOrigins[strings.ToLower(o)] = true
		}
	}
	for _, h := range strings.Split(os.Getenv("LB_ALLOWED_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.AllowedHosts[h] = true
		}
	}
	return s
}

func hostOnly(hostport string) string {
	h := hostport
	if x, _, err := net.SplitHostPort(hostport); err == nil {
		h = x
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func isLoopbackAddr(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		h = remote
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

func deny(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// originAllowed: same-origin (Origin host equals the request Host) or explicitly allow-listed.
func (s *SecurityConfig) originAllowed(r *http.Request, origin string) bool {
	o := strings.ToLower(origin)
	if s.AllowedOrigins[o] {
		return true
	}
	return o == "http://"+strings.ToLower(r.Host) || o == "https://"+strings.ToLower(r.Host)
}

// apiBodyKeys lists the keys each POST endpoint accepts. Anything else is rejected (strict decoding).
var apiBodyKeys = map[string][]string{
	"/api/strategy":        {"strategy"},
	"/api/backends/toggle": {"name"},
	"/api/backends/weight": {"name", "weight"},
	"/api/policy":          {"action", "decision_id"},
	"/api/send-request":    {"client_ip", "delay"},
	"/api/events/marker":   {"kind", "backend", "detail"},
	"/api/agent/kill":      {"kill", "baseline_action"},
	"/api/agent/reset":     {"archive"},
}

// guardBody enforces size, single-JSON-object and allowed-keys for POST bodies, then restores the body.
func guardBody(w http.ResponseWriter, r *http.Request, max int64) bool {
	allowed, known := apiBodyKeys[r.URL.Path]
	if !known {
		return true // unknown path: the router answers 404
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			deny(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			deny(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	if len(bytes.TrimSpace(data)) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil || obj == nil { // rejects NaN/Infinity, trailing data, non-objects
			deny(w, http.StatusBadRequest, "invalid request body: expected a single JSON object")
			return false
		}
		ok := map[string]bool{}
		for _, k := range allowed {
			ok[k] = true
		}
		for k := range obj {
			if !ok[k] {
				deny(w, http.StatusBadRequest, "invalid request body: unknown field \""+k+"\"")
				return false
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

// checkAPI applies the host / origin / method / content-type / auth rules to an /api request.
// It writes the error response itself and returns false when the request must not proceed.
func (s *SecurityConfig) checkAPI(w http.ResponseWriter, r *http.Request) bool {
	if !s.AllowedHosts[hostOnly(r.Host)] {
		deny(w, http.StatusForbidden, "host not allowed")
		return false
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		if !s.originAllowed(r, origin) {
			deny(w, http.StatusForbidden, "origin not allowed")
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if r.Method != http.MethodPost {
		deny(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	// authentication: bearer token if configured, otherwise loopback clients only
	if s.AdminToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(got), []byte(s.AdminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			deny(w, http.StatusUnauthorized, "admin token required")
			return false
		}
	} else if !isLoopbackAddr(r.RemoteAddr) {
		deny(w, http.StatusForbidden, "management API is loopback-only (set LB_ADMIN_TOKEN to allow remote administration)")
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if ct != "application/json" {
		deny(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	return guardBody(w, r, s.MaxBodyBytes)
}

// preflight answers CORS preflight requests: allowed origins only, nothing else.
func (s *SecurityConfig) preflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.AllowedHosts[hostOnly(r.Host)] || !s.originAllowed(r, origin) {
		deny(w, http.StatusForbidden, "origin not allowed")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// maskClientIP hides the host part of a client address in the event stream (IPv4 a.b.c.0, IPv6 first
// three groups); anything that is not an IP address is replaced by "unknown". Routing is unaffected.
func maskClientIP(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String()
	}
	m := make(net.IP, 16)
	copy(m[:6], ip[:6])
	return m.String()
}

// displayIP is the client address as shown in events.
func (s *SecurityConfig) displayIP(raw string) string {
	if s == nil || !s.MaskClientIP {
		return raw
	}
	return maskClientIP(raw)
}

// newHTTPServer builds the LB server with hardened timeouts. No WriteTimeout: /api/events is a long-lived
// SSE stream; slow clients are bounded by the SSE cap, header/idle timeouts and the proxy's own deadlines.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
