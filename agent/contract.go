// Package agent defines the versioned Go<->Python decision contract (schema v1)
// and a bounded-timeout client for it. The rules here are mirrored exactly by
// ai/contract.py; both sides are tested against the same fixtures in
// testdata/contract/.
//
// Contract rules (v1)
//   - schema_version and state_version must both be 1.
//   - decision_id: 1..64 chars of [A-Za-z0-9._:-].
//   - observed_at: RFC3339; window_seconds in (0, 3600].
//   - every feature is a finite number in [0,1]; at most MaxBackends backends,
//     unique ids in sorted order; messages are limited to MaxBodyBytes.
//   - strategies are the wire names in policy.WireNames(); unknown names are rejected.
//   - unknown JSON fields are rejected (strict v1); NaN/Inf cannot be encoded.
package agent

import (
	"adaptive-load/policy"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"
)

const (
	SchemaVersion  = 1
	StateVersion   = 1
	MaxBackends    = 64
	MaxBodyBytes   = 64 * 1024
	MaxWindowSecs  = 3600
	MaxDiagnostic  = 64
	MaxModelVerLen = 64
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,64}$`)

// GlobalFeatures mirrors State v1 global features (all in [0,1]).
type GlobalFeatures struct {
	RPSNormalized                float64 `json:"rps_normalized"`
	RequestDurationP95Normalized float64 `json:"request_duration_p95_normalized"`
	LongRequestRatio             float64 `json:"long_request_ratio"`
	GlobalErrorRate              float64 `json:"global_error_rate"`
}

// BackendFeatures mirrors State v1 per-backend features (all in [0,1]).
type BackendFeatures struct {
	ID                          string  `json:"id"`
	Healthy                     float64 `json:"healthy"`
	CPUUtilization              float64 `json:"cpu_utilization"`
	MemoryUtilization           float64 `json:"memory_utilization"`
	ActiveConnectionsNormalized float64 `json:"active_connections_normalized"`
	BackendRPSNormalized        float64 `json:"backend_rps_normalized"`
	P95LatencyNormalized        float64 `json:"p95_latency_normalized"`
	ErrorRate                   float64 `json:"error_rate"`
	CapacityRatio               float64 `json:"capacity_ratio"`
}

// Features is the normalized state sent once per control interval.
type Features struct {
	Global   GlobalFeatures    `json:"global"`
	Backends []BackendFeatures `json:"backends"`
}

// BackendOutcome is per-backend evidence for the previous window.
type BackendOutcome struct {
	ID       string `json:"id"`
	Requests uint64 `json:"requests"`
	Healthy  bool   `json:"healthy"`
	Weight   int    `json:"weight"`
}

// PreviousOutcome reports what happened during the evaluation window of the
// previous decision so the learner can compute its reward. CleanExposure is
// decided by Go (policy unchanged, no foreign switch, telemetry valid).
type PreviousOutcome struct {
	DecisionID    string           `json:"decision_id"`
	WindowSeconds float64          `json:"window_seconds"`
	Requests      uint64           `json:"requests"`
	Errors        uint64           `json:"errors"`
	P95Ms         float64          `json:"p95_ms"`
	CleanExposure bool             `json:"clean_exposure"`
	Backends      []BackendOutcome `json:"backends"`
}

// DecisionRequest is POST /v1/decision.
type DecisionRequest struct {
	SchemaVersion     int              `json:"schema_version"`
	DecisionID        string           `json:"decision_id"`
	StateVersion      int              `json:"state_version"`
	ObservedAt        time.Time        `json:"observed_at"`
	WindowSeconds     float64          `json:"window_seconds"`
	CurrentStrategy   string           `json:"current_strategy"`
	AllowedStrategies []string         `json:"allowed_strategies"`
	Features          Features         `json:"features"`
	PreviousOutcome   *PreviousOutcome `json:"previous_outcome,omitempty"`
}

// DecisionResponse is the reply. Scores are optional per-action expected values
// (index = action ID, null = not allowed) used by Go for the switch-margin check.
type DecisionResponse struct {
	SchemaVersion int                `json:"schema_version"`
	DecisionID    string             `json:"decision_id"`
	Strategy      string             `json:"strategy"`
	Weights       map[string]float64 `json:"weights,omitempty"`
	ModelVersion  string             `json:"model_version"`
	Diagnostic    string             `json:"diagnostic"`
	Scores        []*float64         `json:"scores,omitempty"`
	Explored      bool               `json:"explored,omitempty"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func unit(name string, v float64) error {
	if !finite(v) || v < 0 || v > 1 {
		return fmt.Errorf("%s must be a finite number in [0,1], got %v", name, v)
	}
	return nil
}

