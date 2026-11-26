"""State validation and context construction for the learner (State v1 contract)."""
from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import List, Optional

import numpy as np

from .schemas import StateSnapshot

EXPECTED_STATE_VERSION = "v1"
GLOBAL_FEATURES = 4
PER_BACKEND_FEATURES = 8
# per-backend feature offsets (State v1)
F_HEALTHY, F_CPU, F_MEM, F_CONNS, F_RPS, F_P95, F_ERR, F_CAP = range(8)

DEFAULT_MAX_AGE_MS = 5000.0
_TOL = 1e-9


def expected_dim(n_backends: int) -> int:
    return GLOBAL_FEATURES + PER_BACKEND_FEATURES * n_backends


@dataclass
class ValidationResult:
    ok: bool
    reasons: List[str] = field(default_factory=list)

    @property
    def reason(self) -> str:
        return "; ".join(self.reasons)


def validate_state(
    s: StateSnapshot,
    n_backends: Optional[int] = None,
    max_age_ms: float = DEFAULT_MAX_AGE_MS,
    backend_ids: Optional[tuple] = None,
) -> ValidationResult:
    """A state is usable only if every check passes; otherwise the controller must
    retain the last safe policy and must not explore or learn from it."""
    r: List[str] = []
    if s.state_version != EXPECTED_STATE_VERSION:
        r.append(f"state_version {s.state_version!r} != {EXPECTED_STATE_VERSION!r}")
    n = len(s.backend_ids)
    if len(s.vector) != expected_dim(n):
        r.append(f"vector length {len(s.vector)} != expected {expected_dim(n)} for {n} backends")
    if len(s.feature_names) != len(s.vector):
        r.append("feature_names length does not match vector length")
    if list(s.backend_ids) != sorted(s.backend_ids):
        r.append("backend ids are not in stable sorted order")
    if n_backends is not None and n != n_backends:
        r.append(f"backend count {n} != configured {n_backends}")
    if backend_ids is not None and tuple(s.backend_ids) != tuple(backend_ids):
        r.append("backend id set changed since the model was created")
    bad = [i for i, x in enumerate(s.vector) if not math.isfinite(x) or x < -_TOL or x > 1.0 + _TOL]
    if bad:
        r.append(f"{len(bad)} feature(s) non-finite or outside [0,1] (first index {bad[0]})")
    if s.is_stale or s.snapshot_age_ms > max_age_ms:
        r.append(f"stale snapshot (age {s.snapshot_age_ms:.0f} ms)")
    if s.safe_policy_retain:
        r.append("go service recommends retaining the safe policy" + (f": {s.reason}" if s.reason else ""))
    if s.missing_critical:
        r.append("critical features missing")
    return ValidationResult(ok=not r, reasons=r)


CONTEXT_MODES = ("full", "compact")


def context_dim(mode: str, n_backends: int) -> int:
    if mode == "full":
        return expected_dim(n_backends) + 1
    if mode == "compact":
        return GLOBAL_FEATURES + 10 + 1
    raise ValueError(f"unknown context mode {mode!r}")


def build_context(s: StateSnapshot, mode: str = "compact") -> np.ndarray:
    """Numeric context for the learner, with a trailing bias term of 1.0.

    full:    the whole State v1 vector (4 + 8n features).
    compact: 4 global features + 10 aggregates over backends. Independent of n,
             far fewer parameters to learn from a few hundred decisions.
    """
    v = np.asarray(s.vector, dtype=float)
    if mode == "full":
        return np.append(v, 1.0)
    if mode != "compact":
        raise ValueError(f"unknown context mode {mode!r}")
    n = len(s.backend_ids)
    pb = v[GLOBAL_FEATURES:].reshape(n, PER_BACKEND_FEATURES)
    agg = [
        pb[:, F_HEALTHY].min(),
        pb[:, F_HEALTHY].mean(),
        pb[:, F_CPU].mean(),
        pb[:, F_CPU].max(),
        pb[:, F_CONNS].mean(),
        pb[:, F_CONNS].max(),
        pb[:, F_P95].mean(),
        pb[:, F_P95].max(),
        pb[:, F_ERR].max(),
        pb[:, F_RPS].std(),
    ]
    return np.concatenate([v[:GLOBAL_FEATURES], np.asarray(agg, dtype=float), [1.0]])
