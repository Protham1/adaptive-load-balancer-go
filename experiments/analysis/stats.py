"""Statistics for repeated-run comparisons (Phase 17). numpy only.

Principles
  * report mean AND spread (sample SD, bootstrap CI), never a single run;
  * policies are compared PAIRED by repetition (same seed => identical arrival schedule);
  * the paired test is an exact sign-flip (randomisation) test on the differences, so it needs no
    normality assumption; with n pairs the smallest attainable two-sided p is 2/2**n, hence
    p < 0.05 is IMPOSSIBLE for n < 6. Small-n results are therefore labelled `insufficient_n`;
  * multiple comparisons (several policies against a reference) use Holm's correction;
  * a claim ("better"/"worse") requires: n >= MIN_N, the 95 % paired-bootstrap CI excludes 0, AND
    the Holm-adjusted p < 0.05. Anything else is `inconclusive`.
"""
from __future__ import annotations

import itertools
import math
from typing import Dict, List, Sequence, Tuple

import numpy as np

MIN_N = 5
N_BOOT = 10000


def _clean(x: Sequence[float]) -> np.ndarray:
    a = np.asarray(list(x), dtype=float)
    return a[~np.isnan(a)]


def summarize(x: Sequence[float], seed: int = 0) -> Dict[str, float]:
    a = _clean(x)
    n = len(a)
    if n == 0:
        return {"n": 0, "mean": float("nan"), "sd": float("nan"), "median": float("nan"),
                "ci_lo": float("nan"), "ci_hi": float("nan")}
    lo, hi = bootstrap_ci(a, seed=seed) if n >= 2 else (float(a[0]), float(a[0]))
    return {"n": n, "mean": float(a.mean()), "sd": float(a.std(ddof=1)) if n > 1 else 0.0,
            "median": float(np.median(a)), "ci_lo": lo, "ci_hi": hi}


def bootstrap_ci(x: Sequence[float], n_boot: int = N_BOOT, alpha: float = 0.05, seed: int = 0) -> Tuple[float, float]:
    """Percentile bootstrap CI of the mean."""
    a = _clean(x)
    if len(a) == 0:
        return float("nan"), float("nan")
    rng = np.random.default_rng(seed)
    means = rng.choice(a, size=(n_boot, len(a)), replace=True).mean(axis=1)
    return float(np.quantile(means, alpha / 2)), float(np.quantile(means, 1 - alpha / 2))


def paired_diffs(a: Dict[int, float], b: Dict[int, float]) -> np.ndarray:
    """Differences a-b over repetitions present (and finite) in both."""
    return np.array([a[r] - b[r] for r in sorted(set(a) & set(b))
                     if not (math.isnan(a[r]) or math.isnan(b[r]))], dtype=float)


def sign_flip_pvalue(d: Sequence[float], max_exact: int = 20, n_mc: int = 200000, seed: int = 0) -> float:
    """Two-sided exact randomisation test of H0: the differences are symmetric about 0."""
    d = _clean(d)
    n = len(d)
    if n == 0:
        return float("nan")
    obs = abs(d.mean())
    if obs == 0.0:
        return 1.0
    if n <= max_exact:
        signs = np.array(list(itertools.product((-1.0, 1.0), repeat=n)))
        stat = np.abs((signs * d).mean(axis=1))
        return float((stat >= obs - 1e-12).mean())
    rng = np.random.default_rng(seed)
    signs = rng.choice((-1.0, 1.0), size=(n_mc, n))
    stat = np.abs((signs * d).mean(axis=1))
    return float(((stat >= obs - 1e-12).sum() + 1) / (n_mc + 1))


def holm(pvals: Sequence[float]) -> List[float]:
    """Holm-Bonferroni adjusted p-values (same order as input)."""
    valid = [i for i in range(len(pvals)) if not math.isnan(pvals[i])]
    m = len(valid)  # untestable (NaN) comparisons do not count towards the correction
    adj = [float("nan")] * len(pvals)
    running = 0.0
    for rank, i in enumerate(sorted(valid, key=lambda i: pvals[i])):
        running = max(running, min(1.0, (m - rank) * pvals[i]))
        adj[i] = running
    return adj


def cohens_dz(d: Sequence[float]) -> float:
    d = _clean(d)
    if len(d) < 2 or d.std(ddof=1) == 0:
        return float("nan")
    return float(d.mean() / d.std(ddof=1))


def compare(a: Dict[int, float], b: Dict[int, float], seed: int = 0) -> Dict[str, float]:
    """Paired comparison of a against reference b (by repetition)."""
    d = paired_diffs(a, b)
    n = len(d)
    if n == 0:
        return {"n_pairs": 0, "mean_diff": float("nan"), "ci_lo": float("nan"), "ci_hi": float("nan"),
                "p": float("nan"), "dz": float("nan")}
    lo, hi = bootstrap_ci(d, seed=seed) if n >= 2 else (float(d[0]), float(d[0]))
    return {"n_pairs": n, "mean_diff": float(d.mean()), "ci_lo": lo, "ci_hi": hi,
            "p": sign_flip_pvalue(d, seed=seed), "dz": cohens_dz(d)}


def verdict(n_pairs: int, ci_lo: float, ci_hi: float, p_adj: float, lower_is_better: bool, min_n: int = MIN_N) -> str:
    """The claim rule. `better`/`worse` refer to the metric direction."""
    if n_pairs < min_n:
        return "insufficient_n"
    if any(math.isnan(v) for v in (ci_lo, ci_hi, p_adj)):
        return "inconclusive"
    if p_adj < 0.05 and (ci_lo > 0 or ci_hi < 0):
        improved = (ci_hi < 0) if lower_is_better else (ci_lo > 0)
        return "better" if improved else "worse"
    return "inconclusive"


def min_attainable_p(n_pairs: int) -> float:
    return 2.0 / (2 ** n_pairs) if n_pairs > 0 else float("nan")
