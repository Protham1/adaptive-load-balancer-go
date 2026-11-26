import numpy as np
import pytest

from ai.actions import NUM_ACTIONS
from ai.bandit import (FixedPolicy, LinearEpsilonGreedy, LinUCB, MeanEpsilonGreedy,
                       NoAllowedAction, RandomPolicy)

ALL = (True,) * NUM_ACTIONS


def rng(seed=0):
    return np.random.default_rng(seed)


def test_never_returns_masked_action():
    mask = (False, True, False, False, True)
    g = rng(1)
    x = np.array([0.2, 0.5, 1.0])
    for L in (RandomPolicy(), FixedPolicy(0), MeanEpsilonGreedy(), LinUCB(3), LinearEpsilonGreedy(3)):
        for _ in range(200):
            a = L.select(x, mask, g).action
            assert mask[a]
            L.update(x, a, float(g.normal()))


def test_no_allowed_action_raises():
    for L in (RandomPolicy(), MeanEpsilonGreedy(), LinUCB(3), LinearEpsilonGreedy(3), FixedPolicy(0)):
        with pytest.raises(NoAllowedAction):
            L.select(np.zeros(3), (False,) * NUM_ACTIONS, rng())


def test_contextual_flags_are_honest():
    assert LinUCB(3).contextual and LinearEpsilonGreedy(3).contextual
    assert not MeanEpsilonGreedy().contextual and not RandomPolicy().contextual


def test_mean_eps_greedy_ignores_context():
    L = MeanEpsilonGreedy(epsilon=0.0)
    for a in range(NUM_ACTIONS):
        L.update(np.zeros(1), a, 0.1 * a)
    # same choice regardless of the context passed in
    picks = {L.select(np.random.default_rng(i).random(4), ALL, rng()).action for i in range(20)}
    assert picks == {NUM_ACTIONS - 1}


def _train(L, steps=400, seed=0):
    """Context-dependent reward: action 1 is best when x[0] > .5, action 3 otherwise."""
    g = rng(seed)
    for _ in range(steps):
        x = np.array([g.random(), 1.0])
        a = L.select(x, ALL, g).action
        best = 1 if x[0] > 0.5 else 3
        r = 1.0 if a == best else 0.0
        L.update(x, a, r + 0.05 * g.normal())


@pytest.mark.parametrize("make", [lambda: LinUCB(2, alpha=0.3), lambda: LinearEpsilonGreedy(2, epsilon=0.1)])
def test_contextual_learners_learn_context_dependent_best_action(make):
    L = make()
    _train(L)
    g = rng(99)
    hi = L.select(np.array([0.9, 1.0]), ALL, g)
    lo = L.select(np.array([0.1, 1.0]), ALL, g)
    scores_hi = [s for s in hi.scores]
    # the learned model must rank action 1 above 3 at high x, and 3 above 1 at low x
    mu = lambda a, x: float(L.model.theta(a) @ np.array([x, 1.0]))
    assert mu(1, 0.9) > mu(3, 0.9) and mu(3, 0.1) > mu(1, 0.1)
    assert len(scores_hi) == NUM_ACTIONS


def test_mean_bandit_cannot_solve_context_dependent_task():
    # best action flips with context, so any single fixed action averages ~0.5
    L = MeanEpsilonGreedy(epsilon=0.1)
    _train(L, steps=600)
    assert max(L.mean) < 0.75


def test_sherman_morrison_matches_direct_inverse():
    L = LinUCB(3, ridge=1.0)
    g = rng(5)
    A = np.eye(3)
    for _ in range(30):
        x = g.random(3)
        L.update(x, 2, 0.3)
        A += np.outer(x, x)
    assert np.allclose(L.model.A_inv[2], np.linalg.inv(A))


def test_linucb_explores_untried_actions_first():
    L = LinUCB(2, alpha=1.0)
    g = rng(0)
    x = np.array([0.5, 1.0])
    seen = set()
    for _ in range(NUM_ACTIONS * 3):
        s = L.select(x, ALL, g)
        seen.add(s.action)
        L.update(x, s.action, 0.0)
    assert seen == set(range(NUM_ACTIONS))


def test_state_dict_roundtrip():
    for make in (lambda: LinUCB(2), lambda: LinearEpsilonGreedy(2), lambda: MeanEpsilonGreedy()):
        a, b = make(), make()
        _train(a, steps=50)
        b.load_state_dict(a.state_dict())
        x = np.array([0.3, 1.0])
        assert a.select(x, ALL, rng(4)).scores == b.select(x, ALL, rng(4)).scores


def test_load_rejects_wrong_dimension():
    a, b = LinUCB(2), LinUCB(5)
    with pytest.raises(ValueError):
        b.load_state_dict(a.state_dict())
