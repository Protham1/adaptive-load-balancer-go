"""Rebuild a learner from the decision history (Phase 19: restarts are reproducible).

The learner log written by ai.server contains, per decision, the exact context and chosen action
(`decision` events) and, per evaluated window, the reward (`outcome` events keyed by the decision id).
Replaying those pairs in order through a fresh learner reproduces the saved model exactly, because the
update is deterministic (ridge/Sherman-Morrison); `verify_against_checkpoint` checks that.

    python -m ai.replay --log ai/decisions.jsonl --model ai/model.json --algo linucb --dim 15
"""
from __future__ import annotations

import argparse
import json
import sys
from typing import Dict, Tuple

import numpy as np

from .bandit import Learner, make_learner


def rebuild_from_log(path: str, learner: Learner) -> int:
    """Replay (context, action, reward) triples in log order. Returns the number of updates applied."""
    pending: Dict[str, Tuple[np.ndarray, int]] = {}
    n = 0
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            e = json.loads(line)
            ev = e.get("event")
            if ev == "decision":
                pending[e["decision_id"]] = (np.asarray(e["context"], dtype=float), int(e["action"]))
            elif ev == "outcome" and e.get("target") in pending:
                ctx, action = pending.pop(e["target"])
                learner.update(ctx, action, float(e["reward"]))
                n += 1
            elif ev == "reset":  # a reset command in the log starts a new model lineage
                learner.load_state_dict(make_like(learner).state_dict())
                pending.clear()
                n = 0
    return n


def make_like(learner: Learner) -> Learner:
    cfg = learner.config()
    kind = learner.name
    if kind == "linucb":
        return make_learner("linucb", cfg["dim"], alpha=cfg["alpha"], ridge=cfg["ridge"])
    if kind == "linear-eps-greedy":
        return make_learner("linear-eps-greedy", cfg["dim"], epsilon=cfg["epsilon"], ridge=cfg["ridge"],
                            min_pulls=cfg["min_pulls"])
    if kind == "mean-eps-greedy":
        return make_learner("mean-eps-greedy", 0, epsilon=cfg["epsilon"], min_pulls=cfg["min_pulls"])
    raise ValueError(kind)


def states_equal(a: dict, b: dict, atol: float = 1e-9) -> bool:
    if a.keys() != b.keys():
        return False
    for k in a:
        if not np.allclose(np.asarray(a[k], dtype=float), np.asarray(b[k], dtype=float), atol=atol):
            return False
    return True


def verify_against_checkpoint(log_path: str, model_path: str, learner: Learner) -> dict:
    """Replay the log into `learner` and compare with the checkpoint's state."""
    with open(model_path, "r", encoding="utf-8") as f:
        saved = json.load(f)["state"]
    n = rebuild_from_log(log_path, learner)
    return {"updates_replayed": n, "identical": states_equal(learner.state_dict(), saved)}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--log", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--algo", default="linucb")
    ap.add_argument("--dim", type=int, default=15)
    ap.add_argument("--alpha", type=float, default=0.05)
    a = ap.parse_args(argv)
    kw = {"alpha": a.alpha} if a.algo == "linucb" else {}
    r = verify_against_checkpoint(a.log, a.model, make_learner(a.algo, a.dim, **kw))
    print(json.dumps(r))
    return 0 if r["identical"] else 1


if __name__ == "__main__":
    sys.exit(main())
