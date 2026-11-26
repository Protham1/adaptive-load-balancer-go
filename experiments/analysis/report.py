"""Aggregate runs, compute statistics, and write the report (Phase 17).

    python -m experiments.analysis.report --results experiments/results

Outputs (in <results>/analysis/): run_metrics.csv, summary.csv, comparisons.csv, report.md, plots/*.png
"""
from __future__ import annotations

import argparse
import csv
import glob
import json
import math
import os
from collections import defaultdict
from datetime import datetime, timezone
from typing import Dict, List, Optional, Sequence, Tuple

from . import stats
from .metrics import compute_run_metrics

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
NAN = float("nan")

# (metric key, label, lower_is_better, digits)
CORE = [("p95_ms", "p95 latency (ms)", True, 1), ("p99_ms", "p99 latency (ms)", True, 1),
        ("error_rate", "error rate", True, 4), ("ok_rps", "OK throughput (rps)", False, 1),
        ("imbalance_cv", "imbalance CV", True, 3)]
EXTRA = {
    "E3": [("overload_s", "overload (s)", True, 1), ("adaptation_s", "adaptation (s)", True, 1)],
    "E4": [("overload_s", "overload (s)", True, 1), ("adaptation_s", "adaptation (s)", True, 1)],
    "E6": [("overload_s", "overload (s)", True, 1), ("adaptation_s", "adaptation (s)", True, 1)],
    "E7": [("detect_s", "detection (s)", True, 2), ("reentry_alive_s", "re-entry: marked up (s)", True, 2),
           ("reentry_share_s", "re-entry: share restored (s)", True, 2), ("overload_s", "overload (s)", True, 1)],
    "E8": [("overload_s", "overload (s)", True, 1), ("adaptation_s", "adaptation (s)", True, 1)],
    "E9": [("overload_s", "overload (s)", True, 1), ("switches_per_min", "switches/min", True, 2)],
    "E10": [("overload_s", "overload (s)", True, 1), ("switches_per_min", "switches/min", True, 2)],
    "A1": [("stickiness", "stickiness (1 = full affinity)", False, 3)],
}
POLICY_ORDER = ["rr", "lc", "swrr", "ch", "rule", "ai"]
INTERFERENCE_CPU_PCT = 90.0  # system-wide CPU above this during a run marks it as possibly disturbed


def cond(m: dict) -> str:
    return f"{m['arm']}@{int(m['interval_s'])}s" if m.get("interval_s") else m["arm"]


def _order_key(label: str):
    base = label.split("@")[0]
    return (POLICY_ORDER.index(base) if base in POLICY_ORDER else 99, label)


def fmt(s: dict, d: int) -> str:
    if not s or s["n"] == 0 or math.isnan(s["mean"]):
        return "–"
    return f"{s['mean']:.{d}f} ± {s['sd']:.{d}f} [{s['ci_lo']:.{d}f}, {s['ci_hi']:.{d}f}]"


def load_matrix(results: str) -> dict:
    with open(os.path.join(ROOT, "experiments", "scenarios", "experiments.json"), encoding="utf-8") as f:
        return json.load(f)


def collect(results: str, matrix: dict) -> Tuple[List[dict], List[str]]:
    runs, failed = [], []
    for d in sorted(glob.glob(os.path.join(results, "*", "*"))):
        if not os.path.isdir(d) or os.path.basename(d).startswith("_"):
            continue
        if os.path.exists(os.path.join(d, "failed.json")):
            failed.append(os.path.basename(d))
            continue
        m = compute_run_metrics(d, matrix)
        if m:
            runs.append(m)
    return runs, failed


def metric_by_rep(runs: Sequence[dict], key: str) -> Dict[int, float]:
    return {r["rep"]: (r.get(key) if r.get(key) is not None else NAN) for r in runs}


def write_csv(path: str, rows: Sequence[dict]) -> None:
    if not rows:
        return
    keys: List[str] = []
    for r in rows:
        for k in r:
            if k not in keys:
                keys.append(k)
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=keys)
        w.writeheader()
        w.writerows(rows)


