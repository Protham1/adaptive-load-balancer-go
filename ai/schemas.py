"""Typed, validated views of the Go service's JSON contracts.

Every parser raises SchemaError on malformed input, so downstream code can rely
on finite numbers, non-negative counters and consistent shapes.
"""
from __future__ import annotations

import math
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Mapping


class SchemaError(ValueError):
    """The payload does not match the documented contract."""


_FRAC = re.compile(r"(\.\d{6})\d+")


def parse_timestamp(value: Any) -> float:
    """Parse a Go RFC3339 timestamp (up to 9 fractional digits) to epoch seconds."""
    if not isinstance(value, str):
        raise SchemaError(f"timestamp must be a string, got {type(value).__name__}")
    v = _FRAC.sub(r"\1", value.strip())
    if v.endswith("Z"):
        v = v[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(v)
    except ValueError as e:
        raise SchemaError(f"bad timestamp {value!r}") from e
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.timestamp()


def _get(d: Mapping, key: str, typ=None):
    if not isinstance(d, Mapping) or key not in d:
        raise SchemaError(f"missing field {key!r}")
    v = d[key]
    if typ is not None and not isinstance(v, typ):
        raise SchemaError(f"field {key!r} has type {type(v).__name__}")
    return v


def _num(d: Mapping, key: str, *, non_negative: bool = False) -> float:
    v = _get(d, key)
    if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v):
        raise SchemaError(f"field {key!r} must be a finite number")
    if non_negative and v < 0:
        raise SchemaError(f"field {key!r} must be >= 0")
    return float(v)


@dataclass(frozen=True)
class StateSnapshot:
    """GET /api/state (State v1)."""

    state_version: str
    timestamp: float
    snapshot_age_ms: float
    is_stale: bool
    safe_policy_retain: bool
    missing_critical: bool
    reason: str
    vector: tuple
    feature_names: tuple
    backend_ids: tuple


def parse_state(d: Mapping) -> StateSnapshot:
    vec = _get(d, "flattened_vector", list)
    names = _get(d, "feature_names", list)
    ids = _get(d, "ordered_backend_ids", list)
    for x in vec:
        if isinstance(x, bool) or not isinstance(x, (int, float)):
            raise SchemaError("flattened_vector must contain numbers only")
    return StateSnapshot(
        state_version=_get(d, "state_version", str),
        timestamp=parse_timestamp(_get(d, "timestamp")),
        snapshot_age_ms=_num(d, "snapshot_age_ms"),
        is_stale=bool(_get(d, "is_stale")),
        safe_policy_retain=bool(_get(d, "safe_policy_retain_recommended")),
        missing_critical=bool(_get(d, "has_missing_critical_features")),
        reason=str(d.get("staleness_reason", "")),
        vector=tuple(float(x) for x in vec),  # NaN/inf are rejected later by validate_state
        feature_names=tuple(str(n) for n in names),
        backend_ids=tuple(str(i) for i in ids),
    )


@dataclass(frozen=True)
class BackendCounters:
    backend_id: str
    healthy: bool
    requests: int  # monotonic counter
    errors: int  # monotonic counter
    weight: int  # declared capacity weight
    p95_ms: float


@dataclass(frozen=True)
class MetricsSample:
    """GET /api/metrics, reduced to what the reward needs."""

    t: float  # epoch seconds of the snapshot
    requests: int
    errors: int
    p95_ms: float
    window_valid: bool
    backends: Mapping[str, BackendCounters] = field(default_factory=dict)
    strategy: str = ""
    switch_count: int = 0
    policy_version: str = ""


def parse_metrics(d: Mapping) -> MetricsSample:
    g = _get(d, "global_traffic", Mapping)
    caps = d.get("capacity_configs") or {}
    backends = {}
    for bid, b in _get(d, "backends", Mapping).items():
        lat = _get(b, "latency", Mapping)
        cap = caps.get(bid) or {}
        w = cap.get("static_weight", 1)
        backends[bid] = BackendCounters(
            backend_id=bid,
            healthy=bool(_get(b, "healthy")),
            requests=int(_num(b, "total_requests", non_negative=True)),
            errors=int(_num(b, "total_errors", non_negative=True)),
            weight=int(w) if isinstance(w, (int, float)) and w > 0 else 1,
            p95_ms=_num(lat, "p95_ms", non_negative=True),
        )
    ctl = _get(d, "controller", Mapping)
    return MetricsSample(
        t=parse_timestamp(_get(d, "timestamp")),
        requests=int(_num(g, "total_requests", non_negative=True)),
        errors=int(_num(g, "total_errors", non_negative=True)),
        p95_ms=_num(_get(g, "duration_stats", Mapping), "p95_ms", non_negative=True),
        window_valid=bool(_get(d, "is_valid")),
        backends=backends,
        strategy=str(_get(ctl, "current_strategy")),
        switch_count=int(_num(ctl, "switch_count", non_negative=True)),
        policy_version=str(ctl.get("policy_version", "")),
    )


@dataclass(frozen=True)
class PolicyInfo:
    """GET /api/policy."""

    strategy: str
    version: int
    action_mask: tuple


def parse_policy(d: Mapping) -> PolicyInfo:
    pol = _get(d, "policy", Mapping)
    mask = _get(d, "action_mask", list)
    if not all(isinstance(m, bool) for m in mask):
        raise SchemaError("action_mask must be booleans")
    return PolicyInfo(
        strategy=str(_get(pol, "strategy")),
        version=int(_num(pol, "version", non_negative=True)),
        action_mask=tuple(mask),
    )
