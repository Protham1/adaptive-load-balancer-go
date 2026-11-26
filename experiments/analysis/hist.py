"""Log-bucket latency histogram (same scheme as cmd/loadgen).

Bucket i covers (BASE_MS * RATIO**(i-1), BASE_MS * RATIO**i]. A percentile is the
UPPER bound of the bucket containing it (nearest-rank), i.e. a conservative
estimate at most RATIO (+5 %) above the true value. Histograms merge by addition,
so every aggregation (window, run, policy) uses one methodology and percentiles
are never averaged.
"""
from __future__ import annotations

import math
from collections import Counter
from typing import Iterable, Mapping

BASE_MS = 0.1
RATIO = 1.05


def upper_bound_ms(i: int) -> float:
    return BASE_MS * RATIO ** i


def index_of(ms: float) -> int:
    """Bucket index of a latency (mirrors histIndex in Go; used for tests / synthetic data)."""
    if ms <= BASE_MS:
        return 0
    return int(math.ceil(math.log(ms / BASE_MS) / math.log(RATIO)))


def merge(hists: Iterable[Mapping]) -> Counter:
    out: Counter = Counter()
    for h in hists:
        for k, v in h.items():
            out[int(k)] += int(v)
    return out


def total(h: Mapping) -> int:
    return sum(int(v) for v in h.values())


def percentile(h: Mapping, q: float) -> float:
    """Nearest-rank percentile (upper bucket bound, ms). NaN for an empty histogram."""
    d = {int(k): int(v) for k, v in h.items()}  # JSON keys arrive as strings
    n = sum(d.values())
    if n == 0:
        return float("nan")
    rank = max(1, math.ceil(q * n))
    acc = 0
    for i in sorted(d):
        acc += d[i]
        if acc >= rank:
            return upper_bound_ms(i)
    return upper_bound_ms(max(d))
