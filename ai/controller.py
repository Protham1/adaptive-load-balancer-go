"""Phase 8 - online bandit controller.

Loop (one decision = one evaluation window):
  1. read a validated state S_t                       -> hold (retain policy) if invalid/stale
  2. select an *allowed* action A_t                   -> learner (or forced safe action)
  3. apply it with an explicit decision_id            -> live mode only; shadow mode only records
  4. settle (metrics window must contain only the new policy), then measure
  5. compute R_t from counter deltas of the measured window
  6. update the learner with (S_t, A_t, R_t)          -> live mode only
  7. persist model + decision record
  8. repeat

Reward attribution: a window is only credited to its decision if the exposure
was clean - the active strategy stayed A_t and the service's switch counter did
not move except for our own switch. Otherwise the window is abandoned and the
learner is NOT updated (never mis-attribute a reward).
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import List, Optional, Protocol

import numpy as np

from .actions import ACTION_STRATEGIES, action_for_strategy, safe_fallback
from .bandit import Learner, NoAllowedAction
from .persistence import DecisionLog, load_model, save_model
from .reward import RewardConfig, compute_reward, outcome_from_samples
from .schemas import (MetricsSample, PolicyInfo, SchemaError, StateSnapshot,
                      parse_metrics, parse_policy, parse_state)
from .state import EXPECTED_STATE_VERSION, build_context, context_dim, validate_state


class ActionRejected(Exception):
    """The Go service refused the action (HTTP 400, e.g. masked or invalid)."""


class Environment(Protocol):
    def get_state(self) -> dict: ...
    def get_metrics(self) -> dict: ...
    def get_policy(self) -> dict: ...
    def apply_action(self, action: int, decision_id: str) -> None: ...


@dataclass
class ControllerConfig:
    mode: str = "shadow"  # "shadow": recommend only; "live": apply + learn
    context_mode: str = "compact"
    settle_s: float = 10.0  # >= Go metrics window so p95 reflects only the new policy
    measure_s: float = 20.0
    max_age_ms: float = 5000.0
    reward: RewardConfig = field(default_factory=RewardConfig)
    run_id: str = "run"
    n_backends: Optional[int] = None
    model_path: Optional[str] = None
    seed: int = 0
    predictor: Optional[object] = None  # Phase 20: PredictiveContext appending forecast features

    def __post_init__(self):
        if self.mode not in ("shadow", "live"):
            raise ValueError("mode must be 'shadow' or 'live'")
        self.reward.validate()


@dataclass
class _Pending:
    decision_id: str
    action: int
    strategy: str
    context: np.ndarray
    switch_expected: int
    settle_until: float
    forced: bool
    start: Optional[MetricsSample] = None
    measure_until: float = 0.0


class BanditController:
    def __init__(self, learner: Learner, cfg: ControllerConfig, log: Optional[DecisionLog] = None):
        self.learner, self.cfg = learner, cfg
        self.log = log or DecisionLog()
        self.rng = np.random.default_rng(cfg.seed)
        self.pending: Optional[_Pending] = None
        self.next_decision_at = float("-inf")
        self.counter = 0
        self.updates = 0
        self.backend_ids: Optional[tuple] = None
        self._force_safe = False
        self._model_loaded = cfg.model_path is None

    # ------------------------------------------------------------------ helpers
    def _meta(self, dim: int) -> dict:
        return {"context_mode": self.cfg.context_mode, "dim": dim,
                "state_version": EXPECTED_STATE_VERSION, "backend_ids": list(self.backend_ids or [])}

    def _emit(self, event: str, t: float, **kw) -> dict:
        rec = {"event": event, "t": t, "run_id": self.cfg.run_id, "mode": self.cfg.mode, **kw}
        self.log.append(rec)
        return rec

    def _save(self, dim: int) -> None:
        if self.cfg.model_path:
            save_model(self.cfg.model_path, self.learner, self._meta(dim),
                       {"decisions": self.counter, "updates": self.updates})

    def _abandon(self, t: float, reason: str) -> dict:
        p = self.pending
        self.pending = None
        self.next_decision_at = t
        return self._emit("abandon", t, decision_id=p.decision_id, action=p.action, reason=reason,
                          learner_updated=False)

    # --------------------------------------------------------------------- step
    def step(self, env: Environment) -> List[dict]:
        """Advance the controller using the current telemetry. Cheap; call it
        every few seconds. Returns the events produced by this call."""
        ev: List[dict] = []
        try:
            state = parse_state(env.get_state())
            metrics = parse_metrics(env.get_metrics())
            policy = parse_policy(env.get_policy())
        except (SchemaError, OSError, ValueError) as e:  # bad payload, unreachable service, bad JSON
            t = self.next_decision_at if self.next_decision_at != float("-inf") else 0.0
            ev.append(self._emit("hold", t, reason=f"telemetry unavailable or invalid: {e}"))
            if self.pending:  # cannot trust the window any more
                ev.append(self._abandon(t, "telemetry unavailable during window"))
            return ev
        t = metrics.t
        val = validate_state(state, self.cfg.n_backends, self.cfg.max_age_ms, self.backend_ids)
        if self.backend_ids is None and val.ok:
            self.backend_ids = tuple(state.backend_ids)
        dim = context_dim(self.cfg.context_mode, len(state.backend_ids))
        if self.cfg.predictor is not None:
            dim += len(self.cfg.predictor.feature_names())
        learner_dim = getattr(self.learner, "dim", None)
        if val.ok and learner_dim is not None and learner_dim != dim:
            val.ok, val.reasons = False, [f"context dim {dim} != learner dim {learner_dim}"]

        if val.ok and not self._model_loaded:
            self._load_model(dim)

        if self.pending is not None:
            done = self._advance_pending(t, metrics, val, dim, ev)
            if not done:
                return ev

        if t < self.next_decision_at:
            return ev
        if not val.ok:
            ev.append(self._emit("hold", t, reason=val.reason, active_strategy=metrics.strategy))
            return ev
        self._decide(env, t, state, metrics, policy, ev)
        return ev

    def _load_model(self, dim: int) -> None:
        import os
        self._model_loaded = True
        if self.cfg.model_path and os.path.exists(self.cfg.model_path):
            counters = load_model(self.cfg.model_path, self.learner, self._meta(dim))
            self.counter = int(counters.get("decisions", 0))
            self.updates = int(counters.get("updates", 0))

    # ------------------------------------------------------- pending-window logic
    def _advance_pending(self, t, metrics, val, dim, ev) -> bool:
        """Returns True when the window has finished/abandoned (a new decision may follow)."""
        p = self.pending
        if metrics.strategy != p.strategy:
            ev.append(self._abandon(t, f"policy changed during window ({metrics.strategy!r} != {p.strategy!r})"))
            return True
        if metrics.switch_count != p.switch_expected:
            ev.append(self._abandon(t, "unexpected strategy switch during window"))
            return True
        if not val.ok:
            ev.append(self._abandon(t, f"telemetry invalid during window: {val.reason}"))
            return True
        if p.start is None:
            if t >= p.settle_until:
                p.start, p.measure_until = metrics, t + self.cfg.measure_s
            return False
        if t < p.measure_until:
            return False

        # window complete -> reward
        try:
            outcome = outcome_from_samples(p.start, metrics)
        except ValueError as e:
            ev.append(self._abandon(t, f"unusable window: {e}"))
            return True
        bd = compute_reward(outcome, self.cfg.reward)
        if bd.reward is None:
            ev.append(self._abandon(t, f"invalid window: {bd.reason}"))
            return True
        self.learner.update(p.context, p.action, bd.reward)
        self.updates += 1
        self._save(dim)
        ev.append(self._emit("outcome", t, decision_id=p.decision_id, action=p.action,
                             strategy=p.strategy, forced=p.forced, reward=bd.reward,
                             breakdown=bd.to_dict(), learner_updated=True))
        self.pending = None
        self.next_decision_at = t
        if bd.override and p.action != safe_fallback((True,) * 5):
            self._force_safe = True  # severe window: next decision is the conservative fallback
        return True

    # ------------------------------------------------------------------ decide
    def _decide(self, env, t, state: StateSnapshot, metrics: MetricsSample, policy: PolicyInfo, ev) -> None:
        mask = policy.action_mask
        current = action_for_strategy(metrics.strategy)
        context = build_context(state, self.cfg.context_mode)
        if self.cfg.predictor is not None:  # forecast one decision interval ahead from the decision-time snapshots
            context = self.cfg.predictor.augment(context, t, self.cfg.settle_s + self.cfg.measure_s)
        forced = False
        try:
            if self._force_safe:
                action = safe_fallback(mask, None)
                if action is None:
                    raise NoAllowedAction("no allowed action")
                sel_scores, explored, forced = [None] * 5, False, True
                self._force_safe = False
            else:
                sel = self.learner.select(context, mask, self.rng)
                action, sel_scores, explored = sel.action, sel.scores, sel.explored
        except NoAllowedAction:
            ev.append(self._emit("hold", t, reason="no allowed action", active_strategy=metrics.strategy))
            return
        assert mask[action], "learner returned a masked action"  # invariant: never invalid

        self.counter += 1
        did = f"{self.cfg.run_id}-d{self.counter:06d}"
        base = dict(decision_id=did, action=action, strategy=ACTION_STRATEGIES[action],
                    active_strategy=metrics.strategy, active_action=current, scores=sel_scores,
                    explored=explored, forced=forced, learner=self.learner.name,
                    contextual=self.learner.contextual, context=context.tolist(),
                    policy_version=policy.version)
        if self.cfg.mode == "shadow":
            ev.append(self._emit("recommend", t, applied=False, **base))
            self.next_decision_at = t + self.cfg.settle_s + self.cfg.measure_s
            return

        switching = current != action
        if switching:
            try:
                env.apply_action(action, did)
            except ActionRejected as e:
                self.counter -= 1
                ev.append(self._emit("apply_rejected", t, decision_id=did, action=action, reason=str(e)))
                return
        ev.append(self._emit("decision", t, applied=True, switched=switching, **base))
        self.pending = _Pending(
            decision_id=did, action=action, strategy=ACTION_STRATEGIES[action], context=context,
            switch_expected=metrics.switch_count + (1 if switching else 0),
            settle_until=t + (self.cfg.settle_s if switching else 0.0), forced=forced)
        if not switching:  # policy unchanged: window already reflects it, start measuring now
            self.pending.start, self.pending.measure_until = metrics, t + self.cfg.measure_s
