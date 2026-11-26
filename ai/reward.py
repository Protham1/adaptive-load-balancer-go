"""Phase 7 - reward function.

    R = wT*T_norm - wL*L95_norm - wE*E_norm - wV*V_norm          (weights sum to 1.0)

T_norm   completed requests/s over the evaluation window / reference throughput, clipped to [0,1]
L95_norm window p95 latency / latency scale (500 ms, same as State v1), clipped to [0,1]
E_norm   error fraction of requests completed in the window, in [0,1]
V_norm   load imbalance = (1 - J) / (1 - 1/n), J = Jain's fairness index of the
         capacity-adjusted service rate x_i = (requests_i / dt) / declared_capacity_i
         over backends healthy at both ends of the window; in [0,1] (0 for n < 2)

Normal reward range: [-(wL+wE+wV), wT] = [-0.75, +0.25]. Safety override
(severe error rate or no healthy backend) returns safety_penalty = -1.0, strictly
below any non-override reward, so it can never be "outbid" by throughput.

Correlation note (no double counting): T counts *completed* requests, errors
included, so a failing backend does not lower T *and* raise E for the same
request; errors are penalised once, through E. Latency and imbalance are
correlated under saturation (a saturated backend has both high p95 and a skewed
load); both stay in because they answer different questions (tail experience vs
fairness to declared capacity) - the sensitivity analysis quantifies how much the
ranking depends on that choice.

Every component is returned in RewardBreakdown so it can be logged.
"""
from __future__ import annotations

import math
import random
from dataclasses import asdict, dataclass, field
from typing import Dict, List, Mapping, Optional, Sequence

from .schemas import MetricsSample


@dataclass(frozen=True)
class RewardConfig:
    w_throughput: float = 0.25
    w_latency: float = 0.35
    w_error: float = 0.25
    w_imbalance: float = 0.15
    ref_throughput_rps: float = 100.0  # set from the development workload's peak
    latency_scale_ms: float = 500.0
    severe_error_rate: float = 0.10
    safety_penalty: float = -1.0
    min_requests: int = 20  # below this the window is too small to judge

    def weights(self) -> Dict[str, float]:
        return {
            "throughput": self.w_throughput,
            "latency": self.w_latency,
            "error": self.w_error,
            "imbalance": self.w_imbalance,
        }

    def validate(self) -> "RewardConfig":
        w = self.weights().values()
        if any(x < 0 for x in w) or abs(sum(w) - 1.0) > 1e-9:
            raise ValueError(f"reward weights must be >= 0 and sum to 1.0, got {self.weights()}")
        if self.ref_throughput_rps <= 0 or self.latency_scale_ms <= 0:
            raise ValueError("reference throughput and latency scale must be positive")
        if not (0 < self.severe_error_rate <= 1) or self.safety_penalty >= -(1 - self.w_throughput):
            raise ValueError("safety penalty must be below every non-override reward")
        return self

    def with_weights(self, wt: float, wl: float, we: float, wv: float) -> "RewardConfig":
        s = wt + wl + we + wv
        return RewardConfig(**{**asdict(self), "w_throughput": wt / s, "w_latency": wl / s,
                               "w_error": we / s, "w_imbalance": wv / s})


@dataclass(frozen=True)
class WindowOutcome:
    """Observed performance over one exact evaluation window (counter deltas)."""

    duration_s: float
    requests: int
    errors: int
    p95_ms: float
    backend_requests: Mapping[str, int]
    backend_capacity: Mapping[str, float]
    healthy_at_start: Mapping[str, bool]
    healthy_at_end: Mapping[str, bool]


def outcome_from_samples(start: MetricsSample, end: MetricsSample) -> WindowOutcome:
    """Build the outcome from two snapshots. Raises ValueError if counters went
    backwards (service restart) or time did not advance: the window is unusable."""
    dt = end.t - start.t
    if dt <= 0:
        raise ValueError("evaluation window has non-positive duration")
    dreq, derr = end.requests - start.requests, end.errors - start.errors
    if dreq < 0 or derr < 0:
        raise ValueError("global counters decreased (restart?)")
    breq, cap, h0, h1 = {}, {}, {}, {}
    for bid, b1 in end.backends.items():
        b0 = start.backends.get(bid)
        if b0 is None:
            continue  # backend added mid-window: excluded
        d = b1.requests - b0.requests
        if d < 0:
            raise ValueError(f"counter of {bid} decreased")
        breq[bid], cap[bid] = d, float(b1.weight)
        h0[bid], h1[bid] = b0.healthy, b1.healthy
    return WindowOutcome(dt, dreq, derr, end.p95_ms, breq, cap, h0, h1)


def jain_index(x: Sequence[float]) -> float:
    """J = (sum x)^2 / (n * sum x^2); 1.0 = perfectly even. Defined as 1.0 when all x are 0."""
    n = len(x)
    if n == 0:
        return 1.0
    s2 = sum(v * v for v in x)
    if s2 == 0:
        return 1.0
    return (sum(x) ** 2) / (n * s2)


def capacity_adjusted_rates(o: WindowOutcome) -> Dict[str, float]:
    """x_i = (requests_i / dt) / declared_capacity_i for backends healthy at both ends."""
    return {
        b: (o.backend_requests[b] / o.duration_s) / max(o.backend_capacity[b], 1e-9)
        for b in o.backend_requests
        if o.healthy_at_start.get(b) and o.healthy_at_end.get(b)
    }


@dataclass
class RewardBreakdown:
    reward: Optional[float]  # None => window invalid, do not learn from it
    valid: bool
    override: Optional[str] = None  # "severe_error_rate" | "no_healthy_backend" | None
    reason: str = ""
    raw: Dict[str, float] = field(default_factory=dict)
    normalized: Dict[str, float] = field(default_factory=dict)
    weights: Dict[str, float] = field(default_factory=dict)
    contributions: Dict[str, float] = field(default_factory=dict)  # signed terms of the sum

    def to_dict(self) -> dict:
        return asdict(self)


