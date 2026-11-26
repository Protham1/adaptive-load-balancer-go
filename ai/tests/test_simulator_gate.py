import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from ai.actions import LC, RR, STATIC_SWRR
from ai.bandit import FixedPolicy, LinUCB
from ai.client import HTTPEnvironment
from ai.controller import ActionRejected, BanditController, ControllerConfig
from ai.persistence import DecisionLog
from ai.promotion_gate import (check_deterministic_replay, check_invalid_stale, check_persistence_restart,
                               check_simulated_failure, run_gate)
from ai.reward import RewardConfig, compute_reward, outcome_from_samples
from ai.schemas import parse_metrics
from ai.simulator import SCENARIOS, SimEnvironment, run_controller
from ai.state import context_dim, validate_state
from ai.schemas import parse_state


def window_reward(env, action, seconds=30):
    env.apply_action(action, "x")
    env.advance(10)
    s0 = parse_metrics(env.get_metrics())
    env.advance(seconds)
    s1 = parse_metrics(env.get_metrics())
    return compute_reward(outcome_from_samples(s0, s1))


@pytest.mark.parametrize("name", list(SCENARIOS))
def test_every_scenario_emits_valid_states(name):
    env = SimEnvironment([SCENARIOS[name]], seed=1)
    for _ in range(40):
        env.advance(10)
        r = validate_state(parse_state(env.get_state()), n_backends=3)
        assert r.ok, (name, env.t, r.reason)


def test_simulator_is_deterministic_per_seed():
    a, b = SimEnvironment([SCENARIOS["traffic_spike"]], seed=5), SimEnvironment([SCENARIOS["traffic_spike"]], seed=5)
    a.advance(100)
    b.advance(100)
    assert a.get_metrics() == b.get_metrics() and a.get_state() == b.get_state()
    c = SimEnvironment([SCENARIOS["traffic_spike"]], seed=6)
    c.advance(100)
    assert c.get_metrics() != a.get_metrics()


def test_masked_action_is_rejected_by_simulator():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    with pytest.raises(ActionRejected):
        env.apply_action(3, "x")  # Consistent Hash masked by default
    with pytest.raises(ActionRejected):
        env.apply_action(9, "x")


def test_scenario_expectations_hold_in_the_model():
    # saturation under RR at high load: severe error override; LC/SWRR survive
    env = SimEnvironment([SCENARIOS["high_concurrency"]], seed=1)
    env.advance(30)
    assert window_reward(env, RR).override == "severe_error_rate"
    env = SimEnvironment([SCENARIOS["high_concurrency"]], seed=1)
    env.advance(30)
    assert window_reward(env, LC).override is None
    # failure scenario: errors appear while the dead backend is undetected
    env = SimEnvironment([SCENARIOS["backend_failure"]], seed=1)
    env.advance(55)
    b = window_reward(env, STATIC_SWRR, seconds=20)
    assert b.raw["error_rate"] > 0.05


def test_hand_checked_simulator_reward():
    # Balanced, static SWRR: shares 3:2:1 -> rates exactly proportional to capacity -> V=0.
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    env.advance(30)
    b = window_reward(env, STATIC_SWRR)
    assert b.normalized["imbalance"] < 1e-3 and b.raw["error_rate"] < 0.01
    # ~60 rps / 100 ref = 0.6 throughput; reward ~ 0.25*0.6 - 0.35*p95/500 ... within bounds
    assert b.normalized["throughput"] == pytest.approx(0.6, abs=0.05)
    assert -0.75 <= b.reward <= 0.25


def test_promotion_gate_checks_individually():
    for fn in (check_deterministic_replay, check_invalid_stale, check_persistence_restart):
        ok, detail = fn()
        assert ok, (fn.__name__, detail)


def test_promotion_gate_failure_check_runs():
    ok, detail = check_simulated_failure(seeds=(0, 1))
    assert ok, detail


def test_gate_report_rendering():
    rep = run_gate(run_unit_tests=False)
    text = rep.render()
    assert "PROMOTION GATE" in text and rep.passed, text


# ------------------------------------------------------- HTTP client against a stub service
class _Stub(BaseHTTPRequestHandler):
    env = None
    posted = []

    def log_message(self, *a):
        pass

    def _send(self, code, body):
        data = json.dumps(body).encode() if not isinstance(body, bytes) else body
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        e = type(self).env
        routes = {"/api/state": e.get_state, "/api/metrics": e.get_metrics, "/api/policy": e.get_policy}
        if self.path in routes:
            self._send(200, routes[self.path]())
        else:
            self._send(404, {})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        type(self).posted.append(body)
        try:
            type(self).env.apply_action(body["action"], body.get("decision_id", ""))
            self._send(200, {"status": "ok"})
        except ActionRejected as e:
            self._send(400, str(e).encode())


def test_http_client_end_to_end_against_stub():
    env = SimEnvironment([SCENARIOS["balanced"]], seed=1)
    env.advance(30)
    _Stub.env, _Stub.posted = env, []
    srv = HTTPServer(("127.0.0.1", 0), _Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        client = HTTPEnvironment(f"http://127.0.0.1:{srv.server_port}")
        assert parse_state(client.get_state()).state_version == "v1"
        client.apply_action(LC, "d-1")
        assert _Stub.posted[-1] == {"action": LC, "decision_id": "d-1"}
        with pytest.raises(ActionRejected):
            client.apply_action(3, "d-2")
        ctl = BanditController(FixedPolicy(STATIC_SWRR), ControllerConfig(mode="live"), DecisionLog())
        for _ in range(60):
            env.advance(2)
            ctl.step(client)
        assert any(r["event"] == "outcome" for r in ctl.log.records)
    finally:
        srv.shutdown()
