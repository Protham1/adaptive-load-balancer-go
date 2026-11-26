"""Phase 9 - seeded offline experiments.

Protocol (no tuning on the test data):
  * DEV seeds are used only to pick learner hyper-parameters.
  * Hyper-parameters are then frozen and every policy is evaluated on disjoint TEST seeds.
  * All numbers come from the simulator model (see simulator.py): they validate the
    learning logic, not real-system performance.

Run:  python -m ai.experiments            (writes ai/results/*.json, *.png)
      python -m ai.experiments --quick    (fewer seeds, for a smoke run)
"""
from __future__ import annotations

import argparse
import json
import os
import statistics
from typing import Callable, Dict, List, Sequence

import numpy as np

from .actions import LC, RR, RULE_ADAPTIVE, STATIC_SWRR
from .bandit import (FixedPolicy, LinearEpsilonGreedy, LinUCB, MeanEpsilonGreedy,
                     RandomPolicy, Learner)
from .controller import BanditController, ControllerConfig
from .persistence import DecisionLog
from .reward import RewardConfig, WindowOutcome, outcome_from_samples, sensitivity_analysis
from .schemas import parse_metrics
from .simulator import BASE_EPOCH, SCENARIOS, Scenario, SimEnvironment, run_controller
from .state import context_dim

DEV_SEEDS = list(range(1000, 1005))
TEST_SEEDS = list(range(20))
STREAM_REPEATS = 2  # each scenario appears this many times, in seeded random order
LearnerFactory = Callable[[], Learner]


def stream_scenarios(seed: int) -> List[Scenario]:
    rng = np.random.default_rng(10_000 + seed)
    names = list(SCENARIOS) * STREAM_REPEATS
    rng.shuffle(names)
    return [SCENARIOS[n] for n in names]


def run_episode(learner: Learner, scenarios: Sequence[Scenario], seed: int, context_mode: str = "compact",
                reward: RewardConfig = RewardConfig(), cycles: int = 1, poll_s: float = 2.0, predictor=None):
    env = SimEnvironment(scenarios, seed=seed)
    cfg = ControllerConfig(mode="live", context_mode=context_mode, reward=reward,
                           run_id=f"s{seed}", seed=seed, predictor=predictor)
    ctl = BanditController(learner, cfg, DecisionLog())
    records = run_controller(env, ctl, env.total_duration * cycles, poll_s)
    return env, records


def outcomes_of(records: List[dict]) -> List[dict]:
    return [r for r in records if r["event"] == "outcome"]


def scenario_of(env: SimEnvironment, rec: dict, measure_s: float = 20.0) -> str:
    return env.scenario_at(rec["t"] - BASE_EPOCH - measure_s / 2)[0].name


# ------------------------------------------------------------------------ learners
def learner_factories(alpha=0.3, eps=0.1) -> Dict[str, tuple]:
    """name -> (factory, context_mode). dim is derived from the context mode (3 backends)."""
    cd, fd = context_dim("compact", 3), context_dim("full", 3)
    return {
        "random": (lambda: RandomPolicy(), "compact"),
        "always-RR": (lambda: FixedPolicy(RR), "compact"),
        "always-LC": (lambda: FixedPolicy(LC), "compact"),
        "always-StaticSWRR": (lambda: FixedPolicy(STATIC_SWRR), "compact"),
        "always-RuleAdaptive": (lambda: FixedPolicy(RULE_ADAPTIVE), "compact"),
        "mean-eps-greedy (non-contextual)": (lambda: MeanEpsilonGreedy(epsilon=eps), "compact"),
        "linear-eps-greedy (compact ctx)": (lambda: LinearEpsilonGreedy(cd, epsilon=eps), "compact"),
        "linucb (compact ctx)": (lambda: LinUCB(cd, alpha=alpha), "compact"),
        "linucb (full ctx)": (lambda: LinUCB(fd, alpha=alpha), "full"),
    }


def mean_reward(records: List[dict]) -> float:
    r = [o["reward"] for o in outcomes_of(records)]
    return sum(r) / len(r) if r else float("nan")