def _clip01(v: float) -> float:
    return 0.0 if v < 0 else 1.0 if v > 1 else v


def compute_reward(o: WindowOutcome, cfg: RewardConfig = RewardConfig()) -> RewardBreakdown:
    cfg.validate()
    n_total = len(o.healthy_at_end)
    n_healthy_end = sum(1 for v in o.healthy_at_end.values() if v)
    err_rate = (o.errors / o.requests) if o.requests > 0 else 0.0
    thr = o.requests / o.duration_s if o.duration_s > 0 else 0.0
    rates = capacity_adjusted_rates(o)
    n = len(rates)
    jain = jain_index(list(rates.values()))
    imbalance = _clip01((1.0 - jain) / (1.0 - 1.0 / n)) if n >= 2 else 0.0

    raw = {
        "throughput_rps": thr,
        "p95_ms": o.p95_ms,
        "error_rate": err_rate,
        "jain_index": jain,
        "backends_total": float(n_total),
        "backends_healthy_end": float(n_healthy_end),
        "backends_in_fairness_set": float(n),
        "requests": float(o.requests),
        "duration_s": o.duration_s,
    }
    w = cfg.weights()
    norm = {
        "throughput": _clip01(thr / cfg.ref_throughput_rps),
        "latency": _clip01(o.p95_ms / cfg.latency_scale_ms),
        "error": _clip01(err_rate),
        "imbalance": imbalance,
    }

    # Safety overrides come first: they must dominate regardless of throughput.
    if n_total > 0 and n_healthy_end == 0:
        return RewardBreakdown(cfg.safety_penalty, True, "no_healthy_backend",
                               "no healthy backend at end of window", raw, norm, w, {})
    if o.requests < cfg.min_requests:
        return RewardBreakdown(None, False, None,
                               f"only {o.requests} requests (< {cfg.min_requests}): window too small",
                               raw, norm, w, {})
    if err_rate >= cfg.severe_error_rate:
        return RewardBreakdown(cfg.safety_penalty, True, "severe_error_rate",
                               f"error rate {err_rate:.3f} >= {cfg.severe_error_rate}", raw, norm, w, {})

    contrib = {
        "throughput": w["throughput"] * norm["throughput"],
        "latency": -w["latency"] * norm["latency"],
        "error": -w["error"] * norm["error"],
        "imbalance": -w["imbalance"] * norm["imbalance"],
    }
    return RewardBreakdown(sum(contrib.values()), True, None, "", raw, norm, w, contrib)


# --------------------------------------------------------------------------- sensitivity
def _perturbed_configs(base: RewardConfig, rel: float, n_random: int, seed: int) -> List[RewardConfig]:
    ws = [base.w_throughput, base.w_latency, base.w_error, base.w_imbalance]
    out = []
    for i in range(4):  # one weight at a time, +/- rel, renormalised
        for sign in (-1, 1):
            p = list(ws)
            p[i] *= 1 + sign * rel
            out.append(base.with_weights(*p))
    rng = random.Random(seed)
    for _ in range(n_random):  # joint random perturbations
        out.append(base.with_weights(*[w * (1 + rng.uniform(-rel, rel)) for w in ws]))
    return out


def sensitivity_analysis(
    outcomes_by_action: Mapping[int, Sequence[WindowOutcome]],
    base: RewardConfig = RewardConfig(),
    rel: float = 0.25,
    n_random: int = 200,
    seed: int = 0,
    stable_threshold: float = 0.9,
) -> dict:
    """How much does the *ranking of actions* depend on the reward weights?

    Recomputes the mean reward of every action on identical logged outcomes under
    perturbed weights (each weight +/-rel, plus joint random perturbations).
    Reports the fraction of configurations that keep the same best action and the
    mean pairwise-order agreement. If agreement is below stable_threshold the
    result must be reported as weight-sensitive.
    """
    def means(cfg):
        m = {}
        for a, outs in outcomes_by_action.items():
            vals = [b.reward for b in (compute_reward(o, cfg) for o in outs) if b.reward is not None]
            if vals:
                m[a] = sum(vals) / len(vals)
        return m

    base_means = means(base)
    if len(base_means) < 2:
        return {"actions": sorted(base_means), "note": "fewer than two actions with valid outcomes"}
    best0 = max(base_means, key=base_means.get)
    pairs = [(a, b) for a in base_means for b in base_means if a < b]

    cfgs = _perturbed_configs(base, rel, n_random, seed)
    same_best, agree = 0, 0.0
    best_counts: Dict[int, int] = {}
    for c in cfgs:
        m = means(c)
        best = max(m, key=m.get)
        best_counts[best] = best_counts.get(best, 0) + 1
        same_best += best == best0
        agree += sum((base_means[a] - base_means[b]) * (m[a] - m[b]) > 0 for a, b in pairs) / len(pairs)
    n = len(cfgs)
    best_frac, pair_frac = same_best / n, agree / n
    stable = best_frac >= stable_threshold and pair_frac >= stable_threshold
    return {
        "relative_perturbation": rel,
        "configs_tested": n,
        "base_mean_reward": base_means,
        "base_best_action": best0,
        "best_action_unchanged_fraction": best_frac,
        "pairwise_order_agreement": pair_frac,
        "best_action_counts": best_counts,
        "stable": stable,
        "limitation": None if stable else (
            "Action ranking changes materially under small reward-weight perturbations; "
            "conclusions are weight-sensitive and must be reported as such."),
    }