def references(exp_key: str, labels: Sequence[str]) -> List[Tuple[str, str]]:
    """(candidate, reference) pairs. Baselines are compared with the static and LC references; the
    learner/rule policies are the candidates."""
    pairs = []
    if exp_key == "E10":
        for l in labels:
            ref = "ai_full" if l.startswith("ai_") and l != "ai_full" else ("rule_full" if l.startswith("rule_") and l != "rule_full" else None)
            if ref and ref in labels:
                pairs.append((l, ref))
        return pairs
    if exp_key == "E9":
        return [(l, "swrr") for l in labels if l.startswith("ai@") and "swrr" in labels]
    for ref in ("swrr", "lc"):
        if ref in labels:
            pairs += [(l, ref) for l in labels if l != ref and l in ("ai", "rule", "rr", "lc", "ch") and not (ref == "lc" and l == "swrr")]
    return pairs


def build(results: str, out: Optional[str] = None) -> dict:
    matrix = load_matrix(results)
    out = out or os.path.join(results, "analysis")
    os.makedirs(os.path.join(out, "plots"), exist_ok=True)
    runs, failed = collect(results, matrix)
    valid = [r for r in runs if r["valid"]]
    excluded = [r["run_id"] for r in runs if not r["valid"]]
    disturbed = [r["run_id"] for r in valid if (r.get("system_cpu_pct_max") or 0) > INTERFERENCE_CPU_PCT]
    write_csv(os.path.join(out, "run_metrics.csv"), runs)

    by_exp: Dict[str, Dict[str, List[dict]]] = defaultdict(lambda: defaultdict(list))
    for r in valid:
        by_exp[r["experiment"]][cond(r)].append(r)

    summary_rows, comp_rows = [], []
    for exp_key, groups in by_exp.items():
        metrics = CORE + EXTRA.get(exp_key, [])
        labels = sorted(groups, key=_order_key)
        for lab in labels:
            row = {"experiment": exp_key, "condition": lab, "n_runs": len(groups[lab])}
            for key, _, _, _ in metrics:
                s = stats.summarize([r.get(key, NAN) if r.get(key) is not None else NAN for r in groups[lab]])
                for k, v in s.items():
                    row[f"{key}.{k}"] = v
            summary_rows.append(row)
        pairs = references(exp_key, labels)
        for key, label, lower, _ in metrics:
            by_ref: Dict[str, List[dict]] = defaultdict(list)
            for cand, ref in pairs:
                c = stats.compare(metric_by_rep(groups[cand], key), metric_by_rep(groups[ref], key))
                by_ref[ref].append({"experiment": exp_key, "metric": key, "label": label, "candidate": cand,
                                    "reference": ref, "lower_is_better": lower, **c})
            for ref, rows in by_ref.items():
                adj = stats.holm([r["p"] for r in rows])
                for r, a in zip(rows, adj):
                    r["p_holm"] = a
                    r["verdict"] = stats.verdict(r["n_pairs"], r["ci_lo"], r["ci_hi"], a, lower)
                    r["min_attainable_p"] = stats.min_attainable_p(r["n_pairs"])
                    comp_rows.append(r)
    write_csv(os.path.join(out, "summary.csv"), summary_rows)
    write_csv(os.path.join(out, "comparisons.csv"), comp_rows)
    _plots(results, matrix, valid, by_exp, os.path.join(out, "plots"))
    md = _markdown(matrix, runs, valid, failed, excluded, disturbed, by_exp, comp_rows)
    with open(os.path.join(out, "report.md"), "w", encoding="utf-8") as f:
        f.write(md)
    return {"runs": len(runs), "valid": len(valid), "failed": failed, "excluded": excluded, "disturbed": disturbed,
            "comparisons": comp_rows, "summary": summary_rows, "out": out}


