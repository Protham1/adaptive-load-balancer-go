"""Phase 10 - the learner as a separate process: POST /v1/decision, GET /v1/health.

Go sends ONE aggregated snapshot per control interval (never per HTTP request).
Each request may carry the outcome of the previous decision; the service turns it
into a reward and updates the learner only if Go marked the exposure clean.

    python -m ai.server --port 8090 --algo linucb --model ai/model.json

The service never decides *whether* to apply anything: Go's policy manager
validates and may reject every proposal. When the snapshot is stale or invalid the
service answers with the current strategy and a `retain_*` diagnostic.
"""
from __future__ import annotations

import argparse
import threading
import time
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable, Optional, Tuple

import numpy as np

from .actions import ACTION_STRATEGIES, NUM_ACTIONS, WIRE_NAMES
from .bandit import Learner, NoAllowedAction, make_learner
from .contract import (MAX_BODY_BYTES, SCHEMA_VERSION, ContractError, DecisionRequest, dumps_strict,
                       loads_strict, parse_request)
from .persistence import DecisionLog, PersistenceError, load_model, save_model
from .reward import RewardConfig, WindowOutcome, compute_reward
from .schemas import StateSnapshot
from .state import EXPECTED_STATE_VERSION, build_context, context_dim, validate_state

PENDING_LIMIT = 256


class DecisionService:
    """Transport-independent core; `handle()` maps a body to (HTTP status, payload)."""

    def __init__(self, learner: Learner, context_mode: str = "compact", reward: RewardConfig = RewardConfig(),
                 model_path: Optional[str] = None, max_age_s: float = 15.0,
                 clock: Callable[[], float] = time.time, seed: int = 0, log: Optional[DecisionLog] = None):
        self.learner, self.context_mode, self.reward_cfg = learner, context_mode, reward.validate()
        self.model_path, self.max_age_s, self.clock = model_path, max_age_s, clock
        self.rng = np.random.default_rng(seed)
        self.log = log or DecisionLog()
        self.updates = 0
        self.decisions = 0
        self.backend_ids: Optional[tuple] = None
        self._model_loaded = model_path is None
        self._pending: "OrderedDict[str, Tuple[np.ndarray, int]]" = OrderedDict()
        self._lock = threading.Lock()

    @property
    def model_version(self) -> str:
        return f"{self.learner.name}-v1+{self.updates}u"[:64]

    # ------------------------------------------------------------------ helpers
    def _meta(self, dim: int) -> dict:
        return {"context_mode": self.context_mode, "dim": dim, "state_version": EXPECTED_STATE_VERSION,
                "backend_ids": list(self.backend_ids or [])}

    def _respond(self, req: DecisionRequest, strategy: str, diagnostic: str, **extra) -> dict:
        resp = {"schema_version": SCHEMA_VERSION, "decision_id": req.decision_id, "strategy": strategy,
                "model_version": self.model_version, "diagnostic": diagnostic}
        resp.update(extra)
        return resp

    def _learn_from(self, req: DecisionRequest) -> Optional[dict]:
        o = req.previous_outcome
        if o is None:
            return None
        pend = self._pending.pop(o.decision_id, None)
        if pend is None:
            return self.log_event("outcome_ignored", req, reason="unknown or expired decision_id", target=o.decision_id)
        if not o.clean_exposure:
            return self.log_event("outcome_ignored", req, reason="exposure not clean", target=o.decision_id)
        ctx, action = pend
        outcome = WindowOutcome(
            duration_s=o.window_seconds, requests=o.requests, errors=o.errors, p95_ms=o.p95_ms,
            backend_requests={b[0]: b[1] for b in o.backends}, backend_capacity={b[0]: float(b[3]) for b in o.backends},
            healthy_at_start={b[0]: b[2] for b in o.backends}, healthy_at_end={b[0]: b[2] for b in o.backends})
        bd = compute_reward(outcome, self.reward_cfg)
        if bd.reward is None:
            return self.log_event("outcome_ignored", req, reason=bd.reason, target=o.decision_id)
        self.learner.update(ctx, action, bd.reward)
        self.updates += 1
        if self.model_path:
            save_model(self.model_path, self.learner, self._meta(len(ctx)),
                       {"decisions": self.decisions, "updates": self.updates})
        return self.log_event("outcome", req, target=o.decision_id, action=action, reward=bd.reward,
                              breakdown=bd.to_dict())

    def log_event(self, event: str, req: DecisionRequest, **kw) -> dict:
        rec = {"event": event, "t": self.clock(), "decision_id": req.decision_id, **kw}
        self.log.append(rec)
        return rec

    # ------------------------------------------------------------------- decide
    def decide(self, req: DecisionRequest) -> dict:
        # 1. learn from the previous window first (reward attribution is by decision id)
        self._learn_from(req)
        cur = req.current_strategy

        # 2. freshness
        age = self.clock() - req.observed_at
        if age > self.max_age_s or age < -self.max_age_s:
            self.log_event("retain", req, reason="stale_snapshot", age_s=age)
            return self._respond(req, cur, "retain_stale_snapshot")

        # 3. state validity (same checks as the pull-based controller)
        snap = StateSnapshot(
            state_version=EXPECTED_STATE_VERSION, timestamp=req.observed_at, snapshot_age_ms=max(age, 0) * 1000,
            is_stale=False, safe_policy_retain=False, missing_critical=False, reason="", vector=req.vector,
            feature_names=tuple(f"f{i}" for i in range(len(req.vector))), backend_ids=req.backend_ids)
        val = validate_state(snap, backend_ids=self.backend_ids, max_age_ms=self.max_age_s * 1000)
        if not val.ok:
            self.log_event("retain", req, reason=val.reason)
            return self._respond(req, cur, "retain_backend_set_changed" if "backend id set" in val.reason
                                 else "retain_invalid_state")
        if self.backend_ids is None:
            self.backend_ids = req.backend_ids
        dim = context_dim(self.context_mode, len(req.backend_ids))
        ldim = getattr(self.learner, "dim", None)
        if ldim is not None and ldim != dim:
            self.log_event("retain", req, reason=f"context dim {dim} != learner dim {ldim}")
            return self._respond(req, cur, "retain_invalid_state")
        if not self._model_loaded:
            self._model_loaded = True
            import os
            if self.model_path and os.path.exists(self.model_path):
                counters = load_model(self.model_path, self.learner, self._meta(dim))
                self.decisions, self.updates = int(counters.get("decisions", 0)), int(counters.get("updates", 0))

        # 4. choose among allowed strategies only
        ctx = build_context(snap, self.context_mode)
        try:
            sel = self.learner.select(ctx, req.action_mask, self.rng)
        except NoAllowedAction:
            return self._respond(req, cur, "retain_no_allowed_action")
        assert req.action_mask[sel.action]
        self.decisions += 1
        self._pending[req.decision_id] = (ctx, sel.action)
        while len(self._pending) > PENDING_LIMIT:
            self._pending.popitem(last=False)
        self.log_event("decision", req, action=sel.action, strategy=ACTION_STRATEGIES[sel.action],
                       scores=sel.scores, explored=sel.explored, learner=self.learner.name,
                       contextual=self.learner.contextual, context=ctx.tolist(), current=cur)
        scores = [None if s is None else float(s) for s in sel.scores]
        return self._respond(req, WIRE_NAMES[sel.action], "explore" if sel.explored else "ok",
                             scores=scores, explored=bool(sel.explored))

    def handle(self, body: bytes) -> Tuple[int, dict]:
        try:
            req = parse_request(loads_strict(body))
        except ContractError as e:
            return 400, {"error": str(e), "code": "bad_request"}
        with self._lock:
            try:
                return 200, self.decide(req)
            except PersistenceError as e:
                return 500, {"error": str(e), "code": "model_incompatible"}

    def health(self) -> dict:
        return {"status": "ok", "schema_version": SCHEMA_VERSION, "model_version": self.model_version,
                "learner": self.learner.name, "contextual": self.learner.contextual,
                "decisions": self.decisions, "updates": self.updates}


