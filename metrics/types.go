package metrics

import (
	"encoding/json"
	"time"
)

// OptionalFloat represents a float64 measurement that may be missing or invalid.
// Prevents missing metrics (such as unavailable CPU readings) from silently defaulting to 0.0.
type OptionalFloat struct {
	Value float64 `json:"value"`
	Valid bool    `json:"valid"`
}

// NewOptionalFloat constructs an OptionalFloat with a valid value.
func NewOptionalFloat(val float64) OptionalFloat {
	return OptionalFloat{Value: val, Valid: true}
}

// MissingFloat constructs an OptionalFloat representing an explicitly missing measurement.
func MissingFloat() OptionalFloat {
	return OptionalFloat{Value: 0.0, Valid: false}
}

// MarshalJSON serializes OptionalFloat; if invalid, it outputs null in JSON.
func (o OptionalFloat) MarshalJSON() ([]byte, error) {
	if !o.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// UnmarshalJSON deserializes null into an invalid OptionalFloat.
func (o *OptionalFloat) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		o.Valid = false
		o.Value = 0.0
		return nil
	}
	o.Valid = true
	return json.Unmarshal(data, &o.Value)
}

// ErrorCategory identifies the standard classification of request failures.
type ErrorCategory string

const (
	ErrCategoryHTTP5xx      ErrorCategory = "http_5xx"
	ErrCategoryTransport    ErrorCategory = "transport"
	ErrCategoryTimeout      ErrorCategory = "timeout"
	ErrCategoryCancellation ErrorCategory = "cancellation"
)

// ErrorBreakdown contains counts of failures categorized by root cause.
type ErrorBreakdown struct {
	HTTP5xx      uint64 `json:"http_5xx"`
	Transport    uint64 `json:"transport"`
	Timeout      uint64 `json:"timeout"`
	Cancellation uint64 `json:"cancellation"`
}

// LatencyStats holds percentile distributions and summaries for durations.
// Internal calculations strictly preserve time.Duration; float64 millisecond
// conversions are provided only at serialization boundaries.
type LatencyStats struct {
	SampleCount uint64        `json:"sample_count"`
	P50         time.Duration `json:"-"`
	P90         time.Duration `json:"-"`
	P95         time.Duration `json:"-"`
	P99         time.Duration `json:"-"`
	Min         time.Duration `json:"-"`
	Max         time.Duration `json:"-"`
	Avg         time.Duration `json:"-"`

	// Serialization boundary fields in milliseconds
	P50Ms float64 `json:"p50_ms"`
	P90Ms float64 `json:"p90_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MinMs float64 `json:"min_ms"`
	MaxMs float64 `json:"max_ms"`
	AvgMs float64 `json:"avg_ms"`
}

// PopulateSerializationFields converts time.Duration values into milliseconds.
func (l *LatencyStats) PopulateSerializationFields() {
	l.P50Ms = durationToMs(l.P50)
	l.P90Ms = durationToMs(l.P90)
	l.P95Ms = durationToMs(l.P95)
	l.P99Ms = durationToMs(l.P99)
	l.MinMs = durationToMs(l.Min)
	l.MaxMs = durationToMs(l.Max)
	l.AvgMs = durationToMs(l.Avg)
}

func durationToMs(d time.Duration) float64 {
	if d <= 0 {
		return 0.0
	}
	return float64(d.Microseconds()) / 1000.0
}

// BackendRuntimeMetrics represents observed load and service quality for an individual backend.
type BackendRuntimeMetrics struct {
	BackendID        string         `json:"backend_id"`
	URL              string         `json:"url"`
	Healthy          bool           `json:"healthy"`
	ActiveConns      int64          `json:"active_connections"`
	TotalRequests    uint64         `json:"total_requests"` // monotonic counter
	TotalErrors      uint64         `json:"total_errors"`   // monotonic counter
	ErrorsByCategory ErrorBreakdown `json:"errors_by_category"`
	RPS              float64        `json:"rps"`        // calculated over aggregation window
	ErrorRate        float64        `json:"error_rate"` // errors/sec over aggregation window
	Latency          LatencyStats   `json:"latency"`
	WindowSeconds    float64        `json:"window_seconds"`
	LastUpdated      time.Time      `json:"last_updated"`
}

// HostResourceMetrics represents system and process resource pressure.
type HostResourceMetrics struct {
	Timestamp        time.Time     `json:"timestamp"`
	CPUUtilization   OptionalFloat `json:"cpu_utilization"` // percentage 0.0 - 100.0%, explicitly marked if missing
	MemoryUsedBytes  uint64        `json:"memory_used_bytes"`
	MemoryTotalBytes uint64        `json:"memory_total_bytes"`
	MemoryPercent    OptionalFloat `json:"memory_percent"`
	GoroutineCount   int           `json:"goroutine_count"`
	GCNum            uint32        `json:"gc_num"`
	GCPauseTotalNs   uint64        `json:"gc_pause_total_ns"`
	Source           string        `json:"source"` // "process" or "host"
	LimitationNote   string        `json:"limitation_note"`
}

// GlobalTrafficMetrics describes overall incoming workload across all endpoints.
type GlobalTrafficMetrics struct {
	Timestamp      time.Time         `json:"timestamp"`
	TotalRequests  uint64            `json:"total_requests"`
	TotalErrors    uint64            `json:"total_errors"`
	CurrentRPS     float64           `json:"current_rps"`
	ErrorRate      float64           `json:"error_rate"`
	ActiveRequests int64             `json:"active_requests"`
	BytesReceived  uint64            `json:"bytes_received"`
	BytesSent      uint64            `json:"bytes_sent"`
	DurationStats  LatencyStats      `json:"duration_stats"`
	MethodMix      map[string]uint64 `json:"method_mix"`
	RouteMix       map[string]uint64 `json:"route_mix"`
	WindowSeconds  float64           `json:"window_seconds"`
}

// BackendCapacityConfig describes capacity limits and static hardware sizing for heterogeneous backends.
type BackendCapacityConfig struct {
	BackendID        string `json:"backend_id"`
	URL              string `json:"url"`
	StaticWeight     int    `json:"static_weight"`
	CPUCores         int    `json:"cpu_cores"`
	MemoryLimitBytes uint64 `json:"memory_limit_bytes"`
	ConnectionLimit  int64  `json:"connection_limit"`
}

// ControllerMetrics measures adaptive load balancer controller behavior, decisions, and overhead.
type ControllerMetrics struct {
	CurrentStrategy  string        `json:"current_strategy"`
	StrategyAge      time.Duration `json:"-"`
	StrategyAgeSec   float64       `json:"strategy_age_seconds"`
	LastDecisionTime time.Duration `json:"-"`
	LastDecisionUs   float64       `json:"last_decision_microseconds"`
	PolicyVersion    string        `json:"policy_version"`
	SwitchCount      uint64        `json:"switch_count"`
	LastSwitchAt     time.Time     `json:"last_switch_at"`
}

// MetricsSnapshot provides an atomic, consistent point-in-time view of all metric groups.
type MetricsSnapshot struct {
	Timestamp       time.Time                          `json:"timestamp"`
	WindowSeconds   float64                            `json:"window_seconds"`
	Host            HostResourceMetrics                `json:"host"`
	GlobalTraffic   GlobalTrafficMetrics               `json:"global_traffic"`
	Backends        map[string]*BackendRuntimeMetrics  `json:"backends"`
	CapacityConfigs map[string]*BackendCapacityConfig  `json:"capacity_configs"`
	Controller      ControllerMetrics                  `json:"controller"`
}
