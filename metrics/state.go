package metrics

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// CurrentStateVersion defines the active version of the fixed feature vector contract.
const CurrentStateVersion = "v1"

// StateScalerConfig defines the versioned, immutable normalization constants used
// to scale raw metrics into physically meaningful bounds in [0.0, 1.0].
type StateScalerConfig struct {
	Version           string  `json:"version"`
	LatencyScaleMs    float64 `json:"latency_scale_ms"`     // documented scale to clip latencies (e.g., 500ms)
	MaxReferenceRPS   float64 `json:"max_reference_rps"`    // maximum reference RPS for scaling (e.g., 2000 req/s)
	MaxConnectionsRef float64 `json:"max_connections_ref"`  // maximum reference concurrency (e.g., 100)
	UseLogScaleRPS    bool    `json:"use_log_scale_rps"`    // apply log(1+x)/log(1+max) for wide dynamic ranges
	MaxAllowedAgeSec  float64 `json:"max_allowed_age_sec"`  // maximum age before snapshot is marked stale (e.g. 5.0s)
}

// DefaultStateScalerConfig returns production default scalers for State v1.
func DefaultStateScalerConfig() StateScalerConfig {
	return StateScalerConfig{
		Version:           CurrentStateVersion,
		LatencyScaleMs:    500.0,
		MaxReferenceRPS:   2000.0,
		MaxConnectionsRef: 100.0,
		UseLogScaleRPS:    true,
		MaxAllowedAgeSec:  5.0,
	}
}

// GlobalStateFeatures encapsulates normalized system-wide workload signals.
type GlobalStateFeatures struct {
	RPSNormalized                float64 `json:"rps_normalized"`
	RequestDurationP95Normalized float64 `json:"request_duration_p95_normalized"`
	LongRequestRatio             float64 `json:"long_request_ratio"`
	GlobalErrorRate              float64 `json:"global_error_rate"`
}

// BackendStateFeatures encapsulates normalized runtime and capacity metrics for a single backend.
type BackendStateFeatures struct {
	BackendID                   string  `json:"backend_id"`
	Healthy                     float64 `json:"healthy"`                       // 1.0 if healthy, 0.0 otherwise
	CPUUtilization              float64 `json:"cpu_utilization"`              // fraction in [0.0, 1.0]
	CPUIsMissing                bool    `json:"cpu_is_missing"`               // explicit missing-value indicator
	MemoryUtilization           float64 `json:"memory_utilization"`           // fraction in [0.0, 1.0]
	MemoryIsMissing             bool    `json:"memory_is_missing"`            // explicit missing-value indicator
	ActiveConnectionsNormalized float64 `json:"active_connections_normalized"` // fraction in [0.0, 1.0]
	BackendRPSNormalized        float64 `json:"backend_rps_normalized"`       // fraction in [0.0, 1.0]
	P95LatencyNormalized        float64 `json:"p95_latency_normalized"`       // fraction in [0.0, 1.0]
	ErrorRate                   float64 `json:"error_rate"`                   // fraction in [0.0, 1.0]
	CapacityRatio               float64 `json:"capacity_ratio"`               // static weight ratio in [0.0, 1.0]
}

// StateRepresentation is the complete fixed, versioned feature vector contract between
// the Go telemetry subsystem and adaptive control agents.
type StateRepresentation struct {
	StateVersion                string                 `json:"state_version"`
	Timestamp                   time.Time              `json:"timestamp"`
	SnapshotAgeMs               float64                `json:"snapshot_age_ms"`
	MaxAllowedAgeMs             float64                `json:"max_allowed_age_ms"`
	IsStale                     bool                   `json:"is_stale"`
	SafePolicyRetainRecommended bool                   `json:"safe_policy_retain_recommended"`
	StalenessReason             string                 `json:"staleness_reason,omitempty"`
	HasMissingCriticalFeatures  bool                   `json:"has_missing_critical_features"`
	Global                      GlobalStateFeatures    `json:"global"`
	Backends                    []BackendStateFeatures `json:"backends"`
	OrderedBackendIDs           []string               `json:"ordered_backend_ids"`
	FlattenedVector             []float64              `json:"flattened_vector"`
	FeatureNames                []string               `json:"feature_names"`
	Scaler                      StateScalerConfig      `json:"scaler"`
}

