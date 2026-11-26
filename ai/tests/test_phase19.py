import copy
import http.client
import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time

import numpy as np
import pytest

from ai.bandit import LinUCB, MeanEpsilonGreedy, Selection
from ai.drift import DriftMonitor, PageHinkley, psi
from ai.guard import ExplorationGuard
from ai.persistence import (DecisionLog, PersistenceError, archive_model, hash_json, load_model_ex, save_model)
from ai.replay import rebuild_from_log, states_equal, verify_against_checkpoint
from ai.reward import RewardConfig
from ai.schemas import parse_timestamp
from ai.server import DecisionService, serve
from ai.state import context_dim, context_feature_names, feature_schema_hash

FIX = os.path.join(os.path.dirname(__file__), "..", "..", "testdata", "contract", "valid", "request_basic.json")
BASE = json.load(open(FIX, encoding="utf-8"))
T0 = parse_timestamp("2026-10-13T16:00:00Z")
CD = context_dim("compact", 3)
ALL = (True,) * 5


def req(i, outcome_for=None, rps=0.4, errors=12, requests=2400):
    d = copy.deepcopy(BASE)
    d["decision_id"] = f"w-{i}"
    d["features"]["global"]["rps_normalized"] = rps
    if outcome_for:
        d["previous_outcome"] = {"decision_id": outcome_for, "window_seconds": 20, "requests": requests, "errors": errors,
                                 "p95_ms": 85.5, "clean_exposure": True,
                                 "backends": [{"id": "Backend-1", "requests": 1200, "healthy": True, "weight": 3},
                                              {"id": "Backend-2", "requests": 800, "healthy": True, "weight": 2},
                                              {"id": "Backend-3", "requests": 400, "healthy": True, "weight": 1}]}
    return json.dumps(d).encode()


def make(tmp, **kw):
    kw.setdefault("log", DecisionLog(os.path.join(tmp, "log.jsonl"), max_memory=2000))
    return DecisionService(LinUCB(CD, alpha=0.3), model_path=os.path.join(tmp, "m.json"), clock=lambda: T0 + 1, **kw)


def drive(svc, n, rps=0.4):
    """n decisions, each carrying the outcome of the previous one."""
    for i in range(n):
        code, _ = svc.handle(req(i, outcome_for=f"w-{i-1}" if i else None, rps=rps))
        assert code == 200


# ------------------------------------------------------------------ integrity + fallback
def test_checksum_detects_tampering_and_previous_checkpoint_is_used(tmp_path):
    p = str(tmp_path / "m.json")
    L = LinUCB(CD)
    meta = {"dim": CD, "x": 1}
    save_model(p, L, meta, {"updates": 1})
    L.update(np.ones(CD), 2, -0.2)
    save_model(p, L, meta, {"updates": 2})          # first checkpoint is now <p>.prev
    doc = json.load(open(p))
    doc["counters"]["updates"] = 999                 # edit without fixing the checksum
    json.dump(doc, open(p, "w"))
    fresh = LinUCB(CD)
    counters, info = load_model_ex(p, fresh, meta)
    assert info["restored_from_backup"] and counters["updates"] == 1 and "checksum" in info["primary_error"]
    open(p, "w").write("{ not json")                 # primary unreadable too -> still the backup
    assert load_model_ex(p, LinUCB(CD), meta)[1]["restored_from_backup"]
    open(p + ".prev", "w").write("garbage")          # both bad -> refuse
    with pytest.raises(PersistenceError):
        load_model_ex(p, LinUCB(CD), meta)


def test_truncated_model_with_no_backup_is_refused(tmp_path):
    p = str(tmp_path / "m.json")
    save_model(p, LinUCB(CD), {"dim": CD}, {})
    open(p, "w").write(open(p).read()[:60])
    with pytest.raises(PersistenceError):
        load_model_ex(p, LinUCB(CD), {"dim": CD})