# ----------------------------------------------------------------------------- markdown
def _markdown(matrix, runs, valid, failed, excluded, disturbed, by_exp, comps) -> str:
    L: List[str] = []
    L.append("# Experiment report (Phases 14-17)\n")
    L.append(f"Generated {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M UTC')} from `experiments/results/`.\n")
    commits = sorted({r["commit"] for r in runs})
    dirty = sum(1 for r in runs if r["git_dirty"])
    reps = sorted({(r["experiment"], cond(r)): 0 for r in valid})
    L.append(f"* Runs: **{len(runs)}** complete, **{len(valid)}** valid, {len(failed)} failed, {len(excluded)} excluded "
             f"(generator not healthy). Commits recorded in run metadata: {', '.join(commits) or '-'}; runs from a dirty tree: {dirty}.")
    L.append(f"* Possible host interference (system CPU > {INTERFERENCE_CPU_PCT:.0f}% at some second): {len(disturbed)} run(s)"
             + (f" - {', '.join(disturbed[:8])}{'…' if len(disturbed) > 8 else ''}" if disturbed else "") + ".")
    L.append(f"* Window {matrix['window_s']} s; warm-up {matrix['warmup_s']} s excluded from every metric; "
             f"seeds base {matrix['base_seed']} + repetition (identical arrival schedule for every policy of a repetition).\n")
    L.append("**Reading rules.** Cells are `mean ± sd [95 % bootstrap CI of the mean]` over repetitions. Comparisons are paired by "
             "repetition. A result is called *better*/*worse* only if n ≥ 5 pairs, the paired 95 % bootstrap CI excludes 0 and the "
             "Holm-adjusted exact sign-flip p < 0.05; otherwise it is *inconclusive* (or *insufficient_n* when n < 5, where p < 0.05 is "
             "unattainable by construction).\n")
    for exp_key, groups in by_exp.items():
        e = matrix["experiments"][exp_key]
        labels = sorted(groups, key=_order_key)
        ns = sorted({len(groups[l]) for l in labels})
        L.append(f"## {exp_key}: {e['title']}\n")
        L.append(f"*Question:* {e['question']}  \n*Scenario:* `{e['scenario']}`, pool `{e['pool']}`, {e['duration_s']} s; "
                 f"runs per condition: {', '.join(map(str, ns))}.\n")
        metrics = CORE + EXTRA.get(exp_key, [])
        L.append("| condition | " + " | ".join(m[1] for m in metrics) + " |")
        L.append("|---|" + "---|" * len(metrics))
        for lab in labels:
            cells = []
            for key, _, _, d in metrics:
                cells.append(fmt(stats.summarize([r.get(key, NAN) if r.get(key) is not None else NAN for r in groups[lab]]), d))
            L.append(f"| **{lab}** | " + " | ".join(cells) + " |")
        L.append("")
        ec = [c for c in comps if c["experiment"] == exp_key and c["metric"] in ("p95_ms", "error_rate", "ok_rps", "p99_ms")]
        if ec:
            L.append("Paired comparisons (candidate − reference; negative is better for latency/error):\n")
            L.append("| metric | candidate vs reference | mean diff [95% CI] | p (Holm) | verdict |")
            L.append("|---|---|---|---|---|")
            for c in ec:
                d = 4 if c["metric"] == "error_rate" else 1
                L.append(f"| {c['label']} | {c['candidate']} vs {c['reference']} | {c['mean_diff']:.{d}f} "
                         f"[{c['ci_lo']:.{d}f}, {c['ci_hi']:.{d}f}] | {c['p_holm']:.3f} | **{c['verdict']}** |")
            L.append("")
    # controller overhead & switching
    L.append("## Controller overhead and switching\n")
    L.append("| experiment | condition | decisions (accepted) | switches/min | decision latency avg / max (ms) | agent failures | "
             "safety fallbacks | agent CPU % / RAM MB | LB CPU % / RAM MB | request-path overhead p50 / p95 (ms) |")
    L.append("|---|---|---|---|---|---|---|---|---|---|")
    for exp_key, groups in by_exp.items():
        for lab in sorted(groups, key=_order_key):
            rs = groups[lab]
            g = lambda k: stats.summarize([r.get(k, NAN) if r.get(k) is not None else NAN for r in rs])["mean"]
            if not any(r["policy"] == "ai" for r in rs) and lab not in ("rule", "swrr", "lc", "rr"):
                continue
            ai = any(r["policy"] == "ai" for r in rs)
            L.append(f"| {exp_key} | {lab} | " + (f"{g('decisions'):.1f} ({g('decisions_accepted'):.1f})" if ai else "–") +
                     f" | {g('switches_per_min'):.2f} | " + (f"{g('decision_latency_avg_ms'):.1f} / {g('decision_latency_max_ms'):.1f}" if ai else "–") +
                     " | " + (f"{g('agent_failures'):.1f}" if ai else "–") + " | " + (f"{g('safety_fallbacks'):.1f}" if ai else "–") +
                     " | " + (f"{g('agent_cpu_pct'):.1f} / {g('agent_rss_mb'):.0f}" if ai else "–") +
                     f" | {g('lb_cpu_pct'):.1f} / {g('lb_rss_mb'):.0f} | {g('request_path_overhead_p50_ms'):.3f} / {g('request_path_overhead_p95_ms'):.3f} |")
    L.append("")
    return "\n".join(L) + "\n"


