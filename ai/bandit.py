"""Phase 8 - bandit learners.

Contextual (use the state features):
  LinUCB               disjoint linear model per action + uncertainty bonus
  LinearEpsilonGreedy  disjoint ridge regression per action, epsilon-greedy on the predictions

NOT contextual (ignore the state; baselines only, never described as contextual):
  MeanEpsilonGreedy    per-action running mean reward
  RandomPolicy, FixedPolicy

All learners select only among allowed actions (mask) and never return an
invalid one. Ridge inverses are maintained with Sherman-Morrison (O(d^2)/update).
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Dict, List, Optional, Sequence

import numpy as np

from .actions import NUM_ACTIONS, normalize_mask


@dataclass
class Selection:
    action: int
    scores: List[Optional[float]]  # per-action score (None = masked); for logging
    explored: bool


class NoAllowedAction(RuntimeError):
    pass


class Learner:
    name = "learner"
    contextual = False

    def select(self, x: np.ndarray, mask: Sequence[bool], rng: np.random.Generator) -> Selection:
        raise NotImplementedError

    def update(self, x: np.ndarray, action: int, reward: float) -> None:
        raise NotImplementedError

    def config(self) -> dict:
        return {}

    def state_dict(self) -> dict:
        return {}

    def load_state_dict(self, d: dict) -> None:
        pass

    @staticmethod
    def _allowed(mask) -> List[int]:
        allowed = [a for a, ok in enumerate(normalize_mask(mask)) if ok]
        if not allowed:
            raise NoAllowedAction("no allowed action")
        return allowed


def _argmax_random_tie(values: Dict[int, float], rng: np.random.Generator) -> int:
    best = max(values.values())
    ties = [a for a, v in values.items() if v >= best - 1e-12]
    return int(ties[int(rng.integers(len(ties)))])


def _scores_list(values: Dict[int, float]) -> List[Optional[float]]:
    return [float(values[a]) if a in values else None for a in range(NUM_ACTIONS)]


# ----------------------------------------------------------------------------- baselines
class RandomPolicy(Learner):
    name = "random"

    def select(self, x, mask, rng):
        a = self._allowed(mask)
        return Selection(int(a[int(rng.integers(len(a)))]), [None] * NUM_ACTIONS, True)

    def update(self, x, action, reward):
        pass


class FixedPolicy(Learner):
    """Always plays one action (e.g. always-RR); falls back to a valid one if masked."""

    def __init__(self, action: int):
        self.action = action
        self.name = f"always-{action}"

    def config(self):
        return {"action": self.action}

    def select(self, x, mask, rng):
        allowed = self._allowed(mask)
        a = self.action if self.action in allowed else allowed[0]
        return Selection(a, [None] * NUM_ACTIONS, False)

    def update(self, x, action, reward):
        pass


class MeanEpsilonGreedy(Learner):
    """Plain action-average epsilon-greedy. It ignores the state: NOT contextual."""

    name = "mean-eps-greedy"
    contextual = False

    def __init__(self, epsilon: float = 0.1, min_pulls: int = 1):
        self.epsilon, self.min_pulls = epsilon, min_pulls
        self.n = np.zeros(NUM_ACTIONS)
        self.mean = np.zeros(NUM_ACTIONS)

    def config(self):
        return {"epsilon": self.epsilon, "min_pulls": self.min_pulls}

    def select(self, x, mask, rng):
        allowed = self._allowed(mask)
        scores = {a: float(self.mean[a]) for a in allowed}
        untried = [a for a in allowed if self.n[a] < self.min_pulls]
        if untried:
            return Selection(int(untried[int(rng.integers(len(untried)))]), _scores_list(scores), True)
        if rng.random() < self.epsilon:
            return Selection(int(allowed[int(rng.integers(len(allowed)))]), _scores_list(scores), True)
        return Selection(_argmax_random_tie(scores, rng), _scores_list(scores), False)

    def update(self, x, action, reward):
        self.n[action] += 1
        self.mean[action] += (reward - self.mean[action]) / self.n[action]

    def state_dict(self):
        return {"n": self.n.tolist(), "mean": self.mean.tolist()}

    def load_state_dict(self, d):
        self.n, self.mean = np.array(d["n"], float), np.array(d["mean"], float)


# ------------------------------------------------------------------------- contextual
class _PerActionRidge:
    """Per-action ridge regression: theta = A^-1 b with A = lambda*I + sum x x^T."""

    def __init__(self, dim: int, ridge: float):
        self.dim = dim
        self.A_inv = np.stack([np.eye(dim) / ridge for _ in range(NUM_ACTIONS)])
        self.b = np.zeros((NUM_ACTIONS, dim))
        self.n = np.zeros(NUM_ACTIONS)

    def theta(self, a: int) -> np.ndarray:
        return self.A_inv[a] @ self.b[a]

    def predict(self, a: int, x: np.ndarray) -> float:
        return float(self.theta(a) @ x)

    def uncertainty(self, a: int, x: np.ndarray) -> float:
        return float(np.sqrt(max(x @ self.A_inv[a] @ x, 0.0)))

    def update(self, a: int, x: np.ndarray, r: float) -> None:
        Ax = self.A_inv[a] @ x
        self.A_inv[a] -= np.outer(Ax, Ax) / (1.0 + x @ Ax)
        self.b[a] += r * x
        self.n[a] += 1

    def state_dict(self):
        return {"A_inv": self.A_inv.tolist(), "b": self.b.tolist(), "n": self.n.tolist()}

    def load_state_dict(self, d):
        self.A_inv, self.b, self.n = np.array(d["A_inv"]), np.array(d["b"]), np.array(d["n"])
        if self.A_inv.shape != (NUM_ACTIONS, self.dim, self.dim):
            raise ValueError("model dimension does not match")


class LinUCB(Learner):
    """Disjoint LinUCB: argmax_a  theta_a.x + alpha * sqrt(x' A_a^-1 x)."""

    name = "linucb"
    contextual = True

    def __init__(self, dim: int, alpha: float = 0.3, ridge: float = 1.0):
        self.dim, self.alpha, self.ridge = dim, alpha, ridge
        self.model = _PerActionRidge(dim, ridge)

    def config(self):
        return {"dim": self.dim, "alpha": self.alpha, "ridge": self.ridge}

    def select(self, x, mask, rng):
        allowed = self._allowed(mask)
        mu = {a: self.model.predict(a, x) for a in allowed}
        ucb = {a: mu[a] + self.alpha * self.model.uncertainty(a, x) for a in allowed}
        choice = _argmax_random_tie(ucb, rng)
        greedy = _argmax_random_tie(mu, rng) if len(allowed) > 1 else choice
        # exploration = a non-greedy choice OR the first time this action is ever tried
        explored = bool(mu[choice] + 1e-12 < mu[greedy] or self.model.n[choice] == 0)
        return Selection(choice, _scores_list(ucb), explored)

    def update(self, x, action, reward):
        self.model.update(action, x, reward)

    def state_dict(self):
        return self.model.state_dict()

    def load_state_dict(self, d):
        self.model.load_state_dict(d)


class LinearEpsilonGreedy(Learner):
    """Contextual epsilon-greedy: per-action ridge regression on the state features."""

    name = "linear-eps-greedy"
    contextual = True

    def __init__(self, dim: int, epsilon: float = 0.1, ridge: float = 1.0, min_pulls: int = 1):
        self.dim, self.epsilon, self.ridge, self.min_pulls = dim, epsilon, ridge, min_pulls
        self.model = _PerActionRidge(dim, ridge)

    def config(self):
        return {"dim": self.dim, "epsilon": self.epsilon, "ridge": self.ridge, "min_pulls": self.min_pulls}

    def select(self, x, mask, rng):
        allowed = self._allowed(mask)
        mu = {a: self.model.predict(a, x) for a in allowed}
        untried = [a for a in allowed if self.model.n[a] < self.min_pulls]
        if untried:
            return Selection(int(untried[int(rng.integers(len(untried)))]), _scores_list(mu), True)
        if rng.random() < self.epsilon:
            return Selection(int(allowed[int(rng.integers(len(allowed)))]), _scores_list(mu), True)
        return Selection(_argmax_random_tie(mu, rng), _scores_list(mu), False)

    def update(self, x, action, reward):
        self.model.update(action, x, reward)

    def state_dict(self):
        return self.model.state_dict()

    def load_state_dict(self, d):
        self.model.load_state_dict(d)


def make_learner(kind: str, dim: int, **kw) -> Learner:
    if kind == "linucb":
        return LinUCB(dim, **kw)
    if kind == "linear-eps-greedy":
        return LinearEpsilonGreedy(dim, **kw)
    if kind == "mean-eps-greedy":
        return MeanEpsilonGreedy(**kw)
    if kind == "random":
        return RandomPolicy()
    if kind.startswith("always-"):
        return FixedPolicy(int(kind.split("-")[1]))
    raise ValueError(f"unknown learner {kind!r}")
