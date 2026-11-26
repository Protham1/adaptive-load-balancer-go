"""Run the bandit controller against a live Go load balancer.

  python -m ai.cli --url http://localhost:8080                 # shadow mode (default): recommends only
  python -m ai.cli --mode live --allow-live ...                # applies actions; runs the promotion gate first

Shadow mode writes recommendations to the decision log and never calls POST /api/policy.
"""
from __future__ import annotations

import argparse
import sys
import time

from .bandit import make_learner
from .client import HTTPEnvironment
from .controller import BanditController, ControllerConfig
from .persistence import DecisionLog
from .state import context_dim


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://localhost:8080")
    ap.add_argument("--mode", choices=("shadow", "live"), default="shadow")
    ap.add_argument("--allow-live", action="store_true", help="required for --mode live")
    ap.add_argument("--skip-gate", action="store_true", help="do not run the promotion gate (not recommended)")
    ap.add_argument("--algo", default="linucb", choices=("linucb", "linear-eps-greedy", "mean-eps-greedy"))
    ap.add_argument("--context", default="compact", choices=("compact", "full"))
    ap.add_argument("--backends", type=int, default=3, help="number of configured backends")
    ap.add_argument("--alpha", type=float, default=0.3)
    ap.add_argument("--epsilon", type=float, default=0.1)
    ap.add_argument("--model", default="ai/model.json")
    ap.add_argument("--log", default="ai/decisions.jsonl")
    ap.add_argument("--poll", type=float, default=2.0, help="seconds between controller steps")
    ap.add_argument("--run-id", default=time.strftime("run-%Y%m%d-%H%M%S"))
    ap.add_argument("--seed", type=int, default=0)
    a = ap.parse_args(argv)

    if a.mode == "live":
        if not a.allow_live:
            print("refusing live mode without --allow-live (start in shadow mode first)", file=sys.stderr)
            return 2
        if not a.skip_gate:
            from .promotion_gate import run_gate
            rep = run_gate(run_unit_tests=True)
            print(rep.render())
            if not rep.passed:
                return 3

    dim = context_dim(a.context, a.backends)
    kw = {"alpha": a.alpha} if a.algo == "linucb" else {"epsilon": a.epsilon}
    learner = make_learner(a.algo, dim, **kw)
    cfg = ControllerConfig(mode=a.mode, context_mode=a.context, run_id=a.run_id, n_backends=a.backends,
                           model_path=a.model, seed=a.seed)
    ctl = BanditController(learner, cfg, DecisionLog(a.log))
    env = HTTPEnvironment(a.url)
    print(f"[{a.mode}] {learner.name} against {a.url}; log={a.log} model={a.model}")
    try:
        while True:
            for e in ctl.step(env):
                if e["event"] != "hold":
                    print(e["event"], {k: e.get(k) for k in ("decision_id", "action", "reward", "reason") if k in e})
            time.sleep(a.poll)
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