# ------------------------------------------------------------------------------- plots
def _plots(results, matrix, valid, by_exp, out_dir) -> None:
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
        import numpy as np
    except Exception:  # noqa: BLE001
        return
    for exp_key, groups in by_exp.items():
        labels = sorted(groups, key=_order_key)
        fig, axes = plt.subplots(1, 2, figsize=(11, 4))
        for ax, (key, title) in zip(axes, (("p95_ms", "p95 latency (ms)"), ("error_rate", "error rate"))):
            means, los, his = [], [], []
            for lab in labels:
                s = stats.summarize([r.get(key, NAN) if r.get(key) is not None else NAN for r in groups[lab]])
                means.append(s["mean"]); los.append(s["mean"] - s["ci_lo"]); his.append(s["ci_hi"] - s["mean"])
            ax.bar(range(len(labels)), means, yerr=[los, his], capsize=3)
            ax.set_xticks(range(len(labels))); ax.set_xticklabels(labels, rotation=30, ha="right", fontsize=8)
            ax.set_title(f"{exp_key}: {title}  (mean, 95% bootstrap CI)")
        fig.tight_layout(); fig.savefig(os.path.join(out_dir, f"{exp_key}_summary.png"), dpi=110); plt.close(fig)
    # time series of per-second p95 (mean over repetitions) for experiments with a change
    for exp_key in ("E4", "E6", "E7", "E8"):
        if exp_key not in by_exp:
            continue
        fig, ax = plt.subplots(figsize=(9, 4))
        for lab in sorted(by_exp[exp_key], key=_order_key):
            series = []
            for r in by_exp[exp_key][lab]:
                p = os.path.join(results, exp_key, r["run_id"], "loadgen.json")
                with open(p, encoding="utf-8") as f:
                    sec = json.load(f)["seconds"]
                series.append([s["p95_ms"] if s["completed"] - s["errors"] > 0 else np.nan for s in sec])
            n = min(len(s) for s in series)
            arr = np.array([s[:n] for s in series], dtype=float)
            ax.plot(np.nanmean(arr, axis=0), label=lab, lw=1.6)
        ch = matrix["experiments"][exp_key].get("change")
        if ch:
            ax.axvline(ch["at_s"], color="k", ls="--", lw=0.8)
        ax.set_yscale("log"); ax.set_xlabel("scenario time (s)"); ax.set_ylabel("per-second p95 (ms)")
        ax.set_title(f"{exp_key}: p95 over time (mean of repetitions; dashed = injected change)")
        ax.legend(fontsize=7); fig.tight_layout(); fig.savefig(os.path.join(out_dir, f"{exp_key}_p95_timeseries.png"), dpi=110)
        plt.close(fig)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", default=os.path.join(ROOT, "experiments", "results"))
    ap.add_argument("--out", default=None)
    a = ap.parse_args(argv)
    r = build(a.results, a.out)
    print(f"{r['runs']} runs ({r['valid']} valid, {len(r['failed'])} failed, {len(r['excluded'])} excluded); report in {r['out']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
