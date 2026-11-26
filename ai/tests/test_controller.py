import os

import numpy as np
import pytest

from ai.actions import ACTION_STRATEGIES, RR, STATIC_SWRR
from ai.bandit import FixedPolicy, LinUCB, RandomPolicy
from ai.controller import BanditController, ControllerConfig
from ai.persistence import DecisionLog, PersistenceError
from ai.simulator import SCENARIOS, SimEnvironment, run_controller
from ai.state import context_dim

CD = context_dim("compact", 3)


def make(env, learner=None, mode="live", **kw):
    cfg = ControllerConfig(mode=mode, run_id="t", seed=1, **kw)
    return BanditController(learner or LinUCB(CD), cfg, DecisionLog())


def events(ctl, name):
    return [r for r in ctl.log.records if r["event"] == name]


def test_decision_ids_pair_decision_and_outcome():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env)
    run_controller(env, ctl, 400)
    decisions = {r["decision_id"]: r for r in events(ctl, "decision")}
    outcomes = events(ctl, "outcome")
    assert len(outcomes) >= 5
    ids = [r["decision_id"] for r in events(ctl, "decision")]
    assert len(ids) == len(set(ids))  # unique
    for o in outcomes:
        assert o["decision_id"] in decisions and o["action"] == decisions[o["decision_id"]]["action"]
    # every applied switch carried the decision id to the service
    applied_ids = {did for _, _, did in env.applied}
    assert applied_ids <= set(decisions)


def test_outcome_logs_all_reward_components():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env)
    run_controller(env, ctl, 200)
    o = events(ctl, "outcome")[0]
    for k in ("raw", "normalized", "weights", "contributions"):
        assert o["breakdown"][k]
    assert o["reward"] == pytest.approx(sum(o["breakdown"]["contributions"].values()))


def test_shadow_mode_recommends_but_never_applies_or_learns():
    env = SimEnvironment([SCENARIOS["traffic_spike"]], seed=2)
    ctl = make(env, mode="shadow")
    run_controller(env, ctl, 600)
    assert events(ctl, "recommend") and not events(ctl, "decision")
    assert env.applied == [] and env.switch_count == 0 and ctl.updates == 0
    assert all(r["applied"] is False for r in events(ctl, "recommend"))


def test_policy_changed_by_someone_else_abandons_window_without_learning():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env, FixedPolicy(STATIC_SWRR))
    env.advance(30)
    ctl.step(env)  # decision applied
    assert ctl.pending is not None
    env.advance(4)
    env.apply_action(RR, "intruder")  # operator changes policy mid-window
    env.advance(2)
    ctl.step(env)
    ab = events(ctl, "abandon")
    assert ab and "policy changed" in ab[0]["reason"] or "unexpected strategy switch" in ab[0]["reason"]
    assert ctl.updates == 0


def test_outcome_never_attributed_when_exposure_unclean():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env, FixedPolicy(1))
    env.advance(30)
    ctl.step(env)
    env.advance(4)
    env.apply_action(1, "same-strategy-resent")  # same strategy, but a switch the controller did not make
    env.advance(40)
    ctl.step(env)
    assert events(ctl, "abandon") and ctl.updates == 0


@pytest.mark.parametrize("fault", ["inject_stale", "inject_nan", "inject_bad_schema"])
def test_bad_telemetry_holds_policy(fault):
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    env.advance(30)
    setattr(env, fault, True)
    ctl = make(env)
    run_controller(env, ctl, 100)
    assert events(ctl, "hold") and env.applied == [] and ctl.updates == 0


def test_telemetry_going_bad_mid_window_abandons():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env, FixedPolicy(STATIC_SWRR))
    env.advance(30)
    ctl.step(env)
    env.advance(14)
    env.inject_stale = True
    ctl.step(env)
    assert events(ctl, "abandon") and ctl.updates == 0