# ------------------------------------------------------------- restart validation
def test_restart_is_refused_for_other_schema_reward_algorithm_or_backends(tmp_path):
    a = make(str(tmp_path))
    drive(a, 4)
    assert a.updates == 3 and os.path.exists(a.model_path)

    def restart(**kw):
        b = DecisionService(kw.pop("learner", LinUCB(CD, alpha=0.3)), model_path=a.model_path, clock=lambda: T0 + 1,
                            **kw)
        return b, b.handle(req(0))

    b, (code, body) = restart()
    assert code == 200 and b.updates == 3                                   # compatible -> restored
    _, (code, body) = restart(reward=RewardConfig(w_latency=0.5, w_throughput=0.1))
    assert code == 500 and "reward" in body["error"]                        # reward definition changed
    b, (code, _) = restart(reward=RewardConfig(w_latency=0.5, w_throughput=0.1), allow_reward_change=True)
    assert code == 200 and b.updates == 3                                   # operator accepts the change
    _, (code, body) = restart(learner=MeanEpsilonGreedy())
    assert code == 500                                                      # different algorithm
    # different backend set
    d = json.loads(req(0))
    for x in d["features"]["backends"]:
        x["id"] = x["id"].replace("Backend", "Node")
    c = DecisionService(LinUCB(CD, alpha=0.3), model_path=a.model_path, clock=lambda: T0 + 1)
    assert c.handle(json.dumps(d).encode())[0] == 500


def test_incompatible_model_can_be_archived_instead_of_refused(tmp_path):
    a = make(str(tmp_path))
    drive(a, 3)
    b = DecisionService(LinUCB(CD, alpha=0.3), model_path=a.model_path, clock=lambda: T0 + 1,
                        reward=RewardConfig(w_latency=0.5, w_throughput=0.1), on_incompatible="reset")
    assert b.handle(req(0))[0] == 200 and b.updates == 0
    assert b.persistence_info["archived_to"] and os.path.exists(b.persistence_info["archived_to"])


def test_feature_schema_hash_changes_with_layout():
    ids = ["Backend-1", "Backend-2", "Backend-3"]
    h = feature_schema_hash("compact", ids)
    assert h == feature_schema_hash("compact", ids) and h != feature_schema_hash("full", ids)
    assert h != feature_schema_hash("compact", ids, extra=("forecast.rps",))
    assert feature_schema_hash("full", ids) != feature_schema_hash("full", ids[:2])
    assert len(context_feature_names("compact", ids)) == CD == 15
    assert len(context_feature_names("full", ids)) == context_dim("full", 3)


# ------------------------------------------------------------- reproducibility
def test_restart_reproduces_the_model_and_decision_history_replays_to_it(tmp_path):
    a = make(str(tmp_path))
    drive(a, 12)
    scores_before = a.learner.select(np.full(CD, 0.3), ALL, np.random.default_rng(1)).scores
    b = DecisionService(LinUCB(CD, alpha=0.3), model_path=a.model_path, clock=lambda: T0 + 1)
    b.handle(req(100))                                                       # first request triggers the restore
    assert b.updates == a.updates == 11 and b.decisions >= a.decisions
    assert b.learner.select(np.full(CD, 0.3), ALL, np.random.default_rng(1)).scores == scores_before
    # the decision history alone reproduces the checkpoint exactly
    r = verify_against_checkpoint(str(tmp_path / "log.jsonl"), a.model_path, LinUCB(CD, alpha=0.3))
    assert r["identical"] and r["updates_replayed"] == 11


def test_replay_honours_reset_events(tmp_path):
    a = make(str(tmp_path))
    drive(a, 6)
    a.reset(archive=True)
    drive(a, 4)
    fresh = LinUCB(CD, alpha=0.3)
    assert rebuild_from_log(str(tmp_path / "log.jsonl"), fresh) == 3          # only the post-reset lineage
    assert states_equal(fresh.state_dict(), a.learner.state_dict())


# ---------------------------------------------------------------- checkpoint triggers
def test_checkpoint_cadence_periodic_and_shutdown(tmp_path):
    svc = make(str(tmp_path), checkpoint_every_updates=3, checkpoint_interval_s=0)
    drive(svc, 3)                                                            # 2 updates: below the cadence
    assert svc.updates == 2 and svc._unsaved == 2 and svc.last_checkpoint is None
    drive(svc, 2)                                                            # re-sends w-0.. ids; updates continue
    assert svc.last_checkpoint and svc.last_checkpoint["reason"] == "update"
    svc._unsaved = 1
    svc.stop()                                                               # graceful shutdown writes a final one
    assert svc.last_checkpoint["reason"] == "shutdown"

    p = make(str(tmp_path / "p"), checkpoint_every_updates=10_000, checkpoint_interval_s=0.05)
    os.makedirs(str(tmp_path / "p"), exist_ok=True)
    p.start_checkpointer()
    drive(p, 3)
    deadline = time.time() + 3
    while time.time() < deadline and not os.path.exists(p.model_path):
        time.sleep(0.05)
    p.stop()
    assert os.path.exists(p.model_path) and p.last_checkpoint["updates"] == 2


