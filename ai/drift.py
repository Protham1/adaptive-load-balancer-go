"""Concept-drift tracking (Phase 19): recent reward and workload distribution.

A large, sustained shift in either signal means the model's past experience may no longer describe the
system. The monitor only FLAGS drift (logged, exposed at /v1/learner, shown in the dashboard); it never
changes the policy by itself. The operator (or the experiment) decides: keep going, or reset.

* Reward drift:   Page-Hinkley test on the reward stream (detects a sustained DROP of the mean, the case
                  that matters: the chosen actions got worse). Parameters delta (tolerated drift) and
                  lambda_ (alarm threshold) are in reward units; rewards live in [-1, 0.25].
* Workload drift: Population Stability Index (PSI) between a frozen REFERENCE window (the first
                  `ref_n` observations) and the most recent `recent_n` observations, per key feature
                  (rps, p95, error rate) with 5 equal-width bins on [0,1] and Laplace smoothing. Conventional reading:
                  PSI < 0.1 stable, 0.1-0.25 moderate, > 0.25 large shift (flagged).
"""
from __future__ import annotations

import math
from collections import deque
from typing import Deque, Dict, List, Optional, Sequence

import numpy as np

KEY_FEATURES = {"rps": 0, "p95": 1, "error_rate": 3}  # indices into the compact context (global features)
BINS = 5            # few bins: PSI is noisy with tens of samples (expected noise ~ (bins-1)(1/n1+1/n2))
PSEUDO = 0.5        # Laplace pseudo-count per bin: empty bins must not dominate small samples


def psi(reference: Sequence[float], recent: Sequence[float], bins: int = BINS) -> float:
    """Population Stability Index of two samples on [0,1] (equal-width bins, epsilon-smoothed)."""
    if len(reference) == 0 or len(recent) == 0:
        return float("nan")
    edges = np.linspace(0.0, 1.0, bins + 1)
    rc = np.histogram(np.clip(reference, 0, 1), bins=edges)[0] + PSEUDO
    cc = np.histogram(np.clip(recent, 0, 1), bins=edges)[0] + PSEUDO
    r, c = rc / rc.sum(), cc / cc.sum()
    return float(np.sum((c - r) * np.log(c / r)))


class PageHinkley:
    """One-sided Page-Hinkley test for a decrease of the mean."""

    def __init__(self, delta: float = 0.02, lambda_: float = 0.5, min_n: int = 10):
        self.delta, self.lambda_, self.min_n = delta, lambda_, min_n
        self.reset()

    def reset(self):
        self.n, self.mean, self.cum, self.cum_max, self.alarm = 0, 0.0, 0.0, 0.0, False

    def update(self, x: float) -> bool:
        self.n += 1
        self.mean += (x - self.mean) / self.n
        # cumulative sum of (mean - x - delta): grows when x falls below its running mean
        self.cum += (self.mean - x - self.delta)
        self.cum_min = getattr(self, "cum_min", 0.0)
        self.cum_min = min(self.cum_min, self.cum)
        if self.n >= self.min_n and (self.cum - self.cum_min) > self.lambda_:
            self.alarm = True
        return self.alarm

    def state(self) -> dict:
        return {"n": self.n, "mean": self.mean, "cum": self.cum, "cum_min": getattr(self, "cum_min", 0.0),
                "alarm": self.alarm}

    def load(self, d: dict):
        self.n, self.mean, self.cum = int(d["n"]), float(d["mean"]), float(d["cum"])
        self.cum_min, self.alarm = float(d["cum_min"]), bool(d["alarm"])


class DriftMonitor:
    def __init__(self, ref_n: int = 30, recent_n: int = 30, psi_threshold: float = 0.25,
                 ph_delta: float = 0.02, ph_lambda: float = 0.5):
        self.ref_n, self.recent_n, self.psi_threshold = ref_n, recent_n, psi_threshold
        self.ref: Dict[str, List[float]] = {k: [] for k in KEY_FEATURES}
        self.recent: Dict[str, Deque[float]] = {k: deque(maxlen=recent_n) for k in KEY_FEATURES}
        self.ph = PageHinkley(ph_delta, ph_lambda)
        self.rewards: Deque[float] = deque(maxlen=200)
        self.observations = 0

    def observe_context(self, context: Sequence[float]) -> None:
        for name, i in KEY_FEATURES.items():
            v = float(context[i])
            if len(self.ref[name]) < self.ref_n:
                self.ref[name].append(v)
            else:
                self.recent[name].append(v)
        self.observations += 1

    def observe_reward(self, reward: float) -> None:
        self.rewards.append(float(reward))
        self.ph.update(float(reward))

    def status(self) -> dict:
        psis = {}
        for k in KEY_FEATURES:
            ready = len(self.ref[k]) >= self.ref_n and len(self.recent[k]) >= max(5, self.recent_n // 2)
            psis[k] = psi(self.ref[k], list(self.recent[k])) if ready else None
        reasons = []
        for k, v in psis.items():
            if v is not None and not math.isnan(v) and v > self.psi_threshold:
                reasons.append(f"workload shift in {k} (PSI {v:.2f} > {self.psi_threshold})")
        if self.ph.alarm:
            reasons.append(f"sustained reward drop (Page-Hinkley alarm, mean {self.ph.mean:+.3f})")
        r = list(self.rewards)
        return {"drift_suspected": bool(reasons), "reasons": reasons, "psi": psis,
                "page_hinkley": self.ph.state(), "observations": self.observations,
                "reward_recent_mean": float(np.mean(r[-self.recent_n:])) if r else None,
                "reward_reference_mean": float(np.mean(r[:self.ref_n])) if len(r) >= self.ref_n else None,
                "psi_threshold": self.psi_threshold, "reference_filled": len(self.ref["rps"]) >= self.ref_n}

    def reset(self) -> None:
        self.__init__(self.ref_n, self.recent_n, self.psi_threshold, self.ph.delta, self.ph.lambda_)

    # persistence (so that a restart does not forget the reference window)
    def state_dict(self) -> dict:
        return {"ref": self.ref, "recent": {k: list(v) for k, v in self.recent.items()}, "ph": self.ph.state(),
                "rewards": list(self.rewards), "observations": self.observations}

    def load_state_dict(self, d: dict) -> None:
        self.ref = {k: [float(x) for x in d["ref"][k]] for k in KEY_FEATURES}
        self.recent = {k: deque((float(x) for x in d["recent"][k]), maxlen=self.recent_n) for k in KEY_FEATURES}
        self.ph.load(d["ph"])
        self.rewards = deque((float(x) for x in d["rewards"]), maxlen=200)
        self.observations = int(d["observations"])