# ------------------------------------------------------------------- dev: tuning
def tune_on_dev(seeds=DEV_SEEDS) -> dict:
    results = {"linucb_alpha": {}, "linear_eps": {}, "mean_eps": {}}
    cd = context_dim("compact", 3)
    for a in (0.05, 0.1, 0.3, 1.0):
        results["linucb_alpha"][a] = statistics.mean(
            mean_reward(run_episode(LinUCB(cd, alpha=a), stream_scenarios(s), s)[1]) for s in seeds)
    for e in (0.05, 0.1, 0.2):
        results["linear_eps"][e] = statistics.mean(
            mean_reward(run_episode(LinearEpsilonGreedy(cd, epsilon=e), stream_scenarios(s), s)[1]) for s in seeds)
        results["mean_eps"][e] = statistics.mean(
            mean_reward(run_episode(MeanEpsilonGreedy(epsilon=e), stream_scenarios(s), s)[1]) for s in seeds)
    best = {
        "alpha": max(results["linucb_alpha"], key=results["linucb_alpha"].get),
        "eps": max(results["linear_eps"], key=results["linear_eps"].get),
    }
    return {"scores": {k: {str(x): v for x, v in d.items()} for k, d in results.items()}, "chosen": best}


# ------------------------------------------------------------------ test: stream
def run_stream(seeds=TEST_SEEDS, alpha=0.3, eps=0.1) -> dict:
    out = {}
    for name, (factory, ctx) in learner_factories(alpha, eps).items():
        per_seed, curves, actions, per_scn, aband = [], [], np.zeros(5), {}, []
        for s in seeds:
            env, recs = run_episode(factory(), stream_scenarios(s), s, ctx)
            outs = outcomes_of(recs)
            per_seed.append(mean_reward(recs))
            curves.append(np.cumsum([o["reward"] for o in outs]))
            for r in recs:
                if r["event"] == "decision":
                    actions[r["action"]] += 1
            for o in outs:
                per_scn.setdefault(scenario_of(env, o), []).append(o["reward"])
            n_ab = sum(r["event"] == "abandon" for r in recs)
            aband.append(n_ab / max(1, n_ab + len(outs)))
        n = min(len(c) for c in curves)
        out[name] = {
            "mean_reward": statistics.mean(per_seed),
            "std_over_seeds": statistics.pstdev(per_seed),
            "per_seed": per_seed,
            "action_share": (actions / max(actions.sum(), 1)).tolist(),
            "per_scenario_mean": {k: statistics.mean(v) for k, v in sorted(per_scn.items())},
            "abandoned_window_fraction": statistics.mean(aband),
            "cumulative_curve_mean": np.mean([c[:n] for c in curves], axis=0).tolist(),
        }
    return out


# ----------------------------------------- fixed policies per scenario + sensitivity
def run_fixed_per_scenario(seeds=TEST_SEEDS, reward: RewardConfig = RewardConfig()) -> dict:
    """Mean reward of each fixed action in each scenario (the 'oracle' reference table)."""
    table: dict = {}
    for scn_name, scn in SCENARIOS.items():
        for action, label in ((RR, "RR"), (LC, "LC"), (STATIC_SWRR, "StaticSWRR"), (RULE_ADAPTIVE, "RuleAdaptive")):
            vals = []
            for s in seeds:
                env = SimEnvironment([scn], seed=s)
                ctl = BanditController(FixedPolicy(action), ControllerConfig(
                    mode="live", reward=reward, run_id=f"f{s}", seed=s), DecisionLog())
                recs = run_controller(env, ctl, env.total_duration * 2)
                vals += [o["reward"] for o in outcomes_of(recs)]
            table.setdefault(scn_name, {})[label] = statistics.mean(vals)
    return table


def collect_outcomes_direct(seeds=TEST_SEEDS) -> Dict[str, Dict[int, List[WindowOutcome]]]:
    """Capture true WindowOutcomes (with per-backend counters) for every fixed action and scenario."""
    out: Dict[str, Dict[int, List[WindowOutcome]]] = {}
    for scn_name, scn in SCENARIOS.items():
        for action in (RR, LC, STATIC_SWRR, RULE_ADAPTIVE):
            for s in seeds:
                env = SimEnvironment([scn], seed=s)
                ctl = BanditController(FixedPolicy(action), ControllerConfig(
                    mode="live", run_id=f"f{s}", seed=s), DecisionLog())
                # sample metrics at window edges ourselves (same cadence the controller uses)
                snaps = []
                for _ in range(int(env.total_duration * 2 / 2.0)):
                    env.advance(2.0)
                    ctl.step(env)
                    snaps.append(parse_metrics(env.get_metrics()))
                for rec in outcomes_of(ctl.log.records):
                    t_end = rec["t"]
                    t_start = t_end - rec["breakdown"]["raw"]["duration_s"]
                    s0 = min(snaps, key=lambda m: abs(m.t - t_start))
                    s1 = min(snaps, key=lambda m: abs(m.t - t_end))
                    out.setdefault(scn_name, {}).setdefault(action, []).append(outcome_from_samples(s0, s1))
    return out


