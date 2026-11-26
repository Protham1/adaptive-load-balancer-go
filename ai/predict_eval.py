"""Phase 20 evaluation: does prediction improve the load-balancing OBJECTIVE?

Two separate questions, never conflated:

  A. FORECAST ACCURACY (chronological split). For each series the first 60 % of time is TRAIN, the last 40 %
     TEST. Predictor hyper-parameters are chosen on the train part only (walk-forward, no shuffling); test
     errors use the chosen parameters and only data observed before each forecast. Compared with the naive
     persistence forecast. Series: simulator workloads (10 s sampling, 30 s horizon) and, when present, real
     per-second offered-load traces from the Phase 14 runs (4 s horizon).

  B. CONTROL (the claim that matters). Reactive LinUCB vs LinUCB whose context is augmented with forecasts,
     on identical scenarios and seeds (paired). Predictor choice is made on DEV seeds; the TEST seeds are
     disjoint. Objective: mean reward per window (also p95 and error rate). Statistics: paired bootstrap CI,
     exact sign-flip test, Holm correction, and the claim rule used for the experiments (n >= 5, CI excludes
     zero, Holm p < 0.05).

    python -m ai.predict_eval            (writes ai/results/predictive.json and a plot)
    python -m ai.predict_eval --quick
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import statistics
import sys
from typing import Callable, Dict, List, Optional, Sequence, Tuple

import numpy as np

from .actions import STATIC_SWRR
from .bandit import FixedPolicy, LinUCB
from .experiments import run_episode, outcomes_of
from .predict import PredictiveContext, make_forecaster
from .schemas import parse_state
from .simulator import SCENARIOS, TREND_SCENARIOS, SimEnvironment
from .state import build_context, context_dim

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
from experiments.analysis import stats as S  # noqa: E402

CD = context_dim("compact", 3)
DEV_SEEDS = list(range(2000, 2005))
TEST_SEEDS = list(range(30))
ALPHA = 0.05  # LinUCB exploration, from the Phase 9 development seeds (same for reactive and predictive arms)

CANDIDATES: Dict[str, List[dict]] = {
    "ma": [{"window_s": w} for w in (60.0, 120.0, 240.0)],
    "ema": [{"alpha": a, "beta": b} for a in (0.3, 0.6) for b in (0.2, 0.5)],
    "linreg": [{"window_s": w} for w in (90.0, 150.0, 240.0)],
}


# ---------------------------------------------------------------------------- A. forecast accuracy
def walk_forward_errors(ts: Sequence[float], ys: Sequence[float], kind: str, params: dict, k: int,
                        lo: int, hi: int) -> List[float]:
    """Absolute errors of k-step-ahead forecasts for targets with index in [lo, hi). The forecaster is fed
    observations 0..i in order and asked for t_{i+k}; it never sees index > i (causal)."""
    f = make_forecaster(kind, **params)
    errs: List[float] = []
    for i in range(len(ys) - k):
        f.update(ts[i], ys[i])
        j = i + k
        if lo <= j < hi:
            p = f.forecast(ts[j] - ts[i])
            if p is not None:
                errs.append(abs(p - ys[j]))
    return errs


def choose_on_train(ts, ys, kind: str, k: int, n_train: int) -> Tuple[dict, float]:
    """Best hyper-parameters by MAE over targets inside the TRAIN part only."""
    best, best_mae = None, float("inf")
    for params in CANDIDATES[kind]:
        e = walk_forward_errors(ts, ys[:n_train], kind, params, k, 3, n_train)
        if e and np.mean(e) < best_mae:
            best, best_mae = params, float(np.mean(e))
    return best or CANDIDATES[kind][0], best_mae


def accuracy_for_series(ts: Sequence[float], ys: Sequence[float], k: int, split: float = 0.6) -> Dict[str, dict]:
    n = len(ys)
    n_train = int(n * split)
    out = {}
    f = make_forecaster("persistence")
    pers = []
    for i in range(n - k):
        f.update(ts[i], ys[i])
        if n_train <= i + k < n:
            pers.append(abs(f.forecast(0) - ys[i + k]))
    out["persistence"] = {"mae": float(np.mean(pers)) if pers else float("nan"), "n_test": len(pers), "params": {}}
    for kind in CANDIDATES:
        params, _ = choose_on_train(ts, ys, kind, k, n_train)
        # the test part is evaluated with the TRAIN-selected parameters; history before the split is legitimate input
        errs = walk_forward_errors(ts, ys, kind, params, k, n_train, n)
        out[kind] = {"mae": float(np.mean(errs)) if errs else float("nan"), "n_test": len(errs), "params": params}
    return out


def sim_series(scenario_name: str, seed: int, step_s: int = 10) -> Tuple[List[float], List[float]]:
    sc = {**SCENARIOS, **TREND_SCENARIOS}[scenario_name]
    env = SimEnvironment([sc], seed=seed)
    ts, ys = [], []
    for _ in range(int(sc.duration_s / step_s)):
        env.advance(step_s)
        ctx = build_context(parse_state(env.get_state()), "compact")
        ts.append(env.t)
        ys.append(float(ctx[0]))  # rps_normalized (log-scaled)
    return ts, ys


def real_series(results_dir: str, exps=("E6", "E8")) -> List[Tuple[str, List[float], List[float]]]:
    out = []
    for e in exps:
        for p in sorted(glob.glob(os.path.join(results_dir, e, f"{e}_swrr_r*", "loadgen.json")))[:3]:
            try:
                sec = json.load(open(p, encoding="utf-8"))["seconds"]
            except (OSError, ValueError, KeyError):
                continue
            ys = [float(s["offered"]) for s in sec if s["t"] < len(sec) - 2]
            out.append((f"{e}:{os.path.basename(os.path.dirname(p))}", [float(i) for i in range(len(ys))], ys))
    return out


def forecast_accuracy(seeds: Sequence[int], results_dir: Optional[str]) -> dict:
    rows: Dict[str, List[dict]] = {}
    for name in list(TREND_SCENARIOS) + ["traffic_spike", "balanced"]:
        for s in seeds:
            ts, ys = sim_series(name, s)
            rows.setdefault(f"sim:{name}", []).append(accuracy_for_series(ts, ys, k=3))   # 30 s horizon
    if results_dir:
        for tag, ts, ys in real_series(results_dir):
            rows.setdefault(f"real:{tag.split(':')[0]}", []).append(accuracy_for_series(ts, ys, k=4))  # 4 s horizon
    summary = {}
    for src, lst in rows.items():
        summary[src] = {kind: {"mae": float(np.nanmean([r[kind]["mae"] for r in lst])), "n_series": len(lst)}
                        for kind in lst[0]}
        base = summary[src]["persistence"]["mae"]
        for kind in summary[src]:
            m = summary[src][kind]["mae"]
            summary[src][kind]["skill_vs_persistence"] = float(1 - m / base) if base > 0 else float("nan")
    return summary


# ----------------------------------------------------------------------------------- B. control
def stream(kind: str, seed: int):
    rng = np.random.default_rng(50_000 + seed)
    if kind == "trend":
        names = list(TREND_SCENARIOS) * 3
    else:
        names = list(SCENARIOS) * 2
    rng.shuffle(names)
    pool = {**SCENARIOS, **TREND_SCENARIOS}
    return [pool[n] for n in names]


def episode_metrics(kind: str, seed: int, predictor: Optional[PredictiveContext]) -> Dict[str, float]:
    dim = CD + (len(predictor.feature_names()) if predictor else 0)
    learner = LinUCB(dim, alpha=ALPHA)
    if predictor:
        predictor.reset()
    env, recs = run_episode(learner, stream(kind, seed), seed, "compact", predictor=predictor)
    outs = outcomes_of(recs)
    r = [o["reward"] for o in outs]
    raw = [o["breakdown"]["raw"] for o in outs if o["breakdown"].get("raw")]
    return {"reward": float(np.mean(r)), "p95_ms": float(np.mean([x["p95_ms"] for x in raw])),
            "error_rate": float(np.mean([x["error_rate"] for x in raw])), "windows": len(r)}


def mean_over(kind: str, seeds: Sequence[int], make: Callable[[], Optional[PredictiveContext]]) -> float:
    return statistics.mean(episode_metrics(kind, s, make())["reward"] for s in seeds)


def control_comparison(kind: str, dev_seeds: Sequence[int], test_seeds: Sequence[int]) -> dict:
    # 1. predictor hyper-parameters chosen on DEV seeds only (best config per family)
    chosen, dev_scores = {}, {}
    for fam, grid in CANDIDATES.items():
        scores = [(mean_over(kind, dev_seeds, lambda p=p: PredictiveContext(fam, **p)), p) for p in grid]
        best = max(scores, key=lambda x: x[0])
        chosen[fam], dev_scores[fam] = best[1], {"best_dev_reward": best[0], "grid_size": len(grid)}
    dev_reactive = mean_over(kind, dev_seeds, lambda: None)
    # 2. frozen configs on the disjoint TEST seeds, paired by seed
    arms: Dict[str, Optional[dict]] = {"reactive": None, **{f"predictive-{f}": chosen[f] for f in CANDIDATES}}
    per_arm: Dict[str, Dict[int, dict]] = {}
    for arm, params in arms.items():
        per_arm[arm] = {}
        for s in test_seeds:
            pc = PredictiveContext(arm.split("-")[1], **params) if params is not None else None
            per_arm[arm][s] = episode_metrics(kind, s, pc)
    out = {"stream": kind, "dev_seeds": list(dev_seeds), "test_seeds": list(test_seeds), "chosen_on_dev": chosen,
           "dev": dev_scores, "dev_reactive_reward": dev_reactive, "arms": {}, "comparisons": []}
    for arm, per in per_arm.items():
        out["arms"][arm] = {m: S.summarize([per[s][m] for s in test_seeds]) for m in ("reward", "p95_ms", "error_rate")}
    for metric, lower in (("reward", False), ("p95_ms", True), ("error_rate", True)):
        rows = []
        for arm in per_arm:
            if arm == "reactive":
                continue
            c = S.compare({s: per_arm[arm][s][metric] for s in test_seeds}, {s: per_arm["reactive"][s][metric] for s in test_seeds})
            rows.append({"metric": metric, "candidate": arm, "reference": "reactive", "lower_is_better": lower, **c})
        for r, a in zip(rows, S.holm([r["p"] for r in rows])):
            r["p_holm"] = a
            r["verdict"] = S.verdict(r["n_pairs"], r["ci_lo"], r["ci_hi"], a, lower)
        out["comparisons"] += rows
    return out


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--quick", action="store_true")
    ap.add_argument("--results", default=os.path.join(ROOT, "experiments", "results"))
    ap.add_argument("--out", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "results"))
    a = ap.parse_args(argv)
    os.makedirs(a.out, exist_ok=True)
    dev = DEV_SEEDS[:2] if a.quick else DEV_SEEDS
    test = TEST_SEEDS[:6] if a.quick else TEST_SEEDS
    doc = {"alpha": ALPHA, "candidates": CANDIDATES}
    print("A. forecast accuracy (chronological 60/40 split) ...", flush=True)
    doc["forecast_accuracy"] = forecast_accuracy(dev + test[:10], a.results if os.path.isdir(a.results) else None)
    for src, d in doc["forecast_accuracy"].items():
        print(f"  {src:14s} " + "  ".join(f"{k}: MAE {v['mae']:.4f} (skill {v['skill_vs_persistence']:+.2f})" for k, v in d.items()), flush=True)
    doc["control"] = {}
    for kind in ("trend", "standard"):
        print(f"B. control comparison on the '{kind}' stream ...", flush=True)
        doc["control"][kind] = control_comparison(kind, dev, test)
        c = doc["control"][kind]
        print("  chosen on dev:", c["chosen_on_dev"])
        for arm, m in c["arms"].items():
            print(f"  {arm:20s} reward {m['reward']['mean']:+.4f} +/- {m['reward']['sd']:.4f}   p95 {m['p95_ms']['mean']:.0f} ms   errors {m['error_rate']['mean']:.4f}", flush=True)
        for r in c["comparisons"]:
            if r["metric"] == "reward":
                print(f"  {r['candidate']:20s} vs reactive: dReward {r['mean_diff']:+.4f} [{r['ci_lo']:+.4f}, {r['ci_hi']:+.4f}] p_holm={r['p_holm']:.4f} -> {r['verdict']}", flush=True)
    with open(os.path.join(a.out, "predictive.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