def make_handler(service: DecisionService):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):  # keep stdout quiet; decisions are logged as JSONL
            pass

        def _send(self, code: int, doc: dict):
            data = dumps_strict(doc)
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _drain(self, n: int):
            """Discard (a bounded amount of) the unread body so the client sees our status, not a reset."""
            left = min(max(n, 0), 1024 * 1024)
            while left > 0:
                chunk = self.rfile.read(min(left, 65536))
                if not chunk:
                    break
                left -= len(chunk)
            self.close_connection = True

        def do_GET(self):
            if self.path == "/v1/health":
                self._send(200, service.health())
            else:
                self._send(404, {"error": "not found", "code": "not_found"})

        def do_POST(self):
            if self.path != "/v1/decision":
                return self._send(404, {"error": "not found", "code": "not_found"})
            if "json" not in (self.headers.get("Content-Type") or ""):
                self._drain(int(self.headers.get("Content-Length") or 0))
                return self._send(415, {"error": "Content-Type must be application/json", "code": "bad_media_type"})
            try:
                n = int(self.headers.get("Content-Length") or "")
            except ValueError:
                return self._send(411, {"error": "Content-Length required", "code": "length_required"})
            if n > MAX_BODY_BYTES:
                self._drain(n)
                return self._send(413, {"error": "body too large", "code": "too_large"})
            code, doc = service.handle(self.rfile.read(n))
            self._send(code, doc)

    return Handler


def serve(service: DecisionService, host: str = "127.0.0.1", port: int = 8090) -> ThreadingHTTPServer:
    srv = ThreadingHTTPServer((host, port), make_handler(service))
    srv.daemon_threads = True
    return srv


def main(argv=None) -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8090)
    ap.add_argument("--algo", default="linucb", choices=("linucb", "linear-eps-greedy", "mean-eps-greedy", "random"))
    ap.add_argument("--context", default="compact", choices=("compact", "full"))
    ap.add_argument("--backends", type=int, default=3)
    ap.add_argument("--alpha", type=float, default=0.05)
    ap.add_argument("--epsilon", type=float, default=0.05)
    ap.add_argument("--model", default="ai/model.json")
    ap.add_argument("--log", default="ai/decisions.jsonl")
    ap.add_argument("--max-age", type=float, default=15.0, help="seconds before a snapshot is stale")
    ap.add_argument("--seed", type=int, default=0)
    a = ap.parse_args(argv)
    kw = {"alpha": a.alpha} if a.algo == "linucb" else ({"epsilon": a.epsilon} if "eps" in a.algo else {})
    learner = make_learner(a.algo, context_dim(a.context, a.backends), **kw)
    svc = DecisionService(learner, a.context, model_path=a.model, max_age_s=a.max_age, seed=a.seed,
                          log=DecisionLog(a.log))
    srv = serve(svc, a.host, a.port)
    print(f"decision service ({learner.name}) on http://{a.host}:{a.port}/v1/decision", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
