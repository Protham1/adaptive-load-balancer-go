import math

import numpy as np
import pytest

from experiments.analysis import hist as H
from experiments.analysis import stats as S


# ------------------------------------------------------------------ histogram
def test_percentile_within_five_percent_of_exact():
    rng = np.random.default_rng(1)
    x = rng.lognormal(mean=3.2, sigma=0.6, size=20000)  # ms
    h = {}
    for v in x:
        i = H.index_of(v)
        h[i] = h.get(i, 0) + 1
    for q in (0.5, 0.95, 0.99):
        exact = float(np.quantile(x, q))
        est = H.percentile(h, q)
        assert exact * 0.97 <= est <= exact * 1.06, (q, exact, est)  # upper bound: >= truth, <= +5% (+sampling)


def test_merge_is_additive_and_accepts_json_string_keys():
    a, b = {"10": 3, "12": 1}, {10: 2, 13: 4}
    m = H.merge([a, b])
    assert m[10] == 5 and m[12] == 1 and m[13] == 4 and H.total(m) == 10
    # percentiles of merged data differ from the average of percentiles (never average percentiles)
    assert H.percentile({"5": 99, "40": 1}, 0.5) == pytest.approx(H.upper_bound_ms(5))
    assert H.percentile({"5": 99, "40": 1}, 1.0) == pytest.approx(H.upper_bound_ms(40))


def test_empty_histogram_is_nan_and_index_floor():
    assert math.isnan(H.percentile({}, 0.95))
    assert H.index_of(0.05) == 0
    assert H.index_of(H.upper_bound_ms(37)) in (37, 38)  # boundary rounding


# ---------------------------------------------------------------------- stats
def test_summarize_ignores_nan_and_reports_spread():
    s = S.summarize([1.0, 2.0, 3.0, float("nan")])
    assert s["n"] == 3 and s["mean"] == pytest.approx(2.0) and s["sd"] == pytest.approx(1.0)
    assert s["ci_lo"] <= 2.0 <= s["ci_hi"]
    assert S.summarize([])["n"] == 0


def test_bootstrap_ci_covers_true_mean_and_narrows_with_n():
    rng = np.random.default_rng(0)
    cover = 0
    for seed in range(100):
        x = rng.normal(10, 2, size=15)
        lo, hi = S.bootstrap_ci(x, n_boot=1000, seed=seed)
        cover += lo <= 10 <= hi
    assert 85 <= cover <= 100  # nominal 95 %
    w = lambda n: np.subtract(*S.bootstrap_ci(rng.normal(0, 1, n), seed=1)[::-1])
    assert w(200) < w(10)


def test_sign_flip_exact_values():
    assert S.sign_flip_pvalue([1, 1, 1]) == pytest.approx(0.25)  # 2 / 2**3
    assert S.sign_flip_pvalue([1] * 5) == pytest.approx(2 / 32)  # 0.0625: p < 0.05 impossible for n = 5
    assert S.sign_flip_pvalue([2.0] * 6) == pytest.approx(2 / 64)
    assert S.sign_flip_pvalue([0, 0, 0]) == 1.0
    assert S.sign_flip_pvalue([1, -1]) == pytest.approx(1.0)
    assert S.min_attainable_p(5) == pytest.approx(0.0625) and S.min_attainable_p(6) < 0.05


def test_sign_flip_matches_scipy_permutation_test():
    scipy_stats = pytest.importorskip("scipy.stats")
    rng = np.random.default_rng(3)
    for n in (6, 8, 10):
        d = rng.normal(0.5, 1.0, size=n)
        ref = scipy_stats.permutation_test((d,), np.mean, permutation_type="samples", alternative="two-sided",
                                           n_resamples=10 ** 6, random_state=0).pvalue
        assert S.sign_flip_pvalue(d) == pytest.approx(ref, abs=1e-9)


def test_holm_known_example():
    assert S.holm([0.01, 0.04, 0.03]) == pytest.approx([0.03, 0.06, 0.06])
    assert S.holm([0.5]) == [0.5]
    adj = S.holm([0.2, float("nan"), 0.01])
    assert math.isnan(adj[1]) and adj[2] == pytest.approx(0.02) and adj[0] == pytest.approx(0.2)


def test_compare_pairs_by_repetition_only():
    a = {0: 10.0, 1: 12.0, 2: 11.0, 5: 99.0}  # rep 5 has no partner: must be ignored
    b = {0: 20.0, 1: 21.0, 2: 22.0, 3: 1.0}
    c = S.compare(a, b)
    assert c["n_pairs"] == 3 and c["mean_diff"] == pytest.approx(-(10 + 9 + 11) / 3)
    assert S.compare({}, {})["n_pairs"] == 0


def test_verdict_claim_rule():
    # clear, consistent improvement (lower is better) with n=8
    d = {r: 100.0 for r in range(8)}
    e = {r: 80.0 + r * 0.5 for r in range(8)}
    c = S.compare(e, d)
    assert S.verdict(c["n_pairs"], c["ci_lo"], c["ci_hi"], S.holm([c["p"]])[0], True) == "better"
    assert S.verdict(c["n_pairs"], c["ci_lo"], c["ci_hi"], S.holm([c["p"]])[0], False) == "worse"  # direction matters
    # same effect but only 3 pairs: never a claim
    c3 = S.compare({r: e[r] for r in range(3)}, {r: d[r] for r in range(3)})
    assert S.verdict(c3["n_pairs"], c3["ci_lo"], c3["ci_hi"], 0.01, True) == "insufficient_n"
    # noisy, mixed-sign differences: inconclusive
    mixed = S.compare({r: 100 + (-1) ** r * 20 for r in range(8)}, {r: 100.0 for r in range(8)})
    assert S.verdict(mixed["n_pairs"], mixed["ci_lo"], mixed["ci_hi"], 1.0, True) == "inconclusive"
    # NaN guards
    assert S.verdict(8, float("nan"), 1.0, 0.01, True) == "inconclusive"
