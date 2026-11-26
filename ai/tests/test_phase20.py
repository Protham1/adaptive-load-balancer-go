import json
import os

import numpy as np
import pytest

from ai.bandit import LinUCB
from ai.controller import BanditController, ControllerConfig
from ai.persistence import DecisionLog
from ai.predict import EMATrend, LinearRegression, MovingAverage, Persistence, PredictiveContext, make_forecaster
from ai.predict_eval import (CANDIDATES, accuracy_for_series, choose_on_train, control_comparison, walk_forward_errors)
from ai.schemas import parse_timestamp
from ai.server import DecisionService
from ai.simulator import SCENARIOS, TREND_SCENARIOS, SimEnvironment, run_controller
from ai.state import context_dim, feature_schema_hash

CD = context_dim("compact", 3)
FIX = os.path.join(os.path.dirname(__file__), "..", "..", "testdata", "contract", "valid", "request_basic.json")


# ----------------------------------------------------------------- forecasters (hand-computed)
def test_moving_average_flat_forecast_and_window():
    f = MovingAverage(window_s=20)
    for t, y in [(0, 10), (10, 20), (20, 30), (30, 40)]:
        f.update(t, y)
    assert f.forecast(100) == pytest.approx((20 + 30 + 40) / 3)   # t=0 fell out of the 20 s window


def test_linear_regression_is_exact_on_a_line_with_irregular_spacing():
    f = LinearRegression(window_s=1000)
    for t in (0, 3, 4, 10, 11, 17):
        f.update(t, 2.0 + 0.5 * t)
    assert f.forecast(30) == pytest.approx(2.0 + 0.5 * (17 + 30))
    one = LinearRegression()
    assert one.forecast(5) is None
    one.update(0, 7.0)
    one.update(1, 9.0)
    assert one.forecast(5) == 9.0                                  # fewer than min_points: last value
    const = LinearRegression()
    for t in range(6):
        const.update(t, 0.3)
    assert const.forecast(40) == pytest.approx(0.3)


def test_ema_trend_converges_on_a_linear_series_and_beats_the_flat_average():
    f, ma = EMATrend(alpha=0.5, beta=0.3), MovingAverage(60)
    for t in range(0, 200, 5):
        f.update(t, 1.0 + 0.1 * t)
        ma.update(t, 1.0 + 0.1 * t)
    truth = 1.0 + 0.1 * (195 + 30)
    assert abs(f.forecast(30) - truth) < 0.05 and abs(ma.forecast(30) - truth) > 5
    with pytest.raises(ValueError):
        EMATrend(alpha=0, beta=0.3)


def test_persistence_and_factory():
    p = Persistence()
    assert p.forecast(10) is None
    p.update(0, 0.4)
    assert p.forecast(10) == 0.4
    assert isinstance(make_forecaster("ema", alpha=0.3, beta=0.2), EMATrend)
    with pytest.raises(ValueError):
        make_forecaster("lstm")


# ------------------------------------------------------------------------- no leakage
@pytest.mark.parametrize("kind,params", [("ma", {"window_s": 60.0}), ("ema", {"alpha": 0.5, "beta": 0.3}),
                                         ("linreg", {"window_s": 90.0})])
def test_forecasters_are_causal(kind, params):
    ts = list(range(0, 200, 5))
    a = [0.2 + 0.001 * t for t in ts]
    b = list(a)
    for i in range(20, len(b)):
        b[i] = 99.0                                                # a different FUTURE from index 20 on
    fa, fb = make_forecaster(kind, **params), make_forecaster(kind, **params)
    for i in range(20):
        fa.update(ts[i], a[i])
        fb.update(ts[i], b[i])
    assert fa.forecast(30) == fb.forecast(30)


def test_hyperparameters_are_chosen_without_looking_at_the_test_period():
    ts = [float(i * 10) for i in range(40)]
    clean = [0.1 + 0.01 * i for i in range(40)]
    poisoned = clean[:24] + [50.0] * 16                            # destroy everything after the 60% split
    n_train = 24
    for kind in CANDIDATES:
        assert choose_on_train(ts, clean, kind, 3, n_train)[0] == choose_on_train(ts, poisoned, kind, 3, n_train)[0]


def test_walk_forward_only_scores_targets_inside_the_requested_range():
    ts = [float(i) for i in range(30)]
    ys = [float(i) for i in range(30)]
    errs = walk_forward_errors(ts, ys, "linreg", {"window_s": 50.0}, 2, 20, 30)
    assert len(errs) == 10 and max(errs) < 1e-9                    # a ramp is perfectly forecastable by linreg


