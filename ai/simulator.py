"""Phase 9 - offline workload simulator.

A small, seeded queueing model that emits the SAME JSON contracts as the Go
service (/api/state State v1, /api/metrics, /api/policy), so the controller's
full parse/validate/reward path is exercised offline.

IMPORTANT LIMITATION: this is a *model*, not measured data. How each strategy
splits traffic and how backends degrade are assumptions (documented below), so
results say whether the learning logic works, NOT how the real system performs.
It makes no counterfactual claim about the real service.

Model (1 s ticks; 3 backends with declared weights 3:2:1, nominal 20 rps/weight):
  traffic split  RR: equal | LC: proportional to *true current* capacity (it sees
                 queues) but is attracted to an undetected failed backend (fast
                 errors => low connection count) | SWRR: declared weights |
                 CH: fixed random skew | Rule-adaptive: declared weights x
                 headroom x (1-error EMA), from lagged EMAs (slow reaction)
  backend        lambda < 0.95 mu : p95 = base + 3/(mu-lambda) (M/M/1 tail), no errors
                 otherwise        : saturated, served 0.95 mu, excess dropped as
                                    errors, p95 at the 2 s timeout
  failure        traffic keeps flowing to a failed backend until health checks
                 mark it down (10 s lag), all of it errors
"""
from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Callable, Dict, List, Optional, Sequence, Set

import numpy as np

from .actions import (ACTION_STRATEGIES, CH, LC, NUM_ACTIONS, RR, RULE_ADAPTIVE,
                      STATIC_SWRR, action_for_strategy)
from .controller import ActionRejected

BASE_EPOCH = datetime(2026, 1, 1, tzinfo=timezone.utc).timestamp()
BACKENDS = {"Backend-1": 3, "Backend-2": 2, "Backend-3": 1}
CAP_PER_WEIGHT = 20.0  # nominal rps per unit weight  (total nominal capacity 120 rps)
HEALTH_DETECT_LAG_S = 10.0
RECOVER_DETECT_LAG_S = 5.0
LATENCY_TIMEOUT_MS = 2000.0
LATENCY_SCALE_MS = 500.0
MAX_REF_RPS = 2000.0
CONN_LIMIT = 100.0
WINDOW_TICKS = 10


@dataclass
class Conditions:
    load_rps: float
    cap_mult: Dict[str, float] = field(default_factory=dict)  # true capacity multiplier
    lat_mult: Dict[str, float] = field(default_factory=dict)  # base latency multiplier
    failed: Set[str] = field(default_factory=set)
    cpu_extra: Dict[str, float] = field(default_factory=dict)  # e.g. noisy neighbour


@dataclass
class Scenario:
    name: str
    duration_s: float
    at: Callable[[float], Conditions]  # scenario-local time -> conditions


def _balanced(t):  # ~50% utilisation
    return Conditions(60.0)


def _cpu_imbalance(t):  # Backend-1 loses half its capacity to a noisy neighbour, CPU pinned high
    return Conditions(70.0, cap_mult={"Backend-1": 0.5}, cpu_extra={"Backend-1": 0.4})


def _latency_degradation(t):  # Backend-2 slows down mid-scenario, stays slow
    if t < 90:
        return Conditions(65.0)
    return Conditions(65.0, cap_mult={"Backend-2": 0.35}, lat_mult={"Backend-2": 3.0})


def _high_concurrency(t):  # ~95% of nominal capacity
    return Conditions(110.0)


def _traffic_spike(t):  # 40 rps with a 120 s spike to 100 rps
    return Conditions(100.0 if 90 <= t < 210 else 40.0)


def _backend_failure(t):  # Backend-2 down for 120 s
    return Conditions(70.0, failed={"Backend-2"} if 60 <= t < 180 else set())


SCENARIOS: Dict[str, Scenario] = {s.name: s for s in [
    Scenario("balanced", 360, _balanced),
    Scenario("cpu_imbalance", 360, _cpu_imbalance),
    Scenario("latency_degradation", 360, _latency_degradation),
    Scenario("high_concurrency", 360, _high_concurrency),
    Scenario("traffic_spike", 360, _traffic_spike),
    Scenario("backend_failure", 360, _backend_failure),
]}