// FeatureDefinition describes an individual element in the fixed feature vector.
type FeatureDefinition struct {
	Index             int     `json:"index"`
	Name              string  `json:"name"`
	Scope             string  `json:"scope"` // "global" or "backend"
	Min               float64 `json:"min"`
	Max               float64 `json:"max"`
	Unit              string  `json:"unit"`
	ScalerDescription string  `json:"scaler_description"`
}

// StateSchemaMetadata documents the ordering, bounds, and scaling formulas for a state version.
type StateSchemaMetadata struct {
	StateVersion       string              `json:"state_version"`
	GlobalFeatureCount int                 `json:"global_feature_count"`
	PerBackendCount    int                 `json:"per_backend_count"`
	Features           []FeatureDefinition `json:"features"`
	Description        string              `json:"description"`
}

// BuildStateRepresentation constructs a deterministic, versioned state vector from a metrics snapshot.
// Backend ordering is strictly guaranteed by lexicographical sorting of BackendIDs.
func BuildStateRepresentation(snap *MetricsSnapshot, cfg StateScalerConfig, maxBackends ...int) *StateRepresentation {
	if cfg.Version == "" {
		cfg = DefaultStateScalerConfig()
	}

	now := time.Now()
	snapTime := snap.Timestamp
	if snapTime.IsZero() {
		snapTime = now
	}

	age := now.Sub(snapTime)
	ageMs := float64(age.Microseconds()) / 1000.0
	maxAgeMs := cfg.MaxAllowedAgeSec * 1000.0
	isStale := ageMs > maxAgeMs

	var stalenessReason string
	safePolicyRetain := isStale
	if isStale {
		stalenessReason = fmt.Sprintf("snapshot age (%.1fms) exceeds maximum allowed age (%.1fms)", ageMs, maxAgeMs)
	}

	// 1. Global features
	rawRPS := snap.GlobalTraffic.CurrentRPS.Raw
	var rpsNorm float64
	if cfg.UseLogScaleRPS {
		rpsNorm = LogScaleNormalize(rawRPS, cfg.MaxReferenceRPS)
	} else {
		rpsNorm = MinMaxNormalize(rawRPS, 0, cfg.MaxReferenceRPS)
	}

	p95Norm := LatencyScaleNormalize(snap.GlobalTraffic.DurationStats.P95Ms, cfg.LatencyScaleMs)
	longRatio := math.Max(0.0, math.Min(1.0, snap.GlobalTraffic.Workload.LongRequestFraction))

	// Global error rate as a fraction of traffic [0.0, 1.0]
	globalErrRate := 0.0
	if snap.GlobalTraffic.TotalRequests > 0 {
		globalErrRate = math.Min(1.0, float64(snap.GlobalTraffic.TotalErrors)/float64(snap.GlobalTraffic.TotalRequests))
	}

	globalFeatures := GlobalStateFeatures{
		RPSNormalized:                rpsNorm,
		RequestDurationP95Normalized: p95Norm,
		LongRequestRatio:             longRatio,
		GlobalErrorRate:              globalErrRate,
	}

	// 2. Stable Backend Ordering
	backendIDs := make([]string, 0, len(snap.Backends))
	for id := range snap.Backends {
		backendIDs = append(backendIDs, id)
	}
	sort.Strings(backendIDs)

	// Calculate total weight for capacity ratio
	totalWeight := 0
	for _, id := range backendIDs {
		if cfgVal := snap.CapacityConfigs[id]; cfgVal != nil && cfgVal.StaticWeight > 0 {
			totalWeight += cfgVal.StaticWeight
		} else {
			totalWeight += 1
		}
	}
	if totalWeight <= 0 {
		totalWeight = len(backendIDs)
	}

	backendFeatures := make([]BackendStateFeatures, 0, len(backendIDs))
	featureNames := []string{
		"global.rps_normalized",
		"global.request_duration_p95_normalized",
		"global.long_request_ratio",
		"global.global_error_rate",
	}
	flattened := []float64{
		rpsNorm,
		p95Norm,
		longRatio,
		globalErrRate,
	}

	hasMissingCritical := !snap.IsValid

	for _, bID := range backendIDs {
		b := snap.Backends[bID]
		capCfg := snap.CapacityConfigs[bID]

		healthyVal := 0.0
		if b.Healthy && b.Eligible {
			healthyVal = 1.0
		} else if b.Healthy {
			healthyVal = 0.5
		}

		// CPU: If missing, flag explicitly and use conservative default 0.5
		cpuVal := 0.5
		cpuMissing := true
		if snap.Host.CPUUtilization.Valid {
			cpuVal = math.Max(0.0, math.Min(1.0, snap.Host.CPUUtilization.Value/100.0))
			cpuMissing = false
		}

		// Memory: used / total OS memory or limit
		memVal := 0.0
		memMissing := false
		if snap.Host.MemoryTotalBytes > 0 {
			memVal = math.Max(0.0, math.Min(1.0, float64(snap.Host.MemoryUsedBytes)/float64(snap.Host.MemoryTotalBytes)))
		} else if snap.Host.MemoryPercent.Valid {
			memVal = math.Max(0.0, math.Min(1.0, snap.Host.MemoryPercent.Value/100.0))
		} else {
			memMissing = true
		}

		// Active connections
		connRef := cfg.MaxConnectionsRef
		if capCfg != nil && capCfg.ConnectionLimit > 0 {
			connRef = float64(capCfg.ConnectionLimit)
		}
		activeConnsNorm := MinMaxNormalize(float64(b.ActiveConns), 0, connRef)

		// Backend RPS
		var bRPSNorm float64
		if cfg.UseLogScaleRPS {
			bRPSNorm = LogScaleNormalize(b.RPS.Raw, cfg.MaxReferenceRPS)
		} else {
			bRPSNorm = MinMaxNormalize(b.RPS.Raw, 0, cfg.MaxReferenceRPS)
		}

		// P95 Latency
		bP95Norm := LatencyScaleNormalize(b.Latency.P95Ms, cfg.LatencyScaleMs)

		// Error Rate fraction
		bErrRate := 0.0
		if b.TotalRequests > 0 {
			bErrRate = math.Min(1.0, float64(b.TotalErrors)/float64(b.TotalRequests))
		}

		// Capacity ratio
		bWeight := 1
		if capCfg != nil && capCfg.StaticWeight > 0 {
			bWeight = capCfg.StaticWeight
		}
		capRatio := math.Min(1.0, float64(bWeight)/float64(totalWeight))

		bf := BackendStateFeatures{
			BackendID:                   bID,
			Healthy:                     healthyVal,
			CPUUtilization:              cpuVal,
			CPUIsMissing:                cpuMissing,
			MemoryUtilization:           memVal,
			MemoryIsMissing:             memMissing,
			ActiveConnectionsNormalized: activeConnsNorm,
			BackendRPSNormalized:        bRPSNorm,
			P95LatencyNormalized:        bP95Norm,
			ErrorRate:                   bErrRate,
			CapacityRatio:               capRatio,
		}
		backendFeatures = append(backendFeatures, bf)

		// Append to flattened vector in exact stable order
		featureNames = append(featureNames,
			fmt.Sprintf("backend[%s].healthy", bID),
			fmt.Sprintf("backend[%s].cpu_utilization", bID),
			fmt.Sprintf("backend[%s].memory_utilization", bID),
			fmt.Sprintf("backend[%s].active_connections_normalized", bID),
			fmt.Sprintf("backend[%s].backend_rps_normalized", bID),
			fmt.Sprintf("backend[%s].p95_latency_normalized", bID),
			fmt.Sprintf("backend[%s].error_rate", bID),
			fmt.Sprintf("backend[%s].capacity_ratio", bID),
		)
		flattened = append(flattened,
			healthyVal,
			cpuVal,
			memVal,
			activeConnsNorm,
			bRPSNorm,
			bP95Norm,
			bErrRate,
			capRatio,
		)
	}

	// Optional fixed-size padding if maxBackends is configured
	if len(maxBackends) > 0 && maxBackends[0] > len(backendIDs) {
		limit := maxBackends[0]
		for i := len(backendIDs); i < limit; i++ {
			padID := fmt.Sprintf("pad_%d", i)
			featureNames = append(featureNames,
				fmt.Sprintf("backend[%s].healthy", padID),
				fmt.Sprintf("backend[%s].cpu_utilization", padID),
				fmt.Sprintf("backend[%s].memory_utilization", padID),
				fmt.Sprintf("backend[%s].active_connections_normalized", padID),
				fmt.Sprintf("backend[%s].backend_rps_normalized", padID),
				fmt.Sprintf("backend[%s].p95_latency_normalized", padID),
				fmt.Sprintf("backend[%s].error_rate", padID),
				fmt.Sprintf("backend[%s].capacity_ratio", padID),
			)
			flattened = append(flattened, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0)
		}
	}

	if snap.FallbackRecommended {
		safePolicyRetain = true
		if stalenessReason == "" {
			stalenessReason = snap.ReliabilityReason
		}
	}

	return &StateRepresentation{
		StateVersion:                cfg.Version,
		Timestamp:                   snapTime,
		SnapshotAgeMs:               ageMs,
		MaxAllowedAgeMs:             maxAgeMs,
		IsStale:                     isStale,
		SafePolicyRetainRecommended: safePolicyRetain,
		StalenessReason:             stalenessReason,
		HasMissingCriticalFeatures:  hasMissingCritical,
		Global:                      globalFeatures,
		Backends:                    backendFeatures,
		OrderedBackendIDs:           backendIDs,
		FlattenedVector:             flattened,
		FeatureNames:                featureNames,
		Scaler:                      cfg,
	}
}

