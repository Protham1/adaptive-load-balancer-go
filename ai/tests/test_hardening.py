import copy
import http.client
import json
import os
import socket
import subprocess
import sys
import threading
import time

import pytest

from ai.bandit import LinUCB
from ai.persistence import DecisionLog
from ai.schemas import parse_timestamp
from ai.server import BoundedServer, DecisionService, is_loopback_host, serve
from ai.state import context_dim

FIX = os.path.join(os.path.dirname(__file__), "..", "..", "testdata", "contract", "valid", "request_basic.json")
BASE = json.load(open(FIX, encoding="utf-8"))
T0 = parse_timestamp("2026-10-13T16:00:00Z")
CD = context_dim("compact", 3)


def svc(tmp_path=None, **kw):
    kw.setdefault("log", DecisionLog(max_memory=500))
    if tmp_path is not None:
        kw["model_path"] = str(tmp_path / "m.json")
    return DecisionService(LinUCB(CD, alpha=0.3), clock=lambda: T0 + 1, **kw)


def body(i=1, **over):
    d = copy.deepcopy(BASE)
    d["decision_id"] = f"w-{i}"
    d.update(over)
    return json.dumps(d).encode()


def running(service, **kw):
    srv = serve(service, "127.0.0.1", 0, **kw)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def call(srv, method, path, data=None, headers=None, timeout=5):
    c = http.client.HTTPConnection("127.0.0.1", srv.server_port, timeout=timeout)
    h = {"Content-Type": "application/json", **(headers or {})}
    c.request(method, path, body=data, headers=h)
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r.status, (json.loads(raw) if raw else {})


# ------------------------------------------------------------------ bind policy
@pytest.mark.parametrize("host,expected", [("127.0.0.1", True), ("localhost", True), ("::1", True), ("127.0.5.9", True),
                                           ("0.0.0.0", False), ("192.168.1.5", False), ("example.com", False), ("::", False)])
def test_loopback_detection(host, expected):
    assert is_loopback_host(host) is expected


def test_remote_bind_is_refused_without_token_and_explicit_opt_in():
    s = svc()
    for kw in ({}, {"allow_remote": True}, {"token": "t"}):
        with pytest.raises(ValueError, match="non-loopback"):
            serve(s, "0.0.0.0", 0, **kw)
    srv = serve(s, "0.0.0.0", 0, token="t", allow_remote=True)  # both: allowed
    srv.server_close()
    srv = serve(s, "127.0.0.1", 0)
    srv.server_close()


def test_cli_refuses_to_bind_publicly(tmp_path):
    p = subprocess.run([sys.executable, "-W", "ignore", "-m", "ai.server", "--host", "0.0.0.0", "--port", "0",
                        "--model", str(tmp_path / "m.json"), "--log", str(tmp_path / "d.jsonl")],
                       capture_output=True, text=True, timeout=30)
    assert p.returncode != 0 and "non-loopback" in (p.stderr + p.stdout)


# ------------------------------------------------------------------------- auth
def test_bearer_token_is_required_on_every_endpoint_when_configured():
    srv = running(svc(), token="s3cret")
    try:
        for method, path, data in (("GET", "/v1/health", None), ("GET", "/v1/learner", None),
                                   ("POST", "/v1/decision", body()), ("POST", "/v1/reset", b"{}")):
            assert call(srv, method, path, data)[0] == 401, (method, path)
            assert call(srv, method, path, data, {"Authorization": "Bearer wrong"})[0] == 401
            assert call(srv, method, path, data, {"Authorization": "s3cret"})[0] == 401
        assert call(srv, "GET", "/v1/health", headers={"Authorization": "Bearer s3cret"})[0] == 200
        code, resp = call(srv, "POST", "/v1/decision", body(), {"Authorization": "Bearer s3cret"})
        assert code == 200 and resp["decision_id"] == "w-1"
    finally:
        srv.shutdown()


def test_without_a_token_loopback_binding_is_the_protection():
    srv = running(svc())
    try:
        assert call(srv, "GET", "/v1/health")[0] == 200
    finally:
        srv.shutdown()


# ---------------------------------------------------- timeouts and connection limits
def test_stalled_client_is_disconnected_after_the_request_timeout():
    srv = running(svc(), request_timeout=0.6)
    try:
        s = socket.create_connection(("127.0.0.1", srv.server_port), timeout=5)
        s.sendall(b"POST /v1/decision HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n")  # never finishes the request
        t0 = time.time()
        s.settimeout(5)
        try:
            data = s.recv(1024)
        except (ConnectionResetError, socket.timeout):
            data = b""
        assert data == b"" or b"408" in data or b"400" in data
        assert time.time() - t0 < 4, "the server held a stalled connection open"
        s.close()
        assert call(srv, "GET", "/v1/health")[0] == 200  # still serving
    finally:
        srv.shutdown()