def _ramp_up(t):  # Phase 20: load rises linearly 40 -> 118 rps over 240 s (~98% of capacity), then holds
    return Conditions(40.0 + (118.0 - 40.0) * min(t, 240.0) / 240.0)


def _oscillating(t):  # Phase 20: slow periodic load, 75 +- 40 rps, period 180 s
    return Conditions(75.0 + 40.0 * math.sin(2.0 * math.pi * t / 180.0))


# Trend-bearing scenarios for the predictive layer. Kept separate from SCENARIOS so that the Phase 9
# experiments (which iterate SCENARIOS) remain exactly reproducible.
TREND_SCENARIOS: Dict[str, Scenario] = {s.name: s for s in [
    Scenario("ramp_up", 360, _ramp_up),
    Scenario("oscillating", 360, _oscillating),
]}


def _iso(epoch: float) -> str:
    return datetime.fromtimestamp(epoch, tz=timezone.utc).isoformat().replace("+00:00", "Z")


def _weighted_quantile(pairs: Sequence[tuple], q: float) -> float:
    pairs = [(v, w) for v, w in pairs if w > 0]
    if not pairs:
        return 0.0
    pairs.sort()
    total, acc = sum(w for _, w in pairs), 0.0
    for v, w in pairs:
        acc += w
        if acc / total >= q:
            return v
    return pairs[-1][0]


