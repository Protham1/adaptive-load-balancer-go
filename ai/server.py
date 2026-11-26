"""The learner as a separate process (Phases 10 and 19): POST /v1/decision, GET /v1/health,
GET /v1/learner (introspection for the dashboard), POST /v1/reset (reset-to-baseline for experiments).

Go sends ONE aggregated snapshot per control interval (never per HTTP request). Each request may carry
the outcome of the previous decision; the service turns it into a reward and updates the learner only if
Go marked the exposure clean.

    python -m ai.server --port 8090 --algo linucb --model ai/model.json

The service never decides *whether* to apply anything: Go's policy manager validates and may reject every
proposal. When the snapshot is stale or invalid the service answers with the current strategy and a
`retain_*` diagnostic.

Terminology: a contextual bandit is online learning, but it is NOT full reinforcement learning: each
decision is treated as an independent (context, action, reward) sample and the model does not represent
long-term state transitions or discounted returns.

Online learning and persistence (Phase 19)
  * incremental: one O(d^2) Sherman-Morrison update per evaluated window; nothing is retrained from scratch
  * checkpoints: atomic, checksummed; every N updates, periodically (dirty only) and on graceful shutdown;
    the previous checkpoint is kept as <model>.prev and used if the primary file is corrupt
  * restart: algorithm, dimension, FEATURE SCHEMA hash, REWARD configuration, State version and backend
    set are validated before loading; an incompatible model is refused (or archived with --on-incompatible reset)
  * drift: recent reward (Page-Hinkley) and workload distribution (PSI) are tracked and flagged, never acted on
  * bounded exploration: ExplorationGuard caps repeated exploration; first-time tries stay allowed
  * reset: POST /v1/reset archives the model and starts from scratch (the log records the reset event)
"""
from __future__ import annotations

import argparse
import hmac
import ipaddress
import os
import signal
import threading
import time
from collections import OrderedDict
from dataclasses import asdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable, Optional, Tuple
from urllib.parse import parse_qs, urlparse

import numpy as np

from .actions import ACTION_STRATEGIES, WIRE_NAMES
from .bandit import Learner, NoAllowedAction, make_learner
from .contract import (MAX_BODY_BYTES, SCHEMA_VERSION, ContractError, DecisionRequest, dumps_strict,
                       loads_strict, parse_request)
from .drift import DriftMonitor
from .guard import ExplorationGuard
from .persistence import (DecisionLog, PersistenceError, archive_model, hash_json, load_model_ex, now_iso,
                          save_model)
from .replay import make_like
from .reward import RewardConfig, WindowOutcome, compute_reward
from .schemas import StateSnapshot
from .state import (EXPECTED_STATE_VERSION, build_context, context_dim, feature_schema_hash, validate_state)

PENDING_LIMIT = 256
LEARNER_VIEW_LIMIT = 100