// GetStateSchema returns complete documentation and formal metadata for State v1.
func GetStateSchema(version string) StateSchemaMetadata {
	return StateSchemaMetadata{
		StateVersion:       CurrentStateVersion,
		GlobalFeatureCount: 4,
		PerBackendCount:    8,
		Description: "State v1 feature representation for adaptive load balancing: 4 global workload features followed by 8 per-backend features in strictly sorted BackendID order.",
		Features: []FeatureDefinition{
			{Index: 0, Name: "global.rps_normalized", Scope: "global", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "log(1 + RPS) / log(1 + MaxReferenceRPS)"},
			{Index: 1, Name: "global.request_duration_p95_normalized", Scope: "global", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "min(1.0, P95Ms / LatencyScaleMs)"},
			{Index: 2, Name: "global.long_request_ratio", Scope: "global", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "long_requests_in_window / completed_requests"},
			{Index: 3, Name: "global.global_error_rate", Scope: "global", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "total_errors / max(total_requests, 1)"},
			{Index: 4, Name: "backend[id].healthy", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "categorical", ScalerDescription: "1.0 if healthy and eligible, 0.5 if healthy but saturated, 0.0 if down"},
			{Index: 5, Name: "backend[id].cpu_utilization", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "CPU% / 100.0, default 0.5 with missing indicator if unread"},
			{Index: 6, Name: "backend[id].memory_utilization", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "used_bytes / total_bytes"},
			{Index: 7, Name: "backend[id].active_connections_normalized", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "active_conns / connection_limit"},
			{Index: 8, Name: "backend[id].backend_rps_normalized", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "log(1 + backend_rps) / log(1 + MaxReferenceRPS)"},
			{Index: 9, Name: "backend[id].p95_latency_normalized", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "min(1.0, backend_p95_ms / LatencyScaleMs)"},
			{Index: 10, Name: "backend[id].error_rate", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "backend_errors / max(backend_requests, 1)"},
			{Index: 11, Name: "backend[id].capacity_ratio", Scope: "backend", Min: 0.0, Max: 1.0, Unit: "fraction", ScalerDescription: "static_weight / total_weight"},
		},
	}
}