def test_trend_series_gives_positive_skill_over_persistence_noise_does_not():
    ts = [float(i * 10) for i in range(60)]
    ramp = [0.1 + 0.012 * i for i in range(60)]
    acc = accuracy_for_series(ts, ramp, k=3)
    assert acc["linreg"]["mae"] < 0.2 * acc["persistence"]["mae"] and acc["ema"]["mae"] < acc["persistence"]["mae"]
    rng = np.random.default_rng(0)
    noise = list(0.5 + rng.normal(0, 0.05, 60))
    acc = accuracy_for_series(ts, noise, k=3)
    assert acc["ma"]["mae"] <= acc["persistence"]["mae"]           # averaging beats the noisy last value on a flat signal


# ------------------------------------------------------------- context augmentation + wiring
def test_predictive_context_shape_bias_clipping_and_trend():
    pc = PredictiveContext("linreg", window_s=300.0)
    base = np.zeros(CD)
    base[-1] = 1.0
    out = None
    for i in range(6):
        c = base.copy()
        c[0] = 0.1 + 0.15 * i                                      # rps rising
        out = pc.augment(c, t=30.0 * i, horizon_s=30.0)
    assert out.shape == (CD + 3,) and out[-1] == 1.0
    assert out[CD - 1] > c[0]                                      # forecast of a rising signal exceeds the present value
    assert np.all((out[CD - 1:CD + 2] >= 0) & (out[CD - 1:CD + 2] <= 1))
    assert pc.feature_names()[0] == "forecast.rps_normalized"


def test_feature_schema_hash_covers_the_forecast_features():
    ids = ["a", "b", "c"]
    extra = tuple(PredictiveContext.feature_names())
    assert feature_schema_hash("compact", ids) != feature_schema_hash("compact", ids, extra=extra)


def test_controller_runs_with_a_predictor_and_logs_augmented_context():
    pc = PredictiveContext("ema", alpha=0.5, beta=0.3)
    env = SimEnvironment([TREND_SCENARIOS["ramp_up"]], seed=1)
    ctl = BanditController(LinUCB(CD + 3, alpha=0.05), ControllerConfig(mode="live", predictor=pc, seed=1), DecisionLog())
    run_controller(env, ctl, 240)
    ds = [e for e in ctl.log.records if e["event"] == "decision"]
    assert ds and all(len(e["context"]) == CD + 3 for e in ds) and ctl.updates > 0
    # a learner of the wrong dimension is rejected (hold), never silently truncated
    bad = BanditController(LinUCB(CD, alpha=0.05), ControllerConfig(mode="live", predictor=PredictiveContext("ema")),
                           DecisionLog())
    run_controller(SimEnvironment([SCENARIOS["balanced"]], seed=1), bad, 60)
    assert not [e for e in bad.log.records if e["event"] == "decision"]


def test_server_with_predictor_persists_and_refuses_a_model_trained_without_it(tmp_path):
    base = json.load(open(FIX, encoding="utf-8"))
    T0 = parse_timestamp("2026-10-13T16:00:00Z")

    def req(i, prev=None):
        d = json.loads(json.dumps(base))
        d["decision_id"] = f"w-{i}"
        d["observed_at"] = "2026-10-13T16:00:00Z"
        if prev:
            d["previous_outcome"] = {"decision_id": prev, "window_seconds": 20, "requests": 2400, "errors": 12,
                                     "p95_ms": 80.0, "clean_exposure": True,
                                     "backends": [{"id": f"Backend-{k}", "requests": 800, "healthy": True, "weight": 1}
                                                  for k in (1, 2, 3)]}
        return json.dumps(d).encode()

    path = str(tmp_path / "m.json")

    def mk(**kw):
        return DecisionService(LinUCB(CD + 3, alpha=0.3), model_path=path, clock=lambda: T0 + 1,
                               predictor=PredictiveContext("ema"), **kw)

    a = mk()
    for i in range(4):
        assert a.handle(req(i, f"w-{i-1}" if i else None))[0] == 200
    assert a.updates == 3 and a.learner_view()["learner"]["predictor"]["kind"] == "ema"
    ctx = [e for e in a.log.records if e["event"] == "decision"][0]["context"]
    assert len(ctx) == CD + 3
    b = mk()
    assert b.handle(req(10))[0] == 200 and b.updates == 3             # same schema -> restored
    c = DecisionService(LinUCB(CD, alpha=0.3), model_path=path, clock=lambda: T0 + 1)   # no predictor: other schema
    assert c.handle(req(20))[0] == 500


def test_control_comparison_smoke_has_paired_statistics_and_a_claim_rule():
    out = control_comparison("trend", dev_seeds=[2000], test_seeds=[0, 1, 2])
    assert set(out["arms"]) == {"reactive", "predictive-ma", "predictive-ema", "predictive-linreg"}
    c = [r for r in out["comparisons"] if r["metric"] == "reward"]
    assert len(c) == 3 and all(r["n_pairs"] == 3 for r in c)
    assert all(r["verdict"] == "insufficient_n" for r in c)          # n=3 can never support a claim
    assert set(out["chosen_on_dev"]) == {"ma", "ema", "linreg"}
