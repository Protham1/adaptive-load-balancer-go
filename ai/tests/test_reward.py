import math

import pytest

from ai.reward import (RewardConfig, WindowOutcome, capacity_adjusted_rates, compute_reward,
                       jain_index, outcome_from_samples, sensitivity_analysis)
from ai.schemas import BackendCounters, MetricsSample

CAP = {"B1": 3.0, "B2": 2.0, "B3": 1.0}
UP = {"B1": True, "B2": True, "B3": True}


def outcome(reqs, errors=0, p95=0.0, dur=20.0, up_end=UP, up_start=UP):
    total = sum(reqs.values())
    return WindowOutcome(dur, total, errors, p95, reqs, CAP, up_start, up_end)


def test_jain_examples():
    assert jain_index([1, 1, 1]) == pytest.approx(1.0)
    assert jain_index([1, 0, 0, 0]) == pytest.approx(0.25)
    assert jain_index([0, 0]) == 1.0 and jain_index([]) == 1.0
    # hand computed: x = (2, 1) -> 9 / (2*5) = 0.9
    assert jain_index([2, 1]) == pytest.approx(0.9)


def test_config_weights_must_sum_to_one():
    RewardConfig().validate()
    with pytest.raises(ValueError):
        RewardConfig(w_throughput=0.5).validate()
    with pytest.raises(ValueError):
        RewardConfig(w_latency=-0.1, w_throughput=0.7).validate()


def test_hand_computed_balanced_reward():
    # 2400 req / 24 s = 100 rps -> T=1.0 ; capacity-adjusted rates (50/3, 33.3/2, 16.7/1) all 16.67 -> J=1, V=0
    o = outcome({"B1": 1200, "B2": 800, "B3": 400}, errors=24, p95=250.0, dur=24.0)
    b = compute_reward(o)
    assert b.valid and b.override is None
    assert b.normalized["throughput"] == pytest.approx(1.0)
    assert b.normalized["latency"] == pytest.approx(0.5)
    assert b.normalized["error"] == pytest.approx(0.01)
    assert b.normalized["imbalance"] == pytest.approx(0.0)
    # 0.25*1 - 0.35*0.5 - 0.25*0.01 - 0.15*0 = 0.0725
    assert b.reward == pytest.approx(0.0725)
    assert sum(b.contributions.values()) == pytest.approx(b.reward)


def test_hand_computed_imbalance():
    # equal counts although capacities differ: x = (13.33, 20, 40) -> J = 0.82312, V = 0.26531
    o = outcome({"B1": 800, "B2": 800, "B3": 800})
    rates = capacity_adjusted_rates(o)
    assert jain_index(list(rates.values())) == pytest.approx(0.823124, abs=1e-5)
    b = compute_reward(o)
    assert b.normalized["imbalance"] == pytest.approx(0.265314, abs=1e-5)
    # T = 120/100 -> clipped 1 ; reward = 0.25 - 0 - 0 - 0.15*0.265314
    assert b.reward == pytest.approx(0.25 - 0.15 * 0.265314, abs=1e-5)


def test_normalized_terms_are_bounded():
    b = compute_reward(outcome({"B1": 9000, "B2": 10, "B3": 10}, errors=0, p95=99999.0))
    assert all(0.0 <= v <= 1.0 for v in b.normalized.values())
    assert -0.75 <= b.reward <= 0.25


def test_severe_error_override_dominates_throughput():
    o = outcome({"B1": 3000, "B2": 2000, "B3": 1000}, errors=1500, p95=10.0)  # 25% errors, huge throughput
    b = compute_reward(o)
    assert b.override == "severe_error_rate" and b.reward == -1.0
    # and it is strictly below the worst non-override reward
    worst = compute_reward(outcome({"B1": 30, "B2": 0, "B3": 0}, errors=1, p95=1e9))
    assert worst.override is None and worst.reward > b.reward


def test_all_backends_down_override():
    down = {k: False for k in UP}
    b = compute_reward(outcome({"B1": 0, "B2": 0, "B3": 0}, up_end=down))
    assert b.override == "no_healthy_backend" and b.reward == -1.0


def test_small_window_is_invalid_not_zero():
    b = compute_reward(outcome({"B1": 5, "B2": 3, "B3": 2}))
    assert not b.valid and b.reward is None


def test_unhealthy_backend_excluded_from_fairness_set():
    end = {"B1": True, "B2": False, "B3": True}
    o = outcome({"B1": 1200, "B2": 0, "B3": 400}, up_end=end)
    b = compute_reward(o)
    assert b.raw["backends_in_fairness_set"] == 2.0
    assert b.normalized["imbalance"] == pytest.approx(0.0)  # rates 20 and 20


def test_single_backend_has_no_imbalance():
    o = WindowOutcome(20, 2000, 0, 100, {"B1": 2000}, {"B1": 1.0}, {"B1": True}, {"B1": True})
    assert compute_reward(o).normalized["imbalance"] == 0.0


def test_every_component_is_logged():
    d = compute_reward(outcome({"B1": 1200, "B2": 800, "B3": 400}, errors=24, p95=250, dur=24.0)).to_dict()
    for key in ("raw", "normalized", "weights", "contributions", "reward", "override"):
        assert key in d
    assert set(d["contributions"]) == {"throughput", "latency", "error", "imbalance"}


def _sample(t, req, err, p95, per, healthy=True):
    return MetricsSample(t, req, err, p95, True, {
        b: BackendCounters(b, healthy, r, 0, int(CAP[b]), p95) for b, r in per.items()})


def test_outcome_from_samples_uses_counter_deltas():
    s0 = _sample(100.0, 1000, 10, 50, {"B1": 500, "B2": 300, "B3": 200})
    s1 = _sample(120.0, 3000, 30, 80, {"B1": 1700, "B2": 1100, "B3": 600})
    o = outcome_from_samples(s0, s1)
    assert (o.duration_s, o.requests, o.errors, o.p95_ms) == (20.0, 2000, 20, 80)
    assert o.backend_requests == {"B1": 1200, "B2": 800, "B3": 400}


def test_counter_reset_or_time_travel_is_rejected():
    s0 = _sample(100.0, 1000, 10, 50, {"B1": 500})
    with pytest.raises(ValueError):
        outcome_from_samples(s0, _sample(120.0, 10, 0, 50, {"B1": 5}))
    with pytest.raises(ValueError):
        outcome_from_samples(s0, _sample(100.0, 2000, 10, 50, {"B1": 900}))


def test_sensitivity_reports_stability_and_limitation():
    good = [outcome({"B1": 1200, "B2": 800, "B3": 400}, p95=50)] * 5
    bad = [outcome({"B1": 1200, "B2": 800, "B3": 400}, errors=100, p95=400)] * 5
    res = sensitivity_analysis({0: bad, 1: good}, rel=0.25)
    assert res["base_best_action"] == 1 and res["stable"] and res["limitation"] is None
    # two nearly identical actions: ranking is a coin flip under perturbation -> flagged
    a = [outcome({"B1": 1200, "B2": 800, "B3": 400}, p95=100)] * 5
    b = [outcome({"B1": 1000, "B2": 1000, "B3": 400}, p95=60)] * 5  # lower latency, more imbalance
    res2 = sensitivity_analysis({0: a, 1: b}, rel=0.6, n_random=100)
    assert 0.0 <= res2["best_action_unchanged_fraction"] <= 1.0
    assert (res2["limitation"] is None) == res2["stable"]
