"""Phase 9 promotion gate: the learner may only touch live traffic after every check passes.

Checks
  unit_tests              the pytest suite (skipped when called from inside the suite)
  deterministic_replay    same seed twice -> identical decisions and rewards
  invalid_stale_state     stale / NaN / malformed / restricted-mask states never lead to an
                          applied action or a learner update; masked actions are never returned
  simulated_failure       backend-failure scenario: no invalid action, safety override handled,
                          reward not worse than the random policy (paired seeds)
  persistence_restart     save -> new process state -> load reproduces the model exactly

Run:  python -m ai.promotion_gate
"""
from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from typing import List, Tuple

import numpy as np

from .actions import NUM_ACTIONS
from .bandit import LinUCB, RandomPolicy
from .controller import BanditController, ControllerConfig
from .persistence import DecisionLog
from .simulator import SCENARIOS, SimEnvironment, run_controller
from .state import context_dim

CD = context_dim("compact", 3)


@dataclass
class GateReport:
    checks: List[Tuple[str, bool, str]] = field(default_factory=list)

    def add(self, name: str, ok: bool, detail: str = "") -> None:
        self.checks.append((name, bool(ok), detail))

    @property
    def passed(self) -> bool:
        return bool(self.checks) and all(ok for _, ok, _ in self.checks)

    def render(self) -> str:
        lines = [f"[{'PASS' if ok else 'FAIL'}] {n}" + (f" - {d}" if d else "") for n, ok, d in self.checks]
        lines.append("PROMOTION GATE: " + ("PASSED" if self.passed else "FAILED"))
        return "\n".join(lines)


def _controller(env, learner, seed=0, mode="live", model_path=None):
    cfg = ControllerConfig(mode=mode, run_id=f"g{seed}", seed=seed, model_path=model_path)
    return BanditController(learner, cfg, DecisionLog())


def _mean_reward(records):
    r = [x["reward"] for x in records if x["event"] == "outcome"]
    return sum(r) / len(r) if r else float("nan")


def check_deterministic_replay(seed=7) -> Tuple[bool, str]:
    runs = []
    for _ in range(2):
        env = SimEnvironment([SCENARIOS["latency_degradation"], SCENARIOS["balanced"]], seed=seed)
        recs = run_controller(env, _controller(env, LinUCB(CD), seed), 900)
        runs.append([(r["event"], r.get("action"), round(r.get("reward", 0.0), 12)) for r in recs])
    return runs[0] == runs[1] and len(runs[0]) > 5, f"{len(runs[0])} events compared"


def check_invalid_stale(seed=3) -> Tuple[bool, str]:
    problems = []
    for fault in ("inject_stale", "inject_nan", "inject_bad_schema"):
        env = SimEnvironment([SCENARIOS["balanced"]], seed=seed)
        env.advance(30)
        setattr(env, fault, True)
        ctl = _controller(env, LinUCB(CD), seed)
        run_controller(env, ctl, 120)
        if env.applied:
            problems.append(f"{fault}: applied {len(env.applied)} action(s)")
        if ctl.updates:
            problems.append(f"{fault}: learner updated {ctl.updates}x")
        if not any(r["event"] == "hold" for r in ctl.log.records):
            problems.append(f"{fault}: no hold recorded")
    # restricted masks: only the allowed actions may ever be applied
    rng = np.random.default_rng(seed)
    for _ in range(5):
        mask = [bool(x) for x in rng.integers(0, 2, NUM_ACTIONS)]
        mask[int(rng.integers(NUM_ACTIONS))] = True
        env = SimEnvironment([SCENARIOS["traffic_spike"]], seed=seed)
        env.mask_override = mask
        env.advance(30)
        ctl = _controller(env, LinUCB(CD, alpha=1.0), seed)
        run_controller(env, ctl, 600)
        bad = [a for _, a, _ in env.applied if not mask[a]]
        if bad:
            problems.append(f"mask {mask}: invalid action(s) applied {bad}")
    return not problems, "; ".join(problems) or "stale/NaN/schema faults held; masks respected"


def check_simulated_failure(seeds=(0, 1, 2, 3, 4)) -> Tuple[bool, str]:
    learned, rand, overrides = [], [], 0
    for s in seeds:
        for learner, sink in ((LinUCB(CD), learned), (RandomPolicy(), rand)):
            env = SimEnvironment([SCENARIOS["backend_failure"]], seed=s)
            recs = run_controller(env, _controller(env, learner, s), 1500)
            sink.append(_mean_reward(recs))
            if isinstance(learner, LinUCB):
                overrides += sum(1 for r in recs if r["event"] == "outcome" and r["breakdown"]["override"])
    lm, rm = float(np.mean(learned)), float(np.mean(rand))
    return lm >= rm, f"learner {lm:+.4f} vs random {rm:+.4f}; override windows={overrides}"


def check_persistence_restart(seed=5) -> Tuple[bool, str]:
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "model.json")
        env = SimEnvironment([SCENARIOS["cpu_imbalance"], SCENARIOS["balanced"]], seed=seed)
        a = LinUCB(CD)
        ctl = _controller(env, a, seed, model_path=path)
        run_controller(env, ctl, 900)
        if ctl.updates == 0:
            return False, "no learning happened"
        b = LinUCB(CD)
        env2 = SimEnvironment([SCENARIOS["balanced"]], seed=seed)
        ctl2 = _controller(env2, b, seed, model_path=path)
        run_controller(env2, ctl2, 4)  # first valid state triggers the load
        same = np.allclose(a.model.A_inv, b.model.A_inv) and np.allclose(a.model.b, b.model.b)
        return same and ctl2.counter >= ctl.counter, f"{ctl.updates} updates restored"


def run_gate(run_unit_tests: bool = True) -> GateReport:
    rep = GateReport()
    if run_unit_tests:
        here = os.path.dirname(os.path.abspath(__file__))
        p = subprocess.run([sys.executable, "-m", "pytest", "-q", os.path.join(here, "tests")],
                           capture_output=True, text=True, cwd=os.path.dirname(here))
        rep.add("unit_tests", p.returncode == 0, (p.stdout.strip().splitlines() or [""])[-1])
    for name, fn in (("deterministic_replay", check_deterministic_replay),
                     ("invalid_stale_state", check_invalid_stale),
                     ("simulated_failure", check_simulated_failure),
                     ("persistence_restart", check_persistence_restart)):
        try:
            ok, detail = fn()
        except Exception as e:  # a crashing check is a failed check
            ok, detail = False, f"exception: {e!r}"
        rep.add(name, ok, detail)
    return rep


if __name__ == "__main__":
    report = run_gate(run_unit_tests="--skip-tests" not in sys.argv)
    print(report.render())
    sys.exit(0 if report.passed else 1)
