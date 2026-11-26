"""Bounded exploration (Phase 19).

Exploration is what lets a bandit learn, but in live mode it must be bounded. Two bounds:

1. FIRST-TIME tries are always allowed (the learner has no data about an action it never played), but
   they are still subject to the Go policy manager (minimum policy age, safety lockout, health).
2. REPEATED exploration (a non-greedy choice of an action that has been tried before) is capped to
   `max_fraction` of the last `window` decisions: once the budget is used up the guard replaces the
   learner's choice by the greedy one until the fraction drops again.

The learner's own exploration parameter is capped too (`max_alpha`).
"""
from __future__ import annotations

from collections import deque
from typing import Deque, Tuple

from .bandit import Learner, Selection


class ExplorationGuard:
    def __init__(self, max_fraction: float = 0.5, window: int = 10, max_alpha: float = 1.0):
        if not 0.0 <= max_fraction <= 1.0 or window < 1 or max_alpha <= 0:
            raise ValueError("invalid exploration bounds")
        self.max_fraction, self.window, self.max_alpha = max_fraction, window, max_alpha
        self.recent: Deque[bool] = deque(maxlen=window)
        self.overrides = 0

    def clamp_learner(self, learner: Learner) -> None:
        """Cap the learner's exploration coefficient."""
        if hasattr(learner, "alpha") and learner.alpha > self.max_alpha:
            learner.alpha = self.max_alpha

    def explore_fraction(self) -> float:
        return sum(self.recent) / len(self.recent) if self.recent else 0.0

    def apply(self, learner: Learner, x, mask, rng, sel: Selection) -> Tuple[Selection, bool]:
        """Return (selection actually used, overridden?)."""
        repeated = sel.explored and (learner.pulls(sel.action) or 0) > 0
        overridden = False
        if repeated and len(self.recent) >= 1 and self.explore_fraction() >= self.max_fraction:
            sel = learner.greedy(x, mask, rng)
            overridden = True
            self.overrides += 1
        self.recent.append(bool(sel.explored and (learner.pulls(sel.action) or 0) > 0))
        return sel, overridden

    def stats(self) -> dict:
        return {"max_fraction": self.max_fraction, "window": self.window, "recent_explore_fraction": self.explore_fraction(),
                "overrides": self.overrides, "max_alpha": self.max_alpha}
