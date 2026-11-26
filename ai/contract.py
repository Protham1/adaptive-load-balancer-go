"""Go<->Python decision contract, schema v1 (mirror of agent/contract.go).

Both sides validate against the same fixtures in testdata/contract/. Rules:
  * schema_version == 1 and state_version == 1; unknown fields rejected at every level
  * decision_id / backend ids: 1..64 chars of [A-Za-z0-9._:-]
  * observed_at RFC3339; window_seconds in (0, 3600]
  * every feature a finite number in [0,1]; 1..64 backends, unique, sorted by id
  * strategies are wire names (WIRE_NAMES); NaN/Infinity literals are rejected at parse time
  * bodies are limited to MAX_BODY_BYTES
"""
from __future__ import annotations

import json
import math
import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional

from .actions import NUM_ACTIONS, WIRE_NAMES, WIRE_TO_ACTION
from .schemas import SchemaError, parse_timestamp

SCHEMA_VERSION = 1
STATE_VERSION = 1
MAX_BACKENDS = 64
MAX_BODY_BYTES = 64 * 1024
MAX_WINDOW_S = 3600

_ID = re.compile(r"^[A-Za-z0-9._:\-]{1,64}$")
GLOBAL_KEYS = ("rps_normalized", "request_duration_p95_normalized", "long_request_ratio", "global_error_rate")
BACKEND_KEYS = ("healthy", "cpu_utilization", "memory_utilization", "active_connections_normalized",
                "backend_rps_normalized", "p95_latency_normalized", "error_rate", "capacity_ratio")
REQUEST_KEYS = {"schema_version", "decision_id", "state_version", "observed_at", "window_seconds",
                "current_strategy", "allowed_strategies", "features", "previous_outcome"}
RESPONSE_KEYS = {"schema_version", "decision_id", "strategy", "weights", "model_version", "diagnostic",
                 "scores", "explored"}
OUTCOME_KEYS = {"decision_id", "window_seconds", "requests", "errors", "p95_ms", "clean_exposure", "backends"}
OUTCOME_BACKEND_KEYS = {"id", "requests", "healthy", "weight"}


class ContractError(ValueError):
    """The message violates the v1 contract (HTTP 400)."""


def _reject_constant(name):
    raise ContractError(f"non-finite literal {name} is not valid JSON")


def loads_strict(data) -> dict:
    """json.loads that refuses NaN/Infinity and oversized bodies."""
    if len(data) > MAX_BODY_BYTES:
        raise ContractError(f"body is {len(data)} bytes (> {MAX_BODY_BYTES})")
    if isinstance(data, (bytes, bytearray)):
        try:
            data = data.decode("utf-8")
        except UnicodeDecodeError as e:
            raise ContractError("body is not UTF-8") from e
    try:
        doc = json.loads(data, parse_constant=_reject_constant)
    except json.JSONDecodeError as e:
        raise ContractError(f"malformed JSON: {e}") from e
    if not isinstance(doc, dict):
        raise ContractError("body must be a JSON object")
    return doc


def dumps_strict(doc: dict) -> bytes:
    """Encode without ever emitting NaN/Infinity."""
    return json.dumps(doc, allow_nan=False, separators=(",", ":")).encode("utf-8")


def valid_id(s) -> bool:
    return isinstance(s, str) and bool(_ID.match(s))


