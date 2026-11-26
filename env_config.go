package main

import (
	"adaptive-load/controller"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// backendSpec is one configured backend.
type backendSpec struct {
	Port   string
	Name   string
	Weight int
}

// defaultBackends is the standard 3:2:1 heterogeneous pool.
func defaultBackends() []backendSpec {
	return []backendSpec{
		{Port: "8081", Name: "Backend-1", Weight: 3},
		{Port: "8082", Name: "Backend-2", Weight: 2},
		{Port: "8083", Name: "Backend-3", Weight: 1},
	}
}

// ParseBackends parses "Name:port:weight,Name:port:weight". Names must be unique
// and use only the identifier characters the Go/Python contract allows.
func ParseBackends(s string) ([]backendSpec, error) {
	var out []backendSpec
	seenName, seenPort := map[string]bool{}, map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 {
			return nil, fmt.Errorf("backend %q: expected Name:port:weight", part)
		}
		w, err := strconv.Atoi(f[2])
		if err != nil || w < 1 || w > 1000 {
			return nil, fmt.Errorf("backend %q: weight must be an integer in [1,1000]", part)
		}
		p, err := strconv.Atoi(f[1])
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("backend %q: invalid port", part)
		}
		if f[0] == "" || len(f[0]) > 64 || strings.ContainsAny(f[0], " /?#") {
			return nil, fmt.Errorf("backend %q: invalid name", part)
		}
		if seenName[f[0]] || seenPort[f[1]] {
			return nil, fmt.Errorf("backend %q: duplicate name or port", part)
		}
		seenName[f[0]], seenPort[f[1]] = true, true
		out = append(out, backendSpec{Port: f[1], Name: f[0], Weight: w})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no backends")
	}
	return out, nil
}

// backendsFromEnv reads LB_BACKENDS (default: the 3:2:1 pool).
func backendsFromEnv() ([]backendSpec, error) {
	if v := os.Getenv("LB_BACKENDS"); v != "" {
		return ParseBackends(v)
	}
	return defaultBackends(), nil
}

// ruleControllerFromEnv reads the Phase 5 controller settings used by ablations:
// LB_CONTROLLER_INTERVAL_S (decision interval, default 10) and
// LB_CONTROLLER_EMA_ALPHA (1.0 disables smoothing), LB_CONTROLLER_DEADBAND
// (0 disables hysteresis).
func ruleControllerFromEnv() (controller.Config, time.Duration) {
	cfg, interval := controller.DefaultConfig(), 10*time.Second
	if v, err := strconv.ParseFloat(os.Getenv("LB_CONTROLLER_INTERVAL_S"), 64); err == nil && v > 0 {
		interval = time.Duration(v * float64(time.Second))
	}
	if v, err := strconv.ParseFloat(os.Getenv("LB_CONTROLLER_EMA_ALPHA"), 64); err == nil && v > 0 && v <= 1 {
		cfg.EMAAlpha = v
	}
	if v := os.Getenv("LB_CONTROLLER_DEADBAND"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.Deadband = f // 0 disables hysteresis
		}
	}
	return cfg, interval
}