def run_sensitivity(outcomes: Dict[str, Dict[int, List[WindowOutcome]]], rel=0.25) -> dict:
    res = {}
    pooled: Dict[int, List[WindowOutcome]] = {}
    for scn, by_action in outcomes.items():
        res[scn] = sensitivity_analysis(by_action, RewardConfig(), rel=rel)
        for a, outs in by_action.items():
            pooled.setdefault(a, []).extend(outs)
    res["POOLED"] = sensitivity_analysis(pooled, RewardConfig(), rel=rel)
    return res


# ------------------------------------------------------------------------- plots
def make_plots(stream: dict, outdir: str) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    fig, ax = plt.subplots(figsize=(9, 5))
    for name, r in stream.items():
        ax.plot(r["cumulative_curve_mean"], label=name, lw=2 if "linucb" in name else 1.2)
    ax.set_xlabel("decision window (30 s each)")
    ax.set_ylabel("cumulative reward (mean over test seeds)")
    ax.set_title("Cumulative reward - simulator, scenario stream")
    ax.legend(fontsize=7)
    fig.tight_layout()
    fig.savefig(os.path.join(outdir, "cumulative_reward.png"), dpi=130)
    plt.close(fig)

    names = list(stream)
    labels = ["RR", "LC", "StaticSWRR", "CH", "RuleAdaptive"]
    fig, ax = plt.subplots(figsize=(9, 5))
    bottom = np.zeros(len(names))
    for i, lab in enumerate(labels):
        vals = np.array([stream[n]["action_share"][i] for n in names])
        ax.bar(range(len(names)), vals, bottom=bottom, label=lab)
        bottom += vals
    ax.set_xticks(range(len(names)))
    ax.set_xticklabels(names, rotation=35, ha="right", fontsize=7)
    ax.set_ylabel("share of decisions")
    ax.set_title("Action distribution - simulator, scenario stream")
    ax.legend(fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(outdir, "action_distribution.png"), dpi=130)
    plt.close(fig)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--quick", action="store_true")
    ap.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "results"))
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    dev = DEV_SEEDS[:2] if a.quick else DEV_SEEDS
    test = TEST_SEEDS[:4] if a.quick else TEST_SEEDS

    print("tuning on DEV seeds ...")
    tuning = tune_on_dev(dev)
    chosen = tuning["chosen"]
    print("chosen (frozen):", chosen)
    print("stream evaluation on TEST seeds ...")
    stream = run_stream(test, alpha=chosen["alpha"], eps=chosen["eps"])
    print("fixed policies per scenario ...")
    table = run_fixed_per_scenario(test[:5])
    print("reward-weight sensitivity ...")
    outcomes = collect_outcomes_direct(test[:5])
    sens = run_sensitivity(outcomes)

    doc = {"reward_config": RewardConfig().weights(), "dev_seeds": dev, "test_seeds": test,
           "tuning": tuning, "stream": stream, "fixed_per_scenario": table, "sensitivity": sens}
    with open(os.path.join(a.out, "experiments.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
    make_plots(stream, a.out)

    print("\n== Stream (mean reward per window, mean +/- std over test seeds) ==")
    for n, r in sorted(stream.items(), key=lambda kv: -kv[1]["mean_reward"]):
        print(f"{n:38s} {r['mean_reward']:+.4f} +/- {r['std_over_seeds']:.4f}   abandoned={r['abandoned_window_fraction']:.2f}")
    print("\n== Fixed action mean reward per scenario ==")
    for scn, row in table.items():
        print(f"{scn:22s} " + "  ".join(f"{k}={v:+.3f}" for k, v in row.items()))
    print("\n== Sensitivity (+/-25% weights) ==")
    for scn, r in sens.items():
        print(f"{scn:22s} best_unchanged={r.get('best_action_unchanged_fraction', float('nan')):.2f} "
              f"pair_agreement={r.get('pairwise_order_agreement', float('nan')):.2f} stable={r.get('stable')}")


if __name__ == "__main__":
    main()