def _is_num(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def _unit(name, v):
    if not _is_num(v) or v < 0 or v > 1:
        raise ContractError(f"{name} must be a finite number in [0,1]")
    return float(v)


def _exact_keys(name, d, required, allowed=None):
    if not isinstance(d, dict):
        raise ContractError(f"{name} must be an object")
    allowed = set(allowed if allowed is not None else required)
    unknown = set(d) - allowed
    if unknown:
        raise ContractError(f"{name}: unknown field(s) {sorted(unknown)}")
    missing = set(required) - set(d)
    if missing:
        raise ContractError(f"{name}: missing field(s) {sorted(missing)}")


@dataclass(frozen=True)
class Outcome:
    decision_id: str
    window_seconds: float
    requests: int
    errors: int
    p95_ms: float
    clean_exposure: bool
    backends: tuple  # of (id, requests, healthy, weight)


@dataclass(frozen=True)
class DecisionRequest:
    decision_id: str
    observed_at: float
    window_seconds: float
    current_strategy: str
    allowed_strategies: tuple
    global_features: tuple  # in GLOBAL_KEYS order
    backend_ids: tuple
    backend_features: tuple  # tuple of tuples in BACKEND_KEYS order, aligned with backend_ids
    previous_outcome: Optional[Outcome] = None

    @property
    def vector(self) -> tuple:
        """State v1 flattened vector: 4 global + 8 per backend (sorted by id)."""
        out = list(self.global_features)
        for b in self.backend_features:
            out.extend(b)
        return tuple(out)

    @property
    def action_mask(self) -> tuple:
        allowed = set(self.allowed_strategies)
        return tuple(WIRE_NAMES[a] in allowed for a in range(NUM_ACTIONS))

    @property
    def current_action(self) -> int:
        return WIRE_TO_ACTION[self.current_strategy]


def _strategy(name, field_name):
    if not isinstance(name, str) or name not in WIRE_TO_ACTION:
        raise ContractError(f"{field_name}: unknown strategy {name!r}")
    return name


def parse_request(doc: dict) -> DecisionRequest:
    _exact_keys("request", doc, REQUEST_KEYS - {"previous_outcome"}, REQUEST_KEYS)
    if doc["schema_version"] != SCHEMA_VERSION or isinstance(doc["schema_version"], bool):
        raise ContractError(f"schema_version must be {SCHEMA_VERSION}")
    if doc["state_version"] != STATE_VERSION or isinstance(doc["state_version"], bool):
        raise ContractError(f"state_version must be {STATE_VERSION}")
    if not valid_id(doc["decision_id"]):
        raise ContractError("invalid decision_id")
    try:
        observed = parse_timestamp(doc["observed_at"])
    except SchemaError as e:
        raise ContractError(f"observed_at: {e}") from e
    w = doc["window_seconds"]
    if not _is_num(w) or w <= 0 or w > MAX_WINDOW_S:
        raise ContractError(f"window_seconds must be in (0,{MAX_WINDOW_S}]")
    cur = _strategy(doc["current_strategy"], "current_strategy")
    allowed = doc["allowed_strategies"]
    if not isinstance(allowed, list) or not 1 <= len(allowed) <= NUM_ACTIONS:
        raise ContractError("allowed_strategies must list 1..5 strategies")
    for a in allowed:
        _strategy(a, "allowed_strategies")
    if len(set(allowed)) != len(allowed):
        raise ContractError("allowed_strategies contains duplicates")

    feats = doc["features"]
    _exact_keys("features", feats, {"global", "backends"})
    _exact_keys("features.global", feats["global"], GLOBAL_KEYS)
    gf = tuple(_unit(f"features.global.{k}", feats["global"][k]) for k in GLOBAL_KEYS)
    bl = feats["backends"]
    if not isinstance(bl, list) or not 1 <= len(bl) <= MAX_BACKENDS:
        raise ContractError(f"backends must contain 1..{MAX_BACKENDS} entries")
    ids, bf = [], []
    for i, b in enumerate(bl):
        _exact_keys(f"backends[{i}]", b, ("id",) + BACKEND_KEYS)
        if not valid_id(b["id"]):
            raise ContractError(f"backends[{i}]: invalid id")
        ids.append(b["id"])
        bf.append(tuple(_unit(f"backends[{b['id']}].{k}", b[k]) for k in BACKEND_KEYS))
    if len(set(ids)) != len(ids):
        raise ContractError("duplicate backend id")
    if ids != sorted(ids):
        raise ContractError("backends must be in sorted id order")

    outcome = None
    if doc.get("previous_outcome") is not None:
        o = doc["previous_outcome"]
        _exact_keys("previous_outcome", o, OUTCOME_KEYS)
        if not valid_id(o["decision_id"]):
            raise ContractError("previous_outcome.decision_id invalid")
        ow = o["window_seconds"]
        if not _is_num(ow) or ow <= 0 or ow > MAX_WINDOW_S:
            raise ContractError("previous_outcome.window_seconds out of range")
        for k in ("requests", "errors"):
            if not isinstance(o[k], int) or isinstance(o[k], bool) or o[k] < 0:
                raise ContractError(f"previous_outcome.{k} must be a non-negative integer")
        if o["errors"] > o["requests"]:
            raise ContractError("previous_outcome.errors exceeds requests")
        if not _is_num(o["p95_ms"]) or o["p95_ms"] < 0:
            raise ContractError("previous_outcome.p95_ms must be finite and >= 0")
        if not isinstance(o["clean_exposure"], bool):
            raise ContractError("previous_outcome.clean_exposure must be boolean")
        if not isinstance(o["backends"], list) or len(o["backends"]) > MAX_BACKENDS:
            raise ContractError("previous_outcome.backends invalid")
        obs = []
        for b in o["backends"]:
            _exact_keys("previous_outcome.backends[]", b, OUTCOME_BACKEND_KEYS)
            if (not valid_id(b["id"]) or not isinstance(b["requests"], int) or isinstance(b["requests"], bool)
                    or b["requests"] < 0 or not isinstance(b["healthy"], bool)
                    or not isinstance(b["weight"], int) or isinstance(b["weight"], bool) or b["weight"] < 1):
                raise ContractError("previous_outcome.backends entry invalid")
            obs.append((b["id"], b["requests"], b["healthy"], b["weight"]))
        outcome = Outcome(o["decision_id"], float(ow), o["requests"], o["errors"], float(o["p95_ms"]),
                          o["clean_exposure"], tuple(obs))

    return DecisionRequest(doc["decision_id"], observed, float(w), cur, tuple(allowed), gf,
                           tuple(ids), tuple(bf), outcome)


def validate_response(doc: dict, request_decision_id: Optional[str] = None) -> dict:
    """Validate a response document (used by tests and by Python-side clients)."""
    _exact_keys("response", doc, {"schema_version", "decision_id", "strategy", "model_version", "diagnostic"},
                RESPONSE_KEYS)
    if doc["schema_version"] != SCHEMA_VERSION or isinstance(doc["schema_version"], bool):
        raise ContractError(f"schema_version must be {SCHEMA_VERSION}")
    if not valid_id(doc["decision_id"]):
        raise ContractError("invalid decision_id")
    if request_decision_id is not None and doc["decision_id"] != request_decision_id:
        raise ContractError("stale decision: decision_id does not match the request")
    _strategy(doc["strategy"], "strategy")
    for k in ("model_version", "diagnostic"):
        if not isinstance(doc[k], str) or not 1 <= len(doc[k]) <= 64:
            raise ContractError(f"{k} must be 1..64 chars")
    for bid, w in (doc.get("weights") or {}).items():
        if not valid_id(bid) or not _is_num(w) or not 0 <= w <= 1:
            raise ContractError("weights invalid")
    if len(doc.get("weights") or {}) > MAX_BACKENDS:
        raise ContractError("weights too large")
    if "scores" in doc:
        sc = doc["scores"]
        if not isinstance(sc, list) or len(sc) != NUM_ACTIONS or any(
                s is not None and not _is_num(s) for s in sc):
            raise ContractError("scores must be 5 finite numbers or null")
    return doc
