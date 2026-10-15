package metrics

import (
	"sync"
	"time"
)

// ProbeStatus captures health checking state and latency for an upstream backend.
type ProbeStatus struct {
	BackendID            string        `json:"backend_id"`
	Healthy              bool          `json:"healthy"`
	LastProbeDuration    time.Duration `json:"-"`
	LastProbeDurationMs  float64       `json:"last_probe_duration_ms"`
	LastProbeTime        time.Time     `json:"last_probe_time"`
	ConsecutiveSuccesses int           `json:"consecutive_successes"`
	ConsecutiveFailures  int           `json:"consecutive_failures"`
	LastError            string        `json:"last_error,omitempty"`
}

// BackendProbeTracker maintains active probe observations for all configured backends.
type BackendProbeTracker struct {
	mu     sync.RWMutex
	probes map[string]*ProbeStatus
}

// NewBackendProbeTracker initializes probe tracking.
func NewBackendProbeTracker() *BackendProbeTracker {
	return &BackendProbeTracker{
		probes: make(map[string]*ProbeStatus),
	}
}

// RecordProbe records the outcome and duration of a backend health check probe.
func (p *BackendProbeTracker) RecordProbe(backendID string, healthy bool, duration time.Duration, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	status, exists := p.probes[backendID]
	if !exists {
		status = &ProbeStatus{BackendID: backendID}
		p.probes[backendID] = status
	}

	status.Healthy = healthy
	status.LastProbeDuration = duration
	status.LastProbeDurationMs = durationToMs(duration)
	status.LastProbeTime = time.Now()

	if healthy {
		status.ConsecutiveSuccesses++
		status.ConsecutiveFailures = 0
		status.LastError = ""
	} else {
		status.ConsecutiveFailures++
		status.ConsecutiveSuccesses = 0
		if err != nil {
			status.LastError = err.Error()
		} else {
			status.LastError = "health check failed"
		}
	}
}

// GetProbeStatus retrieves the latest probe result for a backend.
func (p *BackendProbeTracker) GetProbeStatus(backendID string) (ProbeStatus, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	status, exists := p.probes[backendID]
	if !exists {
		return ProbeStatus{}, false
	}
	return *status, true
}

// GetAllProbeStatuses returns a snapshot map of all probe observations.
func (p *BackendProbeTracker) GetAllProbeStatuses() map[string]ProbeStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	res := make(map[string]ProbeStatus, len(p.probes))
	for k, v := range p.probes {
		res[k] = *v
	}
	return res
}