def test_connection_cap_answers_503_instead_of_spawning_threads():
    srv = running(svc(), max_connections=2, request_timeout=3.0)
    held = []
    try:
        for _ in range(2):  # two idle connections occupy both slots
            s = socket.create_connection(("127.0.0.1", srv.server_port), timeout=5)
            s.sendall(b"GET /v1/health HTTP/1.1\r\nHost: x\r\n")  # incomplete: the handler waits
            held.append(s)
        time.sleep(0.3)
        s3 = socket.create_connection(("127.0.0.1", srv.server_port), timeout=5)
        s3.sendall(b"GET /v1/health HTTP/1.1\r\nHost: x\r\n\r\n")
        s3.settimeout(3)
        assert s3.recv(1024).startswith(b"HTTP/1.1 503")
        s3.close()
        assert srv.rejected >= 1
        for s in held:
            s.close()
        time.sleep(0.5)
        assert call(srv, "GET", "/v1/health")[0] == 200  # capacity is released
    finally:
        for s in held:
            s.close()
        srv.shutdown()


# ------------------------------------------------------------------ strict bodies
def test_reset_body_is_strict_and_bounded():
    s = svc()
    srv = running(s)
    try:
        assert call(srv, "POST", "/v1/reset", b'{"archive": true, "reason": "x"}')[0] == 200
        for bad in (b'{"archive": "yes"}', b'{"reason": 5}', b'{"archive": true, "evil": 1}', b'[1]', b'NaN', b'{"x":'):
            assert call(srv, "POST", "/v1/reset", bad)[0] == 400, bad
        assert call(srv, "POST", "/v1/reset", b'{"reason": "' + b"a" * 5000 + b'"}')[0] == 413
        assert s.resets == 1  # only the valid request reset anything
    finally:
        srv.shutdown()


def test_decision_endpoint_rejects_nan_negative_and_unknown_values():
    srv = running(svc())
    try:
        d = json.loads(body())
        raw = json.dumps(d).replace('"rps_normalized": 0.4', '"rps_normalized": NaN').encode()
        assert call(srv, "POST", "/v1/decision", raw)[0] == 400
        d["features"]["global"]["rps_normalized"] = -0.2
        assert call(srv, "POST", "/v1/decision", json.dumps(d).encode())[0] == 400
        d = json.loads(body())
        d["allowed_strategies"] = ["round_robin", "teleport"]
        assert call(srv, "POST", "/v1/decision", json.dumps(d).encode())[0] == 400
        d = json.loads(body())
        d["features"]["backends"][0]["id"] = "bad id!"
        assert call(srv, "POST", "/v1/decision", json.dumps(d).encode())[0] == 400
        assert call(srv, "POST", "/v1/decision", b"x" * 70000)[0] == 413
    finally:
        srv.shutdown()


# ------------------------------------------------------------------ bounded logs, privacy
def test_decision_log_is_size_bounded_with_backups(tmp_path):
    p = str(tmp_path / "d.jsonl")
    log = DecisionLog(p, max_memory=10, max_bytes=3000, backups=2)
    for i in range(400):
        log.append({"event": "decision", "i": i, "pad": "x" * 60})
    files = sorted(f for f in os.listdir(tmp_path) if f.startswith("d.jsonl"))
    assert files == ["d.jsonl", "d.jsonl.1", "d.jsonl.2"]
    for f in files:
        assert os.path.getsize(tmp_path / f) <= 3000
    assert len(log.records) == 10                                   # in-memory ring is bounded too
    if os.name == "posix":
        assert oct(os.stat(p).st_mode & 0o777) == "0o600"


ALLOWED_KEYS = {
    "decision": {"event", "t", "decision_id", "action", "strategy", "scores", "explored", "learner", "contextual", "context",
                 "current", "guard_overridden"},
    "outcome": {"event", "t", "decision_id", "target", "action", "reward", "breakdown"},
    "retain": {"event", "t", "decision_id", "reason", "age_s"},
    "outcome_ignored": {"event", "t", "decision_id", "reason", "target"},
}