class SimEnvironment:
    """Implements the controller's Environment protocol plus advance()."""

    def __init__(self, scenarios: Sequence[Scenario], seed: int = 0, noise: float = 1.0):
        self.scenarios = list(scenarios)
        self.total_duration = sum(s.duration_s for s in self.scenarios)
        self.rng = np.random.default_rng(seed)
        self.noise = noise
        self.ids = sorted(BACKENDS)
        self.t = 0.0
        self.strategy = ACTION_STRATEGIES[RR]
        self.switch_count = 0
        self.policy_version = 1
        self.affinity_allowed = False
        self.ch_skew = self.rng.dirichlet([2.0] * len(self.ids))
        # cumulative counters
        self.req = {b: 0.0 for b in self.ids}
        self.err = {b: 0.0 for b in self.ids}
        self.cum_req = self.cum_err = 0.0
        # health-detection bookkeeping and lagged EMAs (for the rule-based strategy)
        self.fail_since: Dict[str, Optional[float]] = {b: None for b in self.ids}
        self.ok_since: Dict[str, Optional[float]] = {b: None for b in self.ids}
        self.detected_down = {b: False for b in self.ids}
        self.util_ema = {b: 0.5 for b in self.ids}
        self.err_ema = {b: 0.0 for b in self.ids}
        self.window: deque = deque(maxlen=WINDOW_TICKS)  # per-tick records
        self.last: Dict[str, dict] = {}
        # fault injection for safety tests
        self.inject_stale = False
        self.inject_nan = False
        self.inject_bad_schema = False
        self.mask_override: Optional[Sequence[bool]] = None
        self.applied: List[tuple] = []  # (t, action, decision_id)

    # -------------------------------------------------------------- scenario time
    def scenario_at(self, t: Optional[float] = None) -> tuple:
        t = self.t if t is None else t
        local = t % self.total_duration
        for s in self.scenarios:
            if local < s.duration_s:
                return s, local
            local -= s.duration_s
        return self.scenarios[-1], 0.0

    # -------------------------------------------------------------------- shares
    def _shares(self, cond: Conditions, routable: List[str], mu_true: Dict[str, float]) -> Dict[str, float]:
        if not routable:
            return {}
        a = action_for_strategy(self.strategy)
        if a == RR:
            w = {b: 1.0 for b in routable}
        elif a == LC:
            w = {}
            for b in routable:
                w[b] = 2.0 * BACKENDS[b] * CAP_PER_WEIGHT if b in cond.failed else mu_true[b]
        elif a == STATIC_SWRR:
            w = {b: float(BACKENDS[b]) for b in routable}
        elif a == CH:
            w = {b: float(self.ch_skew[self.ids.index(b)]) for b in routable}
        else:  # RULE_ADAPTIVE
            w = {b: BACKENDS[b] * max(0.05, 1.0 - self.util_ema[b]) * max(0.05, 1.0 - min(1.0, 4 * self.err_ema[b]))
                 for b in routable}
        tot = sum(w.values())
        return {b: v / tot for b, v in w.items()}

    # ---------------------------------------------------------------------- tick
    def advance(self, dt: float = 1.0) -> None:
        n_ticks = int(round(dt))
        for _ in range(n_ticks):
            self._tick()

    def _tick(self) -> None:
        sc, local = self.scenario_at()
        cond = sc.at(local)
        self.t += 1.0
        now = self.t
        # health detection with lag
        for b in self.ids:
            if b in cond.failed:
                self.ok_since[b] = None
                self.fail_since[b] = self.fail_since[b] if self.fail_since[b] is not None else now
                if now - self.fail_since[b] >= HEALTH_DETECT_LAG_S:
                    self.detected_down[b] = True
            else:
                self.fail_since[b] = None
                if self.detected_down[b]:
                    self.ok_since[b] = self.ok_since[b] if self.ok_since[b] is not None else now
                    if now - self.ok_since[b] >= RECOVER_DETECT_LAG_S:
                        self.detected_down[b] = False
        routable = [b for b in self.ids if not self.detected_down[b]]
        mu_nom = {b: BACKENDS[b] * CAP_PER_WEIGHT for b in self.ids}
        mu_true = {b: mu_nom[b] * cond.cap_mult.get(b, 1.0) for b in self.ids}
        load = max(0.0, cond.load_rps * (1.0 + self.noise * self.rng.normal(0, 0.03)))
        shares = self._shares(cond, routable, mu_true)

        rec: Dict[str, dict] = {}
        for b in self.ids:
            lam = load * shares.get(b, 0.0)
            if b in cond.failed:
                served, errors, p95 = 0.0, lam, 5.0
            elif lam < 0.95 * mu_true[b]:
                base = 20.0 * cond.lat_mult.get(b, 1.0)
                p95 = min(LATENCY_TIMEOUT_MS, base + 3000.0 / max(mu_true[b] - lam, 1e-6))
                served, errors = lam, lam * 0.0005
            else:
                served, p95 = 0.95 * mu_true[b], LATENCY_TIMEOUT_MS
                errors = lam - served
            p95 *= math.exp(self.noise * self.rng.normal(0, 0.05))
            util = min(1.0, lam / mu_nom[b]) if mu_nom[b] else 0.0
            cpu = min(1.0, util + cond.cpu_extra.get(b, 0.0))
            conns = min(CONN_LIMIT, lam * p95 / 3000.0)
            rec[b] = dict(lam=lam, errors=errors, p95=p95, cpu=cpu, conns=conns,
                          healthy=not self.detected_down[b])
            self.req[b] += lam
            self.err[b] += errors
            self.util_ema[b] += 0.3 * (min(1.0, lam / max(mu_true[b], 1e-6)) - self.util_ema[b])
            self.err_ema[b] += 0.3 * ((errors / lam if lam > 0 else 0.0) - self.err_ema[b])
        self.cum_req = sum(self.req.values())
        self.cum_err = sum(self.err.values())
        self.last = rec
        self.window.append(rec)

    # ---------------------------------------------------------------- derived
    def _window_stats(self):
        pairs, reqs = [], 0.0
        for rec in self.window:
            for r in rec.values():
                pairs.append((r["p95"], r["lam"]))
                reqs += r["lam"]
        p95 = _weighted_quantile(pairs, 0.95)
        return p95, reqs / max(len(self.window), 1), reqs

    def _policy_mask(self) -> List[bool]:
        if self.mask_override is not None:
            return list(self.mask_override)
        return [True, True, True, self.affinity_allowed, True]

    # --------------------------------------------------------- Environment API
    def get_state(self) -> dict:
        if self.inject_bad_schema:
            return {"oops": 1}
        p95, rps, wreq = self._window_stats()
        valid = wreq >= 10
        vec = [
            math.log1p(rps) / math.log1p(MAX_REF_RPS),
            min(1.0, p95 / LATENCY_SCALE_MS),
            0.0,
            min(1.0, self.cum_err / self.cum_req) if self.cum_req > 0 else 0.0,
        ]
        names = ["global.rps_normalized", "global.request_duration_p95_normalized",
                 "global.long_request_ratio", "global.global_error_rate"]
        tot_w = sum(BACKENDS.values())
        for b in self.ids:
            r = self.last.get(b) or dict(lam=0, errors=0, p95=0, cpu=0, conns=0, healthy=True)
            brps = np.mean([rec[b]["lam"] for rec in self.window]) if self.window else 0.0
            bp95 = _weighted_quantile([(rec[b]["p95"], rec[b]["lam"]) for rec in self.window], 0.95)
            vec += [
                1.0 if r["healthy"] else 0.0,
                r["cpu"],
                0.4,
                r["conns"] / CONN_LIMIT,
                math.log1p(brps) / math.log1p(MAX_REF_RPS),
                min(1.0, bp95 / LATENCY_SCALE_MS),
                min(1.0, self.err[b] / self.req[b]) if self.req[b] > 0 else 0.0,
                BACKENDS[b] / tot_w,
            ]
            names += [f"backend[{b}].{f}" for f in (
                "healthy", "cpu_utilization", "memory_utilization", "active_connections_normalized",
                "backend_rps_normalized", "p95_latency_normalized", "error_rate", "capacity_ratio")]
        stale = self.inject_stale
        if self.inject_nan:
            vec[1] = float("nan")
        return {
            "state_version": "v1",
            "timestamp": _iso(BASE_EPOCH + self.t - (30 if stale else 0)),
            "snapshot_age_ms": 30000.0 if stale else 5.0,
            "max_allowed_age_ms": 5000.0,
            "is_stale": stale,
            "safe_policy_retain_recommended": stale or not valid,
            "staleness_reason": "snapshot age exceeds maximum" if stale else
                                ("" if valid else "insufficient samples"),
            "has_missing_critical_features": not valid,
            "ordered_backend_ids": list(self.ids),
            "flattened_vector": vec,
            "feature_names": names,
        }

    def get_metrics(self) -> dict:
        p95, _, wreq = self._window_stats()
        backends, caps = {}, {}
        for b in self.ids:
            r = self.last.get(b) or dict(healthy=True)
            bp95 = _weighted_quantile([(rec[b]["p95"], rec[b]["lam"]) for rec in self.window], 0.95)
            backends[b] = {"backend_id": b, "healthy": bool(r["healthy"]),
                           "total_requests": int(self.req[b]), "total_errors": int(self.err[b]),
                           "latency": {"p95_ms": bp95}}
            caps[b] = {"static_weight": BACKENDS[b]}
        return {
            "timestamp": _iso(BASE_EPOCH + self.t),
            "is_valid": wreq >= 10,
            "global_traffic": {"total_requests": int(self.cum_req), "total_errors": int(self.cum_err),
                               "duration_stats": {"p95_ms": p95}},
            "backends": backends,
            "capacity_configs": caps,
            "controller": {"current_strategy": self.strategy, "switch_count": self.switch_count,
                           "policy_version": f"v{self.policy_version}"},
        }

    def get_policy(self) -> dict:
        return {"policy": {"strategy": self.strategy, "version": self.policy_version},
                "action_mask": self._policy_mask()}

    def apply_action(self, action: int, decision_id: str) -> None:
        mask = self._policy_mask()
        if not (0 <= action < NUM_ACTIONS) or not mask[action]:
            raise ActionRejected(f"action {action} invalid")
        self.strategy = ACTION_STRATEGIES[action]
        self.switch_count += 1
        self.policy_version += 1
        self.applied.append((self.t, action, decision_id))

    # the controller needs the metrics timestamp, so expose the clock for drivers
    def now(self) -> float:
        return BASE_EPOCH + self.t


def run_controller(env: SimEnvironment, controller, seconds: float, poll_s: float = 2.0) -> List[dict]:
    """Drive env + controller on a virtual clock; returns the controller's decision log records."""
    steps = int(seconds / poll_s)
    for _ in range(steps):
        env.advance(poll_s)
        controller.step(env)
    return controller.log.records
