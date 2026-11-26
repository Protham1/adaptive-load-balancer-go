"""Experiment matrix scheduler (Phases 15-16).

    python -m experiments.runner.matrix --experiments E1,E4 --reps 3 --out experiments/results
    python -m experiments.runner.matrix --experiments all --reps 5            # plan's minimum exploratory target
    python -m experiments.runner.matrix --experiments E4 --dry-run            # print the randomized order only

Fair-comparison design
  * every policy sees the same scenario, hardware, health checks, warm-up, request mix, offered load and
    duration; repetition r uses seed base+r for EVERY policy, so the arrival schedule is identical across
    policies (paired design);
  * run order is randomized in ROUNDS: round r contains each (experiment, arm) once in a seeded random
    order, so every condition is spread over the same time period (reduces drift/thermal bias);
  * resumable: runs with done.json are skipped.
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
import time
from dataclasses import dataclass
from typing import List, Optional

from .run import load_matrix, run_one
from .stack import ROOT, build_binaries


@dataclass(frozen=True)
class RunSpec:
    exp: str
    arm: str
    rep: int
    interval_s: Optional[float] = None

    @property
    def run_id(self) -> str:
        return f"{self.exp}_{self.arm}_r{self.rep}" + (f"_i{int(self.interval_s)}" if self.interval_s else "")


def conditions(matrix: dict, exp_key: str, policies: Optional[List[str]] = None) -> List[tuple]:
    """(arm, interval) pairs of an experiment."""
    e = matrix["experiments"][exp_key]
    if exp_key == "E10":
        arms = e["arms"]
        return [(a, None) for a in arms if not policies or a in policies]
    if exp_key == "E9":
        return [(p, i) if p == "ai" else (p, None) for p in e["policies"] if not policies or p in policies
                for i in (e["intervals_s"] if p == "ai" else [None])]
    return [(p, None) for p in e["policies"] if not policies or p in policies]


def build_plan(matrix: dict, experiments: List[str], reps: int, policies: Optional[List[str]] = None,
               seed: int = 0) -> List[RunSpec]:
    plan: List[RunSpec] = []
    for r in range(reps):
        block = [RunSpec(x, arm, r, iv) for x in experiments for arm, iv in conditions(matrix, x, policies)]
        random.Random(f"{seed}:{r}").shuffle(block)
        plan.extend(block)
    return plan


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--experiments", default="all", help="comma list (E1..E10,A1) or 'all'")
    ap.add_argument("--policies", default="", help="restrict to these policies/arms (comma list)")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--seed", type=int, default=0, help="seed of the run-order randomization")
    ap.add_argument("--out", default=os.path.join(ROOT, "experiments", "results"))
    ap.add_argument("--sample-rate", type=float, default=0.01)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--force", action="store_true", help="re-run completed runs")
    ap.add_argument("--no-build", action="store_true")
    a = ap.parse_args(argv)

    matrix = load_matrix()
    exps = list(matrix["experiments"]) if a.experiments == "all" else a.experiments.split(",")
    for x in exps:
        if x not in matrix["experiments"]:
            print(f"unknown experiment {x}", file=sys.stderr)
            return 2
    plan = build_plan(matrix, exps, a.reps, [p for p in a.policies.split(",") if p] or None, a.seed)
    est = sum(matrix["experiments"][s.exp]["duration_s"] + 6 for s in plan) / 60.0
    print(f"{len(plan)} runs, ~{est:.0f} min", flush=True)
    if a.dry_run:
        for i, s in enumerate(plan, 1):
            print(f"{i:4d} {s.run_id}")
        return 0
    bin_dir = os.path.join(a.out, "_bin")
    if not a.no_build:
        build_binaries(bin_dir)
    failed = 0
    t_start = time.time()
    for i, s in enumerate(plan, 1):
        t0 = time.time()
        d = run_one(matrix, s.exp, s.arm, s.rep, a.out, bin_dir, s.interval_s, a.sample_rate, a.force)
        ok = os.path.exists(os.path.join(d, "done.json"))
        failed += not ok
        print(f"[{i}/{len(plan)}] {s.run_id}: {'ok' if ok else 'FAILED'} ({time.time() - t0:.0f}s, elapsed {(time.time() - t_start) / 60:.1f} min)",
              flush=True)
    print(f"finished: {len(plan) - failed} ok, {failed} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
