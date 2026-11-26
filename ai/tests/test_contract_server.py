import copy
import http.client
import json
import os
import threading

import pytest

from ai.actions import WIRE_NAMES
from ai.bandit import LinUCB
from ai.contract import (ContractError, dumps_strict, loads_strict, parse_request, validate_response)
from ai.persistence import DecisionLog
from ai.schemas import parse_timestamp
from ai.server import DecisionService, serve
from ai.state import context_dim

FIX = os.path.join(os.path.dirname(__file__), "..", "..", "testdata", "contract")
T0 = parse_timestamp("2026-10-13T16:00:00Z")
CD = context_dim("compact", 3)


def files(sub, prefix):
    d = os.path.join(FIX, sub)
    out = {f: open(os.path.join(d, f), "rb").read() for f in sorted(os.listdir(d)) if f.startswith(prefix)}
    assert out, (sub, prefix)
    return out


BASE = json.loads(files("valid", "request_basic")["request_basic.json"])


def svc(**kw):
    return DecisionService(LinUCB(CD), clock=lambda: T0 + 1, log=DecisionLog(), **kw)


def req(decision_id="window-00042", **over):
    d = copy.deepcopy(BASE)
    d["decision_id"] = decision_id
    d.update(over)
    return d


# ---------------------------------------------------------------- shared fixtures
@pytest.mark.parametrize("name", list(files("valid", "request_")))
def test_valid_request_fixtures(name):
    parse_request(loads_strict(files("valid", "request_")[name]))


@pytest.mark.parametrize("name", list(files("invalid", "request_")))
def test_invalid_request_fixtures(name):
    with pytest.raises(ContractError):
        parse_request(loads_strict(files("invalid", "request_")[name]))


@pytest.mark.parametrize("name", list(files("valid", "response_")))
def test_valid_response_fixtures(name):
    validate_response(loads_strict(files("valid", "response_")[name]), "window-00042")


@pytest.mark.parametrize("name", list(files("invalid", "response_")))
def test_invalid_response_fixtures(name):
    with pytest.raises(ContractError):
        validate_response(loads_strict(files("invalid", "response_")[name]), "window-00042")


def test_wire_names_match_go():
    assert WIRE_NAMES == ("round_robin", "least_connections", "static_swrr", "consistent_hash",
                          "rule_based_adaptive")


def test_dumps_strict_refuses_nan():
    with pytest.raises(ValueError):
        dumps_strict({"x": float("nan")})


# ------------------------------------------------------------------- service logic
def test_response_is_valid_and_only_allowed():
    s = svc()
    for i in range(30):
        status, resp = s.handle(json.dumps(req(f"w-{i}")).encode())
        assert status == 200
        validate_response(resp, f"w-{i}")
        assert resp["strategy"] in BASE["allowed_strategies"]  # consistent_hash never offered
        assert len(resp["scores"]) == 5 and resp["scores"][3] is None


def test_bad_request_is_400_with_reason():
    status, resp = svc().handle(b'{"schema_version": 2}')
    assert status == 400 and resp["code"] == "bad_request"
    assert svc().handle(b"NaN")[0] == 400


def test_stale_snapshot_retains_current_strategy():
    s = DecisionService(LinUCB(CD), clock=lambda: T0 + 120, log=DecisionLog())
    status, resp = s.handle(json.dumps(req()).encode())
    assert status == 200 and resp["strategy"] == "round_robin" and resp["diagnostic"] == "retain_stale_snapshot"


def test_backend_set_change_is_retained():
    s = svc()
    s.handle(json.dumps(req("a")).encode())
    other = req("b")
    for b in other["features"]["backends"]:
        b["id"] = b["id"].replace("Backend", "Node")
    status, resp = s.handle(json.dumps(other).encode())
    assert resp["diagnostic"] == "retain_backend_set_changed"


def outcome_for(decision_id, clean=True, errors=12, requests=2400):
    return {"decision_id": decision_id, "window_seconds": 20, "requests": requests, "errors": errors, "p95_ms": 85.5,
            "clean_exposure": clean,
            "backends": [{"id": "Backend-1", "requests": 1200, "healthy": True, "weight": 3},
                         {"id": "Backend-2", "requests": 800, "healthy": True, "weight": 2},
                         {"id": "Backend-3", "requests": 400, "healthy": True, "weight": 1}]}


def test_learning_uses_previous_outcome_when_clean():
    s = svc()
    s.handle(json.dumps(req("w-1")).encode())
    assert s.updates == 0
    s.handle(json.dumps(req("w-2", previous_outcome=outcome_for("w-1"))).encode())
    assert s.updates == 1
    ev = [r for r in s.log.records if r["event"] == "outcome"][0]
    assert ev["target"] == "w-1" and set(ev["breakdown"]["contributions"]) == {
        "throughput", "latency", "error", "imbalance"}


def test_unclean_unknown_or_tiny_outcomes_never_update():
    s = svc()
    s.handle(json.dumps(req("w-1")).encode())
    s.handle(json.dumps(req("w-2", previous_outcome=outcome_for("w-1", clean=False))).encode())
    s.handle(json.dumps(req("w-3", previous_outcome=outcome_for("never-issued"))).encode())
    s.handle(json.dumps(req("w-4", previous_outcome=outcome_for("w-3", requests=5, errors=0))).encode())
    assert s.updates == 0
    # a decision id can be credited at most once
    s.handle(json.dumps(req("w-5")).encode())
    s.handle(json.dumps(req("w-6", previous_outcome=outcome_for("w-5"))).encode())
    s.handle(json.dumps(req("w-7", previous_outcome=outcome_for("w-5"))).encode())
    assert s.updates == 1


def test_persistence_roundtrip(tmp_path):
    path = str(tmp_path / "m.json")
    a = DecisionService(LinUCB(CD), model_path=path, clock=lambda: T0 + 1, log=DecisionLog())
    a.handle(json.dumps(req("w-1")).encode())
    a.handle(json.dumps(req("w-2", previous_outcome=outcome_for("w-1"))).encode())
    assert os.path.exists(path) and a.updates == 1
    b = DecisionService(LinUCB(CD), model_path=path, clock=lambda: T0 + 1, log=DecisionLog())
    b.handle(json.dumps(req("x-1")).encode())
    assert b.updates == 1


# ------------------------------------------------------------------ real HTTP server
@pytest.fixture()
def server():
    srv = serve(svc(), "127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield srv
    srv.shutdown()


def post(srv, body, ctype="application/json", path="/v1/decision"):
    c = http.client.HTTPConnection("127.0.0.1", srv.server_port, timeout=5)
    c.request("POST", path, body=body, headers={"Content-Type": ctype})
    r = c.getresponse()
    return r.status, json.loads(r.read() or b"{}")


def test_http_decision_and_health(server):
    code, resp = post(server, json.dumps(req()).encode())
    assert code == 200 and resp["decision_id"] == "window-00042"
    c = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=5)
    c.request("GET", "/v1/health")
    h = json.loads(c.getresponse().read())
    assert h["status"] == "ok" and h["schema_version"] == 1 and h["decisions"] == 1


def test_http_rejections(server):
    assert post(server, b"NaN")[0] == 400
    assert post(server, json.dumps(req()).encode(), ctype="text/plain")[0] == 415
    assert post(server, b"x" * (70 * 1024))[0] == 413
    assert post(server, b"{}", path="/v2/other")[0] == 404
    bad = req()
    bad["features"]["global"]["rps_normalized"] = 7
    assert post(server, json.dumps(bad).encode())[0] == 400