class DecisionService:
    """Transport-independent core; `handle()` maps a body to (HTTP status, payload)."""

    def __init__(self, learner: Learner, context_mode: str = "compact", reward: RewardConfig = RewardConfig(),
                 model_path: Optional[str] = None, max_age_s: float = 15.0,
                 clock: Callable[[], float] = time.time, seed: int = 0, log: Optional[DecisionLog] = None,
                 checkpoint_every_updates: int = 1, checkpoint_interval_s: float = 30.0,
                 guard: Optional[ExplorationGuard] = None, drift: Optional[DriftMonitor] = None,
                 allow_reward_change: bool = False, on_incompatible: str = "refuse", predictor=None):
        self.learner, self.context_mode, self.reward_cfg = learner, context_mode, reward.validate()
        self.model_path, self.max_age_s, self.clock = model_path, max_age_s, clock
        self.seed = seed
        self.rng = np.random.default_rng(seed)
        self.log = log or DecisionLog(max_memory=2000)
        self.updates = 0
        self.decisions = 0
        self.backend_ids: Optional[tuple] = None
        self._model_loaded = model_path is None
        self._pending: "OrderedDict[str, Tuple[np.ndarray, int]]" = OrderedDict()
        self._lock = threading.RLock()
        self.guard, self.drift, self.predictor = guard, drift or DriftMonitor(), predictor
        if self.guard:
            self.guard.clamp_learner(self.learner)
        self.allow_reward_change = allow_reward_change
        if on_incompatible not in ("refuse", "reset"):
            raise ValueError("on_incompatible must be 'refuse' or 'reset'")
        self.on_incompatible = on_incompatible
        self.checkpoint_every, self.checkpoint_interval = max(1, checkpoint_every_updates), checkpoint_interval_s
        self._unsaved = 0
        self.last_checkpoint: Optional[dict] = None
        self.persistence_info: dict = {}
        self._ckpt_stop = threading.Event()
        self._ckpt_thread: Optional[threading.Thread] = None
        self.started_at = now_iso()
        self.resets = 0

    @property
    def model_version(self) -> str:
        return f"{self.learner.name}-v1+{self.updates}u"[:64]

    # ------------------------------------------------------------------ persistence
    def _extra_names(self) -> tuple:
        return tuple(self.predictor.feature_names()) if self.predictor else ()

    def _meta(self, dim: int) -> dict:
        ids = list(self.backend_ids or [])
        meta = {"context_mode": self.context_mode, "dim": dim, "state_version": EXPECTED_STATE_VERSION,
                "backend_ids": ids,
                "feature_schema_hash": feature_schema_hash(self.context_mode, ids, extra=self._extra_names()),
                "reward_config": asdict(self.reward_cfg), "reward_hash": hash_json(asdict(self.reward_cfg))}
        return meta

    def _expect_meta(self, dim: int) -> dict:
        m = self._meta(dim)
        if self.allow_reward_change:  # operator accepts that reward semantics changed
            m.pop("reward_config"), m.pop("reward_hash")
        return m

    def checkpoint(self, force: bool = False, reason: str = "update") -> bool:
        """Write the model checkpoint (atomic + checksum). Returns True if a file was written."""
        with self._lock:
            if not self.model_path or self.backend_ids is None:
                return False
            if not self._model_loaded:
                # The existing model has not been loaded (or was REFUSED): never overwrite it with an empty
                # model. It belongs to the operator to inspect, repair, archive or reset.
                return False
            if not force and self._unsaved == 0:
                return False
            dim = context_dim(self.context_mode, len(self.backend_ids)) + len(self._extra_names())
            save_model(self.model_path, self.learner, self._meta(dim),
                       {"decisions": self.decisions, "updates": self.updates, "resets": self.resets},
                       {"drift": self.drift.state_dict()})
            self._unsaved = 0
            self.last_checkpoint = {"at": now_iso(), "updates": self.updates, "reason": reason, "path": self.model_path}
            return True

    def start_checkpointer(self) -> None:
        """Periodic checkpoint thread (only writes when there are unsaved updates)."""
        if not self.model_path or self.checkpoint_interval <= 0 or self._ckpt_thread:
            return

        def loop():
            while not self._ckpt_stop.wait(self.checkpoint_interval):
                try:
                    self.checkpoint(reason="periodic")
                except Exception:  # noqa: BLE001  (never let the checkpointer kill the service)
                    pass

        self._ckpt_thread = threading.Thread(target=loop, daemon=True)
        self._ckpt_thread.start()

    def stop(self) -> None:
        """Graceful shutdown: stop the checkpointer and write a final checkpoint."""
        self._ckpt_stop.set()
        if self._ckpt_thread:
            self._ckpt_thread.join(timeout=2)
        self.checkpoint(force=True, reason="shutdown")

    def _restore(self, dim: int) -> None:
        self._model_loaded = True
        if not (self.model_path and os.path.exists(self.model_path)):
            return
        try:
            counters, info = load_model_ex(self.model_path, self.learner, self._expect_meta(dim))
        except PersistenceError as e:
            if self.on_incompatible == "reset":
                dest = archive_model(self.model_path)
                self.persistence_info = {"discarded": str(e), "archived_to": dest}
                self.log.append({"event": "model_discarded", "t": self.clock(), "reason": str(e), "archived_to": dest})
                return
            self._model_loaded = False  # refuse: keep trying (and refusing) until the operator intervenes
            raise
        self.decisions, self.updates = int(counters.get("decisions", 0)), int(counters.get("updates", 0))
        self.resets = int(counters.get("resets", 0))
        d = (info.get("extra") or {}).get("drift")
        if d:
            try:
                self.drift.load_state_dict(d)
            except (KeyError, ValueError, TypeError):
                pass  # drift state is advisory; an unreadable copy must not block the model
        info.pop("extra", None)
        self.persistence_info = {"restored": True, **{k: v for k, v in info.items() if k != "meta"}}

    # ------------------------------------------------------------------ reset
    def reset(self, archive: bool = True, reason: str = "operator") -> dict:
        """Reset-to-baseline for experiments: archive the checkpoint, forget everything the learner knows."""
        with self._lock:
            dest = archive_model(self.model_path) if (archive and self.model_path) else None
            if not archive and self.model_path:
                for p in (self.model_path, self.model_path + ".prev"):
                    if os.path.exists(p):
                        os.remove(p)
            self.learner.load_state_dict(make_like(self.learner).state_dict())
            self.updates = self.decisions = self._unsaved = 0
            self._pending.clear()
            self.drift.reset()
            if self.guard:
                self.guard.recent.clear()
                self.guard.overrides = 0
            self.resets += 1
            self._model_loaded = True
            self.log.append({"event": "reset", "t": self.clock(), "reason": reason, "archived_to": dest})
            self.persistence_info = {"reset": True, "archived_to": dest}
            return {"status": "reset", "model_version": self.model_version, "archived_to": dest, "resets": self.resets}

    # ------------------------------------------------------------------ helpers
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
        self._unsaved += 1
        self.drift.observe_reward(bd.reward)
        if self._unsaved >= self.checkpoint_every:
            self.checkpoint(reason="update")
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
        dim = context_dim(self.context_mode, len(req.backend_ids)) + len(self._extra_names())
        ldim = getattr(self.learner, "dim", None)
        if ldim is not None and ldim != dim:
            self.log_event("retain", req, reason=f"context dim {dim} != learner dim {ldim}")
            return self._respond(req, cur, "retain_invalid_state")
        if not self._model_loaded:
            self._restore(dim)

        # 4. context (+ optional forecast features), drift tracking, choice among allowed strategies only
        ctx = build_context(snap, self.context_mode)
        if self.predictor:
            ctx = self.predictor.augment(ctx, req.observed_at, req.window_seconds)
        self.drift.observe_context(ctx)
        try:
            sel = self.learner.select(ctx, req.action_mask, self.rng)
            overridden = False
            if self.guard:
                sel, overridden = self.guard.apply(self.learner, ctx, req.action_mask, self.rng, sel)
        except NoAllowedAction:
            return self._respond(req, cur, "retain_no_allowed_action")
        assert req.action_mask[sel.action]
        self.decisions += 1
        self._pending[req.decision_id] = (ctx, sel.action)
        while len(self._pending) > PENDING_LIMIT:
            self._pending.popitem(last=False)
        self.log_event("decision", req, action=sel.action, strategy=ACTION_STRATEGIES[sel.action],
                       scores=sel.scores, explored=sel.explored, learner=self.learner.name,
                       contextual=self.learner.contextual, context=ctx.tolist(), current=cur,
                       guard_overridden=overridden)
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

    # ------------------------------------------------------------------ introspection
    def health(self) -> dict:
        d = self.drift.status()
        return {"status": "ok", "schema_version": SCHEMA_VERSION, "model_version": self.model_version,
                "learner": self.learner.name, "contextual": self.learner.contextual,
                "decisions": self.decisions, "updates": self.updates, "drift_suspected": d["drift_suspected"],
                "last_checkpoint": self.last_checkpoint}

    def learner_view(self, limit: int = LEARNER_VIEW_LIMIT) -> dict:
        """Everything the dashboard needs about the learner. Contains NO confidence value: the bandit's
        scores are optimistic value estimates (UCB), not calibrated probabilities."""
        with self._lock:
            events = list(self.log.records or [])
            decisions: "OrderedDict[str, dict]" = OrderedDict()
            for e in events:
                if e.get("event") == "decision":
                    decisions[e["decision_id"]] = {
                        "decision_id": e["decision_id"], "t": e["t"], "strategy": e["strategy"], "action": e["action"],
                        "explored": bool(e.get("explored")), "scores": e.get("scores"), "current": e.get("current"),
                        "guard_overridden": bool(e.get("guard_overridden")), "reward": None, "components": None,
                        "override": None}
                elif e.get("event") == "outcome" and e.get("target") in decisions:
                    bd = e.get("breakdown") or {}
                    d = decisions[e["target"]]
                    d.update(reward=e["reward"], components=bd.get("contributions") or None,
                             override=bd.get("override"), raw=bd.get("raw"), normalized=bd.get("normalized"))
            recent = list(decisions.values())[-max(1, min(limit, 500)):]
            cum, series = 0.0, []
            for d in recent:
                if d["reward"] is not None:
                    cum += d["reward"]
                    series.append({"decision_id": d["decision_id"], "t": d["t"], "reward": d["reward"],
                                   "components": d["components"], "cumulative": cum, "override": d["override"]})
            explored = [d["explored"] for d in recent]
            ids = list(self.backend_ids or [])
            return {
                "learner": {"name": self.learner.name, "contextual": self.learner.contextual,
                            "config": self.learner.config(), "model_version": self.model_version,
                            "updates": self.updates, "decisions": self.decisions, "resets": self.resets,
                            "context_mode": self.context_mode, "state_version": EXPECTED_STATE_VERSION,
                            "feature_schema_hash": feature_schema_hash(self.context_mode, ids, extra=self._extra_names()),
                            "predictor": self.predictor.describe() if self.predictor else None,
                            "started_at": self.started_at},
                "reward_config": asdict(self.reward_cfg),
                "exploration": {"recent_explored": sum(explored), "recent_decisions": len(explored),
                                "guard": self.guard.stats() if self.guard else None},
                "drift": self.drift.status(),
                "persistence": {"path": self.model_path, "last_checkpoint": self.last_checkpoint,
                                "checkpoint_every_updates": self.checkpoint_every,
                                "checkpoint_interval_s": self.checkpoint_interval, "unsaved_updates": self._unsaved,
                                "info": self.persistence_info},
                "recent_decisions": recent, "reward_series": series,
                "confidence": {"available": False,
                               "reason": "The bandit does not estimate a calibrated confidence; 'scores' are optimistic "
                                         "(UCB) value estimates and must not be shown as probabilities."},
            }


