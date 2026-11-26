import copy

import numpy as np
import pytest

from ai.actions import (CH, NUM_ACTIONS, RR, STATIC_SWRR, allowed_actions, normalize_mask, safe_fallback)
from ai.schemas import (SchemaError, parse_metrics, parse_policy, parse_state, parse_timestamp)
from ai.simulator import SCENARIOS, SimEnvironment
from ai.state import build_context, context_dim, expected_dim, validate_state


def sim_state(**faults):
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    env.advance(30)
    for k, v in faults.items():
        setattr(env, k, v)
    return env, env.get_state()


def test_go_timestamp_parsing():
    assert parse_timestamp("2026-11-26T10:47:46.6076048+05:30") == pytest.approx(
        parse_timestamp("2026-11-26T05:17:46.607604Z"), abs=1e-3)
    with pytest.raises(SchemaError):
        parse_timestamp("yesterday")
    with pytest.raises(SchemaError):
        parse_timestamp(5)


def test_valid_state_passes_and_has_expected_shape():
    _, d = sim_state()
    s = parse_state(d)
    r = validate_state(s, n_backends=3)
    assert r.ok, r.reason
    assert len(s.vector) == expected_dim(3) == 28
    assert s.backend_ids == tuple(sorted(s.backend_ids))


@pytest.mark.parametrize("mutate,needle", [
    (lambda d: d.update(state_version="v2"), "state_version"),
    (lambda d: d["flattened_vector"].pop(), "vector length"),
    (lambda d: d["flattened_vector"].__setitem__(3, float("nan")), "non-finite"),
    (lambda d: d["flattened_vector"].__setitem__(3, 1.7), "outside [0,1]"),
    (lambda d: d.update(is_stale=True, snapshot_age_ms=9000.0), "stale"),
    (lambda d: d.update(safe_policy_retain_recommended=True), "retaining"),
    (lambda d: d.update(has_missing_critical_features=True), "missing"),
    (lambda d: d.update(ordered_backend_ids=["Backend-2", "Backend-1", "Backend-3"]), "sorted"),
])
def test_invalid_states_are_rejected(mutate, needle):
    _, d = sim_state()
    d = copy.deepcopy(d)
    mutate(d)
    r = validate_state(parse_state(d), n_backends=3)
    assert not r.ok and needle in r.reason


def test_backend_set_change_is_rejected():
    _, d = sim_state()
    s = parse_state(d)
    assert not validate_state(s, backend_ids=("a", "b", "c")).ok
    assert not validate_state(s, n_backends=4).ok


def test_malformed_payloads_raise_schema_error():
    with pytest.raises(SchemaError):
        parse_state({"oops": 1})
    _, d = sim_state()
    d["flattened_vector"][0] = "x"
    with pytest.raises(SchemaError):
        parse_state(d)
    with pytest.raises(SchemaError):
        parse_metrics({"timestamp": "2026-01-01T00:00:00Z"})
    with pytest.raises(SchemaError):
        parse_policy({"policy": {"strategy": "x", "version": 1}, "action_mask": [1, 0]})


def test_metrics_parse_reads_go_fields():
    env, _ = sim_state()
    m = parse_metrics(env.get_metrics())
    assert m.requests > 0 and set(m.backends) == {"Backend-1", "Backend-2", "Backend-3"}
    assert m.backends["Backend-1"].weight == 3 and m.strategy == "Round-Robin"


def test_context_modes():
    _, d = sim_state()
    s = parse_state(d)
    full, compact = build_context(s, "full"), build_context(s, "compact")
    assert full.shape == (context_dim("full", 3),) == (29,) and full[-1] == 1.0
    assert compact.shape == (context_dim("compact", 3),) == (15,) and compact[-1] == 1.0
    assert np.isfinite(compact).all()
    with pytest.raises(ValueError):
        build_context(s, "nope")


def test_action_mask_helpers():
    assert normalize_mask([True]) == (False,) * NUM_ACTIONS  # malformed => nothing allowed
    assert allowed_actions((True, False, True, False, False)) == [0, 2]
    assert safe_fallback((True,) * 5, current=RR) == RR
    assert safe_fallback((True,) * 5) == STATIC_SWRR
    assert safe_fallback((True, False, False, True, False), current=1) == RR
    assert safe_fallback((False,) * 5) is None
    assert CH == 3