// ValidID reports whether s is a legal decision / backend identifier.
func ValidID(s string) bool { return idPattern.MatchString(s) }

func validStrategy(name string) error {
	if _, ok := policy.ActionFromWire(name); !ok {
		return fmt.Errorf("unknown strategy %q", name)
	}
	return nil
}

// Validate checks a request against the v1 rules.
func (r *DecisionRequest) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if r.StateVersion != StateVersion {
		return fmt.Errorf("state_version must be %d", StateVersion)
	}
	if !ValidID(r.DecisionID) {
		return errors.New("invalid decision_id")
	}
	if r.ObservedAt.IsZero() {
		return errors.New("observed_at is required")
	}
	if !finite(r.WindowSeconds) || r.WindowSeconds <= 0 || r.WindowSeconds > MaxWindowSecs {
		return fmt.Errorf("window_seconds must be in (0,%d]", MaxWindowSecs)
	}
	if err := validStrategy(r.CurrentStrategy); err != nil {
		return fmt.Errorf("current_strategy: %w", err)
	}
	if len(r.AllowedStrategies) == 0 || len(r.AllowedStrategies) > policy.NumActions {
		return errors.New("allowed_strategies must list 1..5 strategies")
	}
	seen := map[string]bool{}
	for _, s := range r.AllowedStrategies {
		if err := validStrategy(s); err != nil {
			return fmt.Errorf("allowed_strategies: %w", err)
		}
		if seen[s] {
			return fmt.Errorf("allowed_strategies: duplicate %q", s)
		}
		seen[s] = true
	}
	g := r.Features.Global
	for n, v := range map[string]float64{
		"global.rps_normalized": g.RPSNormalized, "global.request_duration_p95_normalized": g.RequestDurationP95Normalized,
		"global.long_request_ratio": g.LongRequestRatio, "global.global_error_rate": g.GlobalErrorRate,
	} {
		if err := unit(n, v); err != nil {
			return err
		}
	}
	bs := r.Features.Backends
	if len(bs) == 0 || len(bs) > MaxBackends {
		return fmt.Errorf("backends must contain 1..%d entries", MaxBackends)
	}
	if !sort.SliceIsSorted(bs, func(i, j int) bool { return bs[i].ID < bs[j].ID }) {
		return errors.New("backends must be in sorted id order")
	}
	for i, b := range bs {
		if !ValidID(b.ID) {
			return fmt.Errorf("backends[%d]: invalid id", i)
		}
		if i > 0 && bs[i-1].ID == b.ID {
			return fmt.Errorf("duplicate backend id %q", b.ID)
		}
		for n, v := range map[string]float64{
			"healthy": b.Healthy, "cpu_utilization": b.CPUUtilization, "memory_utilization": b.MemoryUtilization,
			"active_connections_normalized": b.ActiveConnectionsNormalized, "backend_rps_normalized": b.BackendRPSNormalized,
			"p95_latency_normalized": b.P95LatencyNormalized, "error_rate": b.ErrorRate, "capacity_ratio": b.CapacityRatio,
		} {
			if err := unit("backends["+b.ID+"]."+n, v); err != nil {
				return err
			}
		}
	}
	if po := r.PreviousOutcome; po != nil {
		if !ValidID(po.DecisionID) {
			return errors.New("previous_outcome.decision_id invalid")
		}
		if !finite(po.WindowSeconds) || po.WindowSeconds <= 0 || po.WindowSeconds > MaxWindowSecs {
			return errors.New("previous_outcome.window_seconds out of range")
		}
		if !finite(po.P95Ms) || po.P95Ms < 0 {
			return errors.New("previous_outcome.p95_ms must be finite and >= 0")
		}
		if po.Errors > po.Requests {
			return errors.New("previous_outcome.errors exceeds requests")
		}
		if len(po.Backends) > MaxBackends {
			return errors.New("previous_outcome.backends too large")
		}
		for _, b := range po.Backends {
			if !ValidID(b.ID) || b.Weight < 1 {
				return errors.New("previous_outcome.backends entry invalid")
			}
		}
	}
	return nil
}

