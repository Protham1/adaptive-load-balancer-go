"""Phase 20 - optional predictive layer (an extension, not a prerequisite).

Simple, cheap forecasters over irregularly spaced observations (t seconds, value):

  MovingAverage(window_s)         flat forecast = mean of the last window_s seconds
  EMATrend(alpha, beta)           Holt's linear exponential smoothing (level + trend per second)
  LinearRegression(window_s)      OLS line over the last window_s seconds, extrapolated

Every forecaster is CAUSAL: `forecast(h)` only uses observations passed to `update` so far (no look-ahead;
tests poison the future to prove it). `PredictiveContext` appends the forecasts of the key workload signals
(request rate, p95 latency, error rate) to the bandit's context so the learner can act BEFORE a trend arrives.

A forecast is only useful if acting on it improves the load-balancing objective. Forecast accuracy is
therefore reported separately from, and never substituted for, the control comparison
(ai/predict_eval.py: predictive vs reactive controller on identical scenarios and seeds).
"""
from __future__ import annotations

import math
from collections import deque
from typing import Deque, List, Optional, Sequence, Tuple

import numpy as np


class Forecaster:
    name = "forecaster"

    def update(self, t: float, y: float) -> None:
        raise NotImplementedError

    def forecast(self, horizon_s: float) -> Optional[float]:
        """Predicted value `horizon_s` seconds after the last observation (None before any data)."""
        raise NotImplementedError

    def reset(self) -> None:
        raise NotImplementedError

    def params(self) -> dict:
        return {}


class Persistence(Forecaster):
    """Naive baseline: the future equals the last observation."""

    name = "persistence"

    def __init__(self):
        self.last: Optional[float] = None

    def update(self, t, y):
        self.last = float(y)

    def forecast(self, horizon_s):
        return self.last

    def reset(self):
        self.last = None


class MovingAverage(Forecaster):
    name = "ma"

    def __init__(self, window_s: float = 90.0):
        self.window_s = window_s
        self.buf: Deque[Tuple[float, float]] = deque()

    def update(self, t, y):
        self.buf.append((float(t), float(y)))
        while self.buf and t - self.buf[0][0] > self.window_s:
            self.buf.popleft()

    def forecast(self, horizon_s):
        return float(np.mean([v for _, v in self.buf])) if self.buf else None

    def reset(self):
        self.buf.clear()

    def params(self):
        return {"window_s": self.window_s}


class EMATrend(Forecaster):
    """Holt's linear method on irregular time steps: level and a per-second trend, both exponentially smoothed."""

    name = "ema"

    def __init__(self, alpha: float = 0.5, beta: float = 0.3):
        if not (0 < alpha <= 1 and 0 < beta <= 1):
            raise ValueError("alpha and beta must be in (0, 1]")
        self.alpha, self.beta = alpha, beta
        self.reset()

    def reset(self):
        self.level: Optional[float] = None
        self.trend = 0.0
        self.t_last: Optional[float] = None

    def update(self, t, y):
        y = float(y)
        if self.level is None:
            self.level, self.t_last = y, float(t)
            return
        dt = max(float(t) - self.t_last, 1e-9)
        pred = self.level + self.trend * dt
        new_level = self.alpha * y + (1 - self.alpha) * pred
        self.trend = self.beta * (new_level - self.level) / dt + (1 - self.beta) * self.trend
        self.level, self.t_last = new_level, float(t)

    def forecast(self, horizon_s):
        return None if self.level is None else self.level + self.trend * horizon_s

    def params(self):
        return {"alpha": self.alpha, "beta": self.beta}


class LinearRegression(Forecaster):
    name = "linreg"

    def __init__(self, window_s: float = 120.0, min_points: int = 3):
        self.window_s, self.min_points = window_s, min_points
        self.buf: Deque[Tuple[float, float]] = deque()

    def update(self, t, y):
        self.buf.append((float(t), float(y)))
        while self.buf and t - self.buf[0][0] > self.window_s:
            self.buf.popleft()

    def forecast(self, horizon_s):
        if not self.buf:
            return None
        if len(self.buf) < self.min_points:
            return self.buf[-1][1]
        t = np.array([p[0] for p in self.buf])
        y = np.array([p[1] for p in self.buf])
        tc = t - t[-1]
        denom = float(((tc - tc.mean()) ** 2).sum())
        if denom < 1e-12:
            return float(y[-1])
        slope = float(((tc - tc.mean()) * (y - y.mean())).sum() / denom)
        intercept = float(y.mean() - slope * tc.mean())
        return intercept + slope * horizon_s  # tc = 0 is the last observation

    def reset(self):
        self.buf.clear()

    def params(self):
        return {"window_s": self.window_s, "min_points": self.min_points}


def make_forecaster(kind: str, **kw) -> Forecaster:
    kinds = {"persistence": Persistence, "ma": MovingAverage, "ema": EMATrend, "linreg": LinearRegression}
    if kind not in kinds:
        raise ValueError(f"unknown predictor {kind!r}")
    return kinds[kind](**kw)


# ----------------------------------------------------------------- context augmentation
_SIGNALS = (("forecast.rps_normalized", 0), ("forecast.request_duration_p95_normalized", 1),
            ("forecast.global_error_rate", 3))  # (feature name, index in the compact context)


class PredictiveContext:
    """Appends forecasts of rps / p95 / error rate (clipped to [0,1]) before the bias term of the compact context."""

    def __init__(self, kind: str = "ema", **params):
        self.kind, self.params = kind, params
        self.fc = [make_forecaster(kind, **params) for _ in _SIGNALS]
        self.last_forecast: Optional[List[float]] = None

    @staticmethod
    def feature_names() -> List[str]:
        return [n for n, _ in _SIGNALS]

    def reset(self) -> None:
        for f in self.fc:
            f.reset()
        self.last_forecast = None

    def augment(self, context: np.ndarray, t: float, horizon_s: float) -> np.ndarray:
        """Update with the current observation, then append the forecast for t + horizon_s."""
        out = []
        for f, (_, i) in zip(self.fc, _SIGNALS):
            f.update(t, float(context[i]))
            v = f.forecast(horizon_s)
            out.append(float(np.clip(context[i] if v is None or math.isnan(v) else v, 0.0, 1.0)))
        self.last_forecast = out
        return np.concatenate([context[:-1], np.asarray(out, dtype=float), context[-1:]])

    def describe(self) -> dict:
        return {"kind": self.kind, "params": self.params, "features": self.feature_names(),
                "note": "forecasts are appended to the context; they are model inputs, not a confidence"}