def is_loopback_host(host: str) -> bool:
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


class BoundedServer(ThreadingHTTPServer):
    """ThreadingHTTPServer with a cap on concurrent connections: beyond it the client gets an immediate 503
    instead of spawning another thread (a flood must not exhaust the learner process)."""

    def __init__(self, addr, handler, max_connections: int = 16):
        super().__init__(addr, handler)
        self._slots = threading.BoundedSemaphore(max_connections)
        self.rejected = 0

    def process_request(self, request, client_address):
        if not self._slots.acquire(blocking=False):
            self.rejected += 1
            try:
                request.sendall(b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            except OSError:
                pass
            self.shutdown_request(request)
            return
        super().process_request(request, client_address)

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._slots.release()


RESET_KEYS = {"archive", "reason"}


def make_handler(service: DecisionService, token: Optional[str] = None, request_timeout: float = 10.0):
    class Handler(BaseHTTPRequestHandler):
        timeout = request_timeout  # socket timeout per connection: a stalled client cannot hold a thread forever

        def log_message(self, *a):  # keep stdout quiet; decisions are logged as JSONL
            pass

        def _authorized(self) -> bool:
            """Bearer-token check (constant time). With no token configured the API relies on loopback binding."""
            if not token:
                return True
            got = self.headers.get("Authorization") or ""
            ok = got.startswith("Bearer ") and hmac.compare_digest(got[7:].encode(), token.encode())
            if not ok:
                self._drain(int(self.headers.get("Content-Length") or 0))
                self._send(401, {"error": "unauthorized", "code": "unauthorized"})
            return ok

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
            if not self._authorized():
                return
            u = urlparse(self.path)
            if u.path == "/v1/health":
                self._send(200, service.health())
            elif u.path == "/v1/learner":
                try:
                    lim = int((parse_qs(u.query).get("limit") or [LEARNER_VIEW_LIMIT])[0])
                except ValueError:
                    return self._send(400, {"error": "limit must be an integer", "code": "bad_request"})
                self._send(200, service.learner_view(lim))
            else:
                self._send(404, {"error": "not found", "code": "not_found"})

        def do_POST(self):
            if not self._authorized():
                return
            if self.path == "/v1/reset":
                try:
                    n = int(self.headers.get("Content-Length") or 0)
                    if n > 4096:
                        self._drain(n)
                        return self._send(413, {"error": "body too large", "code": "too_large"})
                    body = loads_strict(self.rfile.read(n)) if n else {}
                    extra = set(body) - RESET_KEYS
                    if extra or not isinstance(body.get("archive", True), bool) or not isinstance(body.get("reason", ""), str):
                        raise ContractError(f"unknown or invalid field(s): {sorted(extra) or 'types'}")
                except (ValueError, ContractError) as e:
                    return self._send(400, {"error": f"invalid body: {e}", "code": "bad_request"})
                return self._send(200, service.reset(archive=bool(body.get("archive", True)),
                                                     reason=str(body.get("reason", "operator"))[:64]))
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


def serve(service: DecisionService, host: str = "127.0.0.1", port: int = 8090, token: Optional[str] = None,
          allow_remote: bool = False, max_connections: int = 16, request_timeout: float = 10.0) -> BoundedServer:
    """Create the HTTP server. The learner API is for the local load balancer only: binding anywhere but
    loopback is refused unless the operator explicitly allows it AND configures a bearer token."""
    if not is_loopback_host(host) and not (allow_remote and token):
        raise ValueError(f"refusing to bind the learner API to non-loopback address {host!r}: "
                         "pass --allow-remote together with --auth-token (and put it behind TLS)")
    srv = BoundedServer((host, port), make_handler(service, token, request_timeout), max_connections)
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
    ap.add_argument("--checkpoint-every", type=int, default=1, help="checkpoint after this many updates")
    ap.add_argument("--checkpoint-interval", type=float, default=30.0, help="periodic checkpoint seconds (0 = off)")
    ap.add_argument("--max-explore-fraction", type=float, default=0.5, help="cap on repeated exploration (see guard)")
    ap.add_argument("--no-guard", action="store_true", help="disable the exploration guard")
    ap.add_argument("--on-incompatible", default="refuse", choices=("refuse", "reset"))
    ap.add_argument("--allow-reward-change", action="store_true", help="load a model trained under another reward config")
    ap.add_argument("--auth-token", default=os.environ.get("AI_AGENT_TOKEN", ""), help="bearer token required on every request")
    ap.add_argument("--allow-remote", action="store_true", help="allow a non-loopback --host (requires --auth-token)")
    ap.add_argument("--max-connections", type=int, default=16, help="concurrent connections before answering 503")
    ap.add_argument("--request-timeout", type=float, default=10.0, help="per-connection socket timeout in seconds")
    ap.add_argument("--log-max-mb", type=int, default=10, help="decision log size bound (rotated, 3 backups)")
    ap.add_argument("--predict", default="none", choices=("none", "ma", "ema", "linreg"),
                    help="Phase 20 predictive layer: append forecast features to the context")
    a = ap.parse_args(argv)

    predictor = None
    dim = context_dim(a.context, a.backends)
    if a.predict != "none":
        from .predict import PredictiveContext
        predictor = PredictiveContext(a.predict)
        dim += len(predictor.feature_names())
    kw = {"alpha": a.alpha} if a.algo == "linucb" else ({"epsilon": a.epsilon} if "eps" in a.algo else {})
    learner = make_learner(a.algo, dim, **kw)
    guard = None if a.no_guard else ExplorationGuard(max_fraction=a.max_explore_fraction)
    svc = DecisionService(learner, a.context, model_path=a.model, max_age_s=a.max_age, seed=a.seed,
                          log=DecisionLog(a.log, max_memory=2000, max_bytes=a.log_max_mb << 20),
                          checkpoint_every_updates=a.checkpoint_every,
                          checkpoint_interval_s=a.checkpoint_interval, guard=guard,
                          allow_reward_change=a.allow_reward_change, on_incompatible=a.on_incompatible,
                          predictor=predictor)
    svc.start_checkpointer()
    try:
        srv = serve(svc, a.host, a.port, token=a.auth_token or None, allow_remote=a.allow_remote,
                    max_connections=a.max_connections, request_timeout=a.request_timeout)
    except ValueError as e:
        raise SystemExit(f"error: {e}")

    def graceful(*_):  # SIGINT / SIGTERM / SIGBREAK: stop serving, then the finally-block checkpoints
        threading.Thread(target=srv.shutdown, daemon=True).start()

    for name in ("SIGINT", "SIGTERM", "SIGBREAK"):
        if hasattr(signal, name):
            try:
                signal.signal(getattr(signal, name), graceful)
            except (ValueError, OSError):
                pass
    print(f"decision service ({learner.name}) on http://{a.host}:{a.port}/v1/decision", flush=True)
    try:
        srv.serve_forever()
    finally:
        svc.stop()
        srv.server_close()
        print("final checkpoint written", flush=True)


if __name__ == "__main__":
    main()