var globalKeys = []string{"rps_normalized", "request_duration_p95_normalized", "long_request_ratio", "global_error_rate"}
var backendKeys = []string{"id", "healthy", "cpu_utilization", "memory_utilization", "active_connections_normalized",
	"backend_rps_normalized", "p95_latency_normalized", "error_rate", "capacity_ratio"}

// DecodeRequest strictly decodes and validates a request: unknown fields are
// rejected, and every feature key must be present (a missing key would otherwise
// silently decode as 0).
func DecodeRequest(data []byte) (*DecisionRequest, error) {
	if len(data) > MaxBodyBytes {
		return nil, fmt.Errorf("request is %d bytes (> %d)", len(data), MaxBodyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r DecisionRequest
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("malformed request: %w", err)
	}
	if dec.More() {
		return nil, errors.New("trailing data after request")
	}
	var raw struct {
		Features struct {
			Global   map[string]json.RawMessage   `json:"global"`
			Backends []map[string]json.RawMessage `json:"backends"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("malformed request: %w", err)
	}
	for _, k := range globalKeys {
		if _, ok := raw.Features.Global[k]; !ok {
			return nil, fmt.Errorf("features.global.%s is required", k)
		}
	}
	for i, b := range raw.Features.Backends {
		for _, k := range backendKeys {
			if _, ok := b[k]; !ok {
				return nil, fmt.Errorf("features.backends[%d].%s is required", i, k)
			}
		}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Marshal validates then encodes a request (NaN/Inf can never reach the wire).
func (r *DecisionRequest) Marshal() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBodyBytes {
		return nil, fmt.Errorf("request is %d bytes (> %d)", len(b), MaxBodyBytes)
	}
	return b, nil
}

// ParseResponse strictly decodes and validates a response for request req.
// A response whose decision_id differs from the request is a stale decision.
func ParseResponse(data []byte, req *DecisionRequest) (*DecisionResponse, error) {
	if len(data) > MaxBodyBytes {
		return nil, fmt.Errorf("response is %d bytes (> %d)", len(data), MaxBodyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var resp DecisionResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("malformed response: %w", err)
	}
	if dec.More() {
		return nil, errors.New("trailing data after response")
	}
	if err := resp.Validate(); err != nil {
		return nil, err
	}
	if req != nil && resp.DecisionID != req.DecisionID {
		return nil, ErrStaleDecision
	}
	return &resp, nil
}

// ErrStaleDecision means the response answers a different decision than requested.
var ErrStaleDecision = errors.New("stale decision: decision_id does not match the request")

// Validate checks a response against the v1 rules.
func (r *DecisionResponse) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if !ValidID(r.DecisionID) {
		return errors.New("invalid decision_id")
	}
	if err := validStrategy(r.Strategy); err != nil {
		return err
	}
	if r.ModelVersion == "" || len(r.ModelVersion) > MaxModelVerLen {
		return errors.New("model_version must be 1..64 chars")
	}
	if r.Diagnostic == "" || len(r.Diagnostic) > MaxDiagnostic {
		return errors.New("diagnostic must be 1..64 chars")
	}
	if len(r.Weights) > MaxBackends {
		return errors.New("weights too large")
	}
	for id, w := range r.Weights {
		if !ValidID(id) {
			return fmt.Errorf("weights: invalid backend id %q", id)
		}
		if !finite(w) || w < 0 || w > 1 {
			return fmt.Errorf("weights[%s] must be a finite number in [0,1]", id)
		}
	}
	if len(r.Scores) != 0 && len(r.Scores) != policy.NumActions {
		return fmt.Errorf("scores must have %d entries", policy.NumActions)
	}
	for _, s := range r.Scores {
		if s != nil && !finite(*s) {
			return errors.New("scores must be finite")
		}
	}
	return nil
}
