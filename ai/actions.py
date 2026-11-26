"""Action space shared with the Go policy package (IDs are a stable contract)."""
from __future__ import annotations

from typing import Optional, Sequence

NUM_ACTIONS = 5
ACTION_STRATEGIES = (
    "Round-Robin",  # 0
    "Least-Connections",  # 1
    "Smooth-Weighted-Round-Robin",  # 2 (static SWRR)
    "Consistent-Hash",  # 3 (affinity only)
    "Adaptive-Weighted-Round-Robin",  # 4 (rule-based baseline)
)
STRATEGY_TO_ACTION = {s: i for i, s in enumerate(ACTION_STRATEGIES)}
RR, LC, STATIC_SWRR, CH, RULE_ADAPTIVE = range(5)

# Preferred conservative fallback order when the current policy cannot be retained.
SAFE_FALLBACK_ORDER = (STATIC_SWRR, RR, LC, RULE_ADAPTIVE)


def action_for_strategy(strategy: str) -> Optional[int]:
    return STRATEGY_TO_ACTION.get(strategy)


def normalize_mask(mask: Sequence[bool]) -> tuple:
    """Return a NUM_ACTIONS-long mask; anything malformed means 'nothing allowed'."""
    if len(mask) != NUM_ACTIONS:
        return (False,) * NUM_ACTIONS
    return tuple(bool(m) for m in mask)


def allowed_actions(mask: Sequence[bool]) -> list:
    return [a for a, ok in enumerate(normalize_mask(mask)) if ok]


def safe_fallback(mask: Sequence[bool], current: Optional[int] = None) -> Optional[int]:
    """Retain the current action if allowed, else the first allowed conservative one."""
    m = normalize_mask(mask)
    if current is not None and 0 <= current < NUM_ACTIONS and m[current]:
        return current
    for a in SAFE_FALLBACK_ORDER:
        if m[a]:
            return a
    return None