def test_unreachable_service_is_a_hold_not_a_crash():
    class Down:
        def get_state(self): raise ConnectionError("refused")
        get_metrics = get_policy = get_state
        def apply_action(self, a, d): raise AssertionError("must not be called")
    ctl = BanditController(LinUCB(CD), ControllerConfig(mode="live"), DecisionLog())
    assert ctl.step(Down())[0]["event"] == "hold"


def test_only_allowed_actions_are_applied():
    mask = [False, True, False, False, True]
    env = SimEnvironment([SCENARIOS["traffic_spike"]], seed=1)
    env.mask_override = mask
    env.advance(30)
    ctl = make(env, LinUCB(CD, alpha=1.0))
    run_controller(env, ctl, 800)
    assert env.applied and all(mask[a] for _, a, _ in env.applied)


def test_no_allowed_action_holds():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    env.mask_override = [False] * 5
    env.advance(30)
    ctl = make(env)
    run_controller(env, ctl, 60)
    assert env.applied == [] and any("no allowed action" in r["reason"] for r in events(ctl, "hold"))


def test_severe_window_forces_safe_fallback_next():
    env = SimEnvironment([SCENARIOS["high_concurrency"]], seed=1)
    env.advance(30)
    ctl = make(env, FixedPolicy(RR))  # RR overloads Backend-3 at 110 rps -> severe errors
    ctl.cfg.reward = ctl.cfg.reward  # default config
    run_controller(env, ctl, 400)
    outs = events(ctl, "outcome")
    assert any(o["breakdown"]["override"] for o in outs)
    forced = [r for r in events(ctl, "decision") if r["forced"]]
    assert forced and forced[0]["action"] == STATIC_SWRR


def test_same_policy_is_not_reswitched():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = make(env, FixedPolicy(RR))  # already Round-Robin
    env.advance(30)
    run_controller(env, ctl, 200)
    assert env.switch_count == 0 and events(ctl, "outcome")


def test_persistence_across_restart(tmp_path):
    path = str(tmp_path / "m.json")
    env = SimEnvironment([SCENARIOS["cpu_imbalance"]], seed=3)
    a = LinUCB(CD)
    ctl = BanditController(a, ControllerConfig(mode="live", run_id="a", model_path=path), DecisionLog())
    run_controller(env, ctl, 500)
    assert os.path.exists(path) and ctl.updates > 0

    b = LinUCB(CD)
    env2 = SimEnvironment([SCENARIOS["cpu_imbalance"]], seed=4)
    ctl2 = BanditController(b, ControllerConfig(mode="live", run_id="b", model_path=path), DecisionLog())
    run_controller(env2, ctl2, 6)
    assert np.allclose(a.model.A_inv, b.model.A_inv) and np.allclose(a.model.b, b.model.b)
    assert ctl2.counter >= ctl.counter and ctl2.updates == ctl.updates


def test_incompatible_model_is_refused(tmp_path):
    path = str(tmp_path / "m.json")
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = BanditController(LinUCB(CD), ControllerConfig(mode="live", model_path=path), DecisionLog())
    run_controller(env, ctl, 300)
    # different context mode => different dimension => refuse, never silently mix
    wrong = BanditController(LinUCB(context_dim("full", 3)),
                             ControllerConfig(mode="live", context_mode="full", model_path=path), DecisionLog())
    env2 = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    with pytest.raises(PersistenceError):
        run_controller(env2, wrong, 10)
    # different algorithm
    other = BanditController(RandomPolicy(), ControllerConfig(mode="live", model_path=path), DecisionLog())
    with pytest.raises(PersistenceError):
        run_controller(SimEnvironment([SCENARIOS["balanced"]], seed=1), other, 10)


def test_decision_log_jsonl(tmp_path):
    import json
    p = str(tmp_path / "d.jsonl")
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    ctl = BanditController(LinUCB(CD), ControllerConfig(mode="live"), DecisionLog(p))
    run_controller(env, ctl, 200)
    rows = [json.loads(l) for l in open(p, encoding="utf-8")]
    assert rows and {"event", "t", "run_id"} <= set(rows[0])
    assert ACTION_STRATEGIES[rows[0]["action"]] if "action" in rows[0] else True