def test_logs_contain_only_numeric_features_and_decision_metadata(tmp_path):
    s = svc(tmp_path, log=DecisionLog(str(tmp_path / "d.jsonl"), max_memory=100))
    for i in range(1, 5):
        d = json.loads(body(i))
        d["features"]["backends"][0]["id"] = "Backend-1"          # an identifier-like value in the request
        if i > 1:
            d["previous_outcome"] = {"decision_id": f"w-{i-1}", "window_seconds": 20, "requests": 2400, "errors": 12,
                                     "p95_ms": 80.0, "clean_exposure": True,
                                     "backends": [{"id": f"Backend-{k}", "requests": 800, "healthy": True, "weight": 1} for k in (1, 2, 3)]}
        assert s.handle(json.dumps(d).encode())[0] == 200
    text = open(tmp_path / "d.jsonl", encoding="utf-8").read()
    rows = [json.loads(l) for l in text.splitlines()]
    assert rows
    for r in rows:
        assert set(r) <= ALLOWED_KEYS[r["event"]], (r["event"], set(r) - ALLOWED_KEYS[r["event"]])
    for needle in ("features", "observed_at", "allowed_strategies", "Backend-"):
        assert needle not in text, f"raw request data ({needle}) leaked into the decision log"


# ------------------------------------------------------------------ model-load failure
def test_a_corrupt_model_does_not_crash_the_service_and_reports_a_clear_error(tmp_path):
    good = svc(tmp_path)
    for i in range(1, 4):
        d = json.loads(body(i))
        if i > 1:
            d["previous_outcome"] = {"decision_id": f"w-{i-1}", "window_seconds": 20, "requests": 2400, "errors": 12,
                                     "p95_ms": 80.0, "clean_exposure": True,
                                     "backends": [{"id": f"Backend-{k}", "requests": 800, "healthy": True, "weight": 1} for k in (1, 2, 3)]}
        good.handle(json.dumps(d).encode())
    path = str(tmp_path / "m.json")
    for p in (path, path + ".prev"):
        if os.path.exists(p):
            open(p, "w").write("{ truncated")
    bad = svc(tmp_path)
    srv = running(bad)
    try:
        code, resp = call(srv, "POST", "/v1/decision", body(10))
        assert code == 500 and resp["code"] == "model_incompatible" and "model file" in resp["error"]
        assert call(srv, "GET", "/v1/health")[0] == 200                     # the process is alive and honest about it
        assert call(srv, "POST", "/v1/decision", body(11))[0] == 500         # it keeps refusing rather than guessing
    finally:
        srv.shutdown()
    # the operator's explicit choice: archive the unusable model and start fresh
    fresh = svc(tmp_path, on_incompatible="reset")
    assert fresh.handle(body(12))[0] == 200 and fresh.updates == 0


def test_a_refused_model_is_never_overwritten_by_shutdown_or_periodic_checkpoints(tmp_path):
    from ai.reward import RewardConfig
    path = str(tmp_path / "m.json")
    good = svc(tmp_path)
    for i in range(1, 4):
        d = json.loads(body(i))
        if i > 1:
            d["previous_outcome"] = {"decision_id": f"w-{i-1}", "window_seconds": 20, "requests": 2400, "errors": 12,
                                     "p95_ms": 80.0, "clean_exposure": True,
                                     "backends": [{"id": f"Backend-{k}", "requests": 800, "healthy": True, "weight": 1} for k in (1, 2, 3)]}
        good.handle(json.dumps(d).encode())
    valid_bytes = open(path, "rb").read()
    # (a) valid but incompatible (different reward configuration) -> refused
    other = DecisionService(LinUCB(CD, alpha=0.3), model_path=path, clock=lambda: T0 + 1,
                            reward=RewardConfig(w_latency=0.5, w_throughput=0.1), log=DecisionLog(max_memory=50))
    assert other.handle(body(20))[0] == 500
    other.checkpoint(force=True, reason="periodic")
    other.stop()                                                     # graceful shutdown writes a final checkpoint ...
    assert open(path, "rb").read() == valid_bytes, "the refused model was overwritten"   # ... but never over a refused model
    # (b) corrupt -> refused, file untouched for the operator
    open(path, "w").write("{ corrupt")
    if os.path.exists(path + ".prev"):
        os.remove(path + ".prev")                                    # no fallback checkpoint: the model is unusable
    broken = svc(tmp_path)
    assert broken.handle(body(21))[0] == 500
    broken.stop()
    assert open(path).read() == "{ corrupt"


def test_server_is_a_bounded_server():
    assert issubclass(BoundedServer, __import__("http.server").server.ThreadingHTTPServer)