def test_graceful_shutdown_of_a_real_process_writes_a_final_checkpoint(tmp_path):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    model = str(tmp_path / "model.json")
    kw = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {}
    proc = subprocess.Popen([sys.executable, "-W", "ignore", "-m", "ai.server", "--port", str(port), "--model", model,
                             "--log", str(tmp_path / "d.jsonl"), "--checkpoint-every", "1000",
                             "--checkpoint-interval", "0"],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, **kw)
    try:
        for _ in range(100):
            try:
                socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
                break
            except OSError:
                time.sleep(0.1)
        d = json.loads(req(1))
        d["observed_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        c.request("POST", "/v1/decision", json.dumps(d), {"Content-Type": "application/json"})
        assert c.getresponse().status == 200
        d2 = json.loads(req(2, outcome_for="w-1"))
        d2["observed_at"] = d["observed_at"]
        c.request("POST", "/v1/decision", json.dumps(d2), {"Content-Type": "application/json"})
        assert c.getresponse().status == 200
        assert not os.path.exists(model)                                      # nothing written yet (cadence 1000)
        proc.send_signal(signal.CTRL_BREAK_EVENT if os.name == "nt" else signal.SIGTERM)
        proc.wait(timeout=10)
    finally:
        if proc.poll() is None:
            proc.kill()
    assert os.path.exists(model), "graceful shutdown must write the final checkpoint"
    doc = json.load(open(model))
    assert doc["counters"]["updates"] == 1 and doc["checksum"] == hash_json(
        {k: doc.get(k) for k in ("learner", "config", "meta", "counters", "state", "extra")})


# ------------------------------------------------------------------------- drift
def test_page_hinkley_flags_reward_drop_but_not_noise():
    rng = np.random.default_rng(0)
    ph = PageHinkley(delta=0.02, lambda_=0.5)
    assert not any(ph.update(float(-0.1 + rng.normal(0, 0.03))) for _ in range(200))
    ph2 = PageHinkley(delta=0.02, lambda_=0.5)
    for _ in range(30):
        ph2.update(float(-0.1 + rng.normal(0, 0.03)))
    assert not ph2.alarm
    for _ in range(40):
        ph2.update(float(-0.6 + rng.normal(0, 0.03)))                         # sustained degradation
    assert ph2.alarm


def test_psi_known_values_and_workload_shift_detection():
    x = np.random.default_rng(1).uniform(0.0, 0.3, 200)
    assert psi(x, x) == pytest.approx(0.0, abs=1e-9)
    assert psi(x, np.random.default_rng(2).uniform(0.0, 0.3, 200)) < 0.1
    assert psi(x, np.random.default_rng(3).uniform(0.7, 1.0, 200)) > 0.25
    m = DriftMonitor(ref_n=30, recent_n=30)
    rng = np.random.default_rng(4)
    for _ in range(60):
        ctx = np.zeros(CD)
        ctx[0] = rng.uniform(0.05, 0.25)
        m.observe_context(ctx)
    assert not m.status()["drift_suspected"]
    for _ in range(35):                                                       # traffic jumps: rps feature moves up
        ctx = np.zeros(CD)
        ctx[0] = rng.uniform(0.75, 0.95)
        m.observe_context(ctx)
    st = m.status()
    assert st["drift_suspected"] and any("rps" in r for r in st["reasons"])


def test_drift_state_survives_restart(tmp_path):
    a = make(str(tmp_path))
    drive(a, 25)
    ref_len = len(a.drift.ref["rps"])
    b = DecisionService(LinUCB(CD, alpha=0.3), model_path=a.model_path, clock=lambda: T0 + 1)
    b.handle(req(200))
    assert ref_len > 0 and len(b.drift.ref["rps"]) >= ref_len


# ----------------------------------------------------------------- bounded exploration
class _AlwaysExplore:
    """Stub learner: every selection is 'exploration' of a previously-tried action 1."""
    name, contextual = "stub", True

    def __init__(self):
        self.n = {0: 5, 1: 5, 2: 0}
        self.alpha = 5.0

    def pulls(self, a):
        return self.n[a]

    def greedy(self, x, mask, rng):
        return Selection(0, [1, 0, 0, None, 0], False)


def test_exploration_guard_caps_repeated_exploration_but_not_first_time_tries():
    g = ExplorationGuard(max_fraction=0.5, window=10, max_alpha=1.0)
    L = _AlwaysExplore()
    g.clamp_learner(L)
    assert L.alpha == 1.0
    used = []
    for _ in range(40):
        sel, over = g.apply(L, None, ALL, None, Selection(1, [0, 1, 0, None, 0], True))
        used.append(sel.explored)
    # steady state: at most half of the window is exploration; the rest were overridden to greedy
    assert sum(used[-10:]) <= 5 and g.overrides > 0
    assert g.explore_fraction() <= 0.5 + 1e-9
    g2 = ExplorationGuard(max_fraction=0.0, window=10)
    for _ in range(10):                                                       # action 2 was never tried: always allowed
        sel, over = g2.apply(L, None, ALL, None, Selection(2, [0, 0, 1, None, 0], True))
        assert sel.action == 2 and not over


def test_guard_is_wired_into_the_service(tmp_path):
    """With a zero exploration budget, a decision may only be 'explored' if its action had never been updated
    before (first-time try); every repeated exploration must have been overridden to the greedy choice."""
    svc = make(str(tmp_path), guard=ExplorationGuard(max_fraction=0.0, window=5))
    drive(svc, 30)
    pulls, first_time, repeated_kept, overridden = {}, 0, 0, 0
    chosen = {}
    for e in svc.log.records:
        if e["event"] == "decision":
            chosen[e["decision_id"]] = e["action"]
            overridden += bool(e["guard_overridden"])
            if e["explored"]:
                if pulls.get(e["action"], 0) == 0:
                    first_time += 1
                elif not e["guard_overridden"]:
                    repeated_kept += 1
        elif e["event"] == "outcome":
            pulls[e["action"]] = pulls.get(e["action"], 0) + 1
    assert repeated_kept == 0, "a repeated exploration slipped through a zero budget"
    assert first_time >= 1


# ------------------------------------------------------------------------- reset
def test_reset_archives_clears_and_is_logged(tmp_path):
    svc = make(str(tmp_path))
    drive(svc, 8)
    assert svc.updates == 7
    out = svc.reset(archive=True, reason="experiment-start")
    assert out["status"] == "reset" and out["archived_to"] and os.path.exists(out["archived_to"])
    assert not os.path.exists(svc.model_path)
    assert svc.updates == 0 and svc.decisions == 0 and not svc._pending
    assert np.allclose(svc.learner.model.b, 0) and svc.drift.observations == 0
    assert any(e["event"] == "reset" and e["reason"] == "experiment-start" for e in svc.log.records)
    drive(svc, 3)
    assert svc.updates == 2                                                   # learns from scratch afterwards


def test_archive_model_moves_prev_too(tmp_path):
    p = str(tmp_path / "m.json")
    save_model(p, LinUCB(CD), {}, {})
    save_model(p, LinUCB(CD), {}, {})
    dest = archive_model(p)
    assert dest and os.path.exists(dest) and os.path.exists(dest + ".prev") and not os.path.exists(p)
    assert archive_model(p) is None


# --------------------------------------------------- learner view + HTTP + no confidence
def test_learner_view_contents_and_no_invented_confidence(tmp_path):
    svc = make(str(tmp_path))
    drive(svc, 6)
    v = svc.learner_view()
    assert v["learner"]["contextual"] and v["learner"]["model_version"].startswith("linucb-v1+")
    assert v["reward_config"]["w_latency"] == pytest.approx(0.35) and v["persistence"]["last_checkpoint"]
    assert len(v["recent_decisions"]) == 6 and v["reward_series"]
    last = v["reward_series"][-1]
    assert last["cumulative"] == pytest.approx(sum(r["reward"] for r in v["reward_series"]))
    assert set(last["components"]) == {"throughput", "latency", "error", "imbalance"}
    assert v["confidence"]["available"] is False
    assert "confidence" not in json.dumps({k: x for k, x in v.items() if k != "confidence"}).lower()
    assert v["drift"]["page_hinkley"]["n"] == 5


def test_http_learner_and_reset_endpoints(tmp_path):
    svc = make(str(tmp_path))
    drive(svc, 4)
    srv = serve(svc, "127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        c = http.client.HTTPConnection("127.0.0.1", srv.server_port, timeout=5)
        c.request("GET", "/v1/learner?limit=2")
        r = c.getresponse()
        body = json.loads(r.read())
        assert r.status == 200 and len(body["recent_decisions"]) == 2
        c.request("GET", "/v1/learner?limit=abc")
        r = c.getresponse()
        r.read()
        assert r.status == 400
        c.request("POST", "/v1/reset", json.dumps({"archive": True, "reason": "test"}), {"Content-Type": "application/json"})
        r = c.getresponse()
        out = json.loads(r.read())
        assert r.status == 200 and out["status"] == "reset" and out["resets"] == 1
        c.request("GET", "/v1/health")
        h = json.loads(c.getresponse().read())
        assert h["updates"] == 0 and "drift_suspected" in h
    finally:
        srv.shutdown()
