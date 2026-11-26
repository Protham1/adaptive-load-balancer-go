"""Per-run metrics (Phase 17). Definitions are fixed here and repeated in documentation.md.

Measurement period = export windows with warm-up <= t0 < scenario duration (the warm-up is a whole
number of windows and is identical for every policy).

  throughput_ok_rps      successful (non-error) requests completed per second; offered_rps reported beside it
  latency p50/p95/p99    percentiles of SUCCESSFUL requests from the MERGED log-bucket histogram (upper bucket
                         bound, <= +5 % conservative); mean from the exact sum. Failures are NOT in the latency
                         distribution; they are in error_rate
  error_rate             failed / completed requests; failure = HTTP 5xx or 4xx, client timeout, or transport error
                         (categories reported separately)
  imbalance_cv, jain     coefficient of variation / Jain index of the capacity-adjusted service rate
                         x_i = (requests_i / s) / declared_weight_i over backends alive in >= 90 % of samples.
                         Raw request counts are NOT used (misleading on a 3:2:1 pool)
  overload_s             seconds after the injected change where the acceptance criterion is violated
  adaptation_s           time from the injected change to the start of STABLE acceptable behaviour, i.e. the
                         last violating second + 1 minus the change time; censored if not stable for
                         `stable_for_s` before the change ends. Criterion (per 1-s bucket): error fraction <=
                         err_max AND p95 <= max(p95_factor * reference, reference + p95_margin_ms), where the
                         reference is the run's own median per-second p95 between warm-up and the change
  detect_s / reentry_s   (E7) fault -> LB marks the backend down; restore -> LB marks it up / its traffic share
                         is back to >= 80 % of its capacity share for 2 consecutive samples
  switches_per_min       policy strategy changes per minute (LB switch counter) and time share per strategy
  controller overhead    AI decision latency, communication failures, AI-process CPU/RAM, LB CPU/RAM, and the
                         request-path overhead measured inside the LB (p50/p95)
"""
from __future__ import annotations

import csv
import json
import math
import os
from typing import Dict, List, Optional

import numpy as np

from . import hist as H

NAN = float("nan")


def _load(path: str):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _mean(xs):
    xs = [x for x in xs if x is not None and not (isinstance(x, float) and math.isnan(x))]
    return float(np.mean(xs)) if xs else NAN


def measurement_windows(lg: dict, warmup_s: float, scenario_s: float) -> List[dict]:
    return [w for w in lg["windows"] if w["t0"] >= warmup_s - 1e-9 and w["t0"] < scenario_s - 1e-9]


def latency_and_throughput(wins: List[dict], scenario_s: float) -> Dict[str, float]:
    span = sum(min(w["t1"], scenario_s) - w["t0"] for w in wins) or NAN
    completed = sum(w["completed"] for w in wins)
    ok = sum(w["ok"] for w in wins)
    failed = sum(w["failed"] for w in wins)
    hist = H.merge(w["hist_ok"] for w in wins)
    out = {
        "offered_rps": sum(w["offered"] for w in wins) / span, "completed_rps": completed / span, "ok_rps": ok / span,
        "p50_ms": H.percentile(hist, 0.50), "p95_ms": H.percentile(hist, 0.95), "p99_ms": H.percentile(hist, 0.99),
        "lat_mean_ms": (sum(w["lat_sum_ok_ms"] for w in wins) / ok) if ok else NAN,
        "error_rate": (failed / completed) if completed else NAN,
        "completed": completed, "failed": failed,
    }
    for key, name in (("err_5xx", "frac_5xx"), ("err_timeout", "frac_timeout"), ("err_transport", "frac_transport"),
                      ("err_4xx", "frac_4xx")):
        out[name] = (sum(w[key] for w in wins) / completed) if completed else NAN
    return out


def complete_samples(lb: List[dict], t_lo: float, t_hi: float) -> List[dict]:
    """LB samples in [t_lo, t_hi] that actually carry data (a 1 Hz status request can time out, leaving
    an empty sample; counters from such a sample would be 0, not 'missing')."""
    return [x for x in lb if t_lo <= x["t"] <= t_hi and x.get("backends") and x.get("strategy")]


def imbalance(lb: List[dict], weights: Dict[str, float], t_lo: float, t_hi: float) -> Dict[str, float]:
    s = complete_samples(lb, t_lo, t_hi)
    if len(s) < 3:
        return {"imbalance_cv": NAN, "jain": NAN, "backends_in_fairness_set": 0}
    dt = s[-1]["t"] - s[0]["t"]
    rates = {}
    for name, w in weights.items():
        alive = [1.0 if x["backends"].get(name, {}).get("alive") else 0.0 for x in s]
        if name not in s[0]["backends"] or name not in s[-1]["backends"] or np.mean(alive) < 0.9:
            continue
        d = s[-1]["backends"][name]["total_requests"] - s[0]["backends"][name]["total_requests"]
        rates[name] = (d / dt) / w
    x = np.array(list(rates.values()), dtype=float)
    if len(x) < 2 or x.mean() == 0:
        return {"imbalance_cv": NAN, "jain": NAN, "backends_in_fairness_set": len(x)}
    return {"imbalance_cv": float(x.std() / x.mean()), "jain": float(x.sum() ** 2 / (len(x) * (x ** 2).sum())),
            "backends_in_fairness_set": len(x)}


def adaptation(seconds: List[dict], warmup_s: float, change: Optional[dict], crit: dict) -> Dict[str, float]:
    if not change:
        return {}
    at, end = float(change["at_s"]), float(change["end_s"])
    pre = [s["p95_ms"] for s in seconds if warmup_s <= s["t"] < at and s["completed"] > 0 and s["p95_ms"] > 0]
    ref = float(np.median(pre)) if pre else NAN
    if math.isnan(ref):
        return {"reference_p95_ms": NAN, "overload_s": NAN, "adaptation_s": NAN, "censored": False}
    limit = max(crit["p95_factor"] * ref, ref + crit["p95_margin_ms"])
    viol = []
    for s in seconds:
        t = s["t"]
        if not (at <= t < end):
            continue
        ok_count = s["completed"] - s["errors"]
        err = s["errors"] / s["completed"] if s["completed"] else 1.0
        bad = (s["completed"] == 0) or err > crit["err_max"] or ok_count <= 0 or s["p95_ms"] > limit
        if bad:
            viol.append(t)
    if not viol:
        return {"reference_p95_ms": ref, "overload_s": 0.0, "adaptation_s": 0.0, "censored": False}
    last = max(viol)
    stable_len = end - (last + 1)
    censored = stable_len < crit["stable_for_s"]
    return {"reference_p95_ms": ref, "overload_s": float(len(viol)),
            "adaptation_s": NAN if censored else float(last + 1 - at), "censored": censored}


def failure_recovery(lb: List[dict], faults: List[dict], change: dict, weights: Dict[str, float]) -> Dict[str, float]:
    """Detection and re-entry times for the failure/recovery experiment."""
    lb = complete_samples(lb, -1e18, 1e18)
    b = change.get("failure_backend")
    if not b or not faults:
        return {}
    down = [f for f in faults if f["backend"] == b and f["ok"]]
    if not down:
        return {}
    t_fail = min(f["actual_s"] for f in down)
    t_up = max((f["actual_s"] for f in down), default=NAN) if len(down) > 1 else NAN
    out = {"detect_s": NAN, "reentry_alive_s": NAN, "reentry_share_s": NAN}
    for s in lb:
        if s["t"] >= t_fail and not s["backends"].get(b, {}).get("alive", True):
            out["detect_s"] = s["t"] - t_fail
            break
    if not math.isnan(t_up):
        for s in lb:
            if s["t"] >= t_up and s["backends"].get(b, {}).get("alive"):
                out["reentry_alive_s"] = s["t"] - t_up
                break
        expected = weights[b] / sum(weights.values())
        streak = 0
        for p, c in zip(lb, lb[1:]):
            if c["t"] < t_up:
                continue
            tot = sum(c["backends"][n]["total_requests"] - p["backends"][n]["total_requests"]
                      for n in weights if n in c["backends"] and n in p["backends"])
            d = c["backends"][b]["total_requests"] - p["backends"][b]["total_requests"]
            streak = streak + 1 if tot > 0 and d / tot >= 0.8 * expected else 0
            if streak >= 2:
                out["reentry_share_s"] = c["t"] - t_up
                break
    return out


def switching(lb: List[dict], t_lo: float, t_hi: float) -> Dict[str, float]:
    s = complete_samples(lb, t_lo, t_hi)
    if len(s) < 2:
        return {"switches_per_min": NAN}
    minutes = (s[-1]["t"] - s[0]["t"]) / 60.0
    out = {"switches_per_min": (s[-1]["switch_count"] - s[0]["switch_count"]) / minutes if minutes > 0 else NAN,
           "switch_total": s[-1]["switch_count"] - s[0]["switch_count"]}
    shares: Dict[str, float] = {}
    for x in s:
        shares[x["strategy"]] = shares.get(x["strategy"], 0) + 1
    for k, v in shares.items():
        out["time_share_" + k.lower().replace("-", "_")] = v / len(s)
    return out


def resources(path: str, t0_unix: float, t_lo: float, t_hi: float, backend_names: List[str]) -> Dict[str, float]:
    if not os.path.exists(path):
        return {}
    rows = [r for r in csv.DictReader(open(path, encoding="utf-8"))]
    sel = [r for r in rows if t0_unix + t_lo <= float(r["t_unix"]) < t0_unix + t_hi]
    def m(pred, key):
        return _mean([float(r[key]) for r in sel if pred(r["proc"])])
    out = {"lb_cpu_pct": m(lambda p: p == "lb", "cpu_pct"), "lb_rss_mb": m(lambda p: p == "lb", "rss_mb"),
           "agent_cpu_pct": m(lambda p: p == "agent", "cpu_pct"), "agent_rss_mb": m(lambda p: p == "agent", "rss_mb"),
           "backends_cpu_pct": sum(_mean([float(r["cpu_pct"]) for r in sel if r["proc"] == n]) for n in backend_names
                                   if any(r["proc"] == n for r in sel)) if sel else NAN,
           "system_cpu_pct_mean": m(lambda p: p == "system", "cpu_pct")}
    sysv = [float(r["cpu_pct"]) for r in sel if r["proc"] == "system"]
    out["system_cpu_pct_max"] = max(sysv) if sysv else NAN
    return out


def controller(run_dir: str, lb: List[dict], t_lo: float, t_hi: float) -> Dict[str, float]:
    out: Dict[str, float] = {}
    s = complete_samples(lb, t_lo, t_hi)
    out["request_path_overhead_p50_ms"] = _mean([x["overhead_p50_ms"] for x in s])
    out["request_path_overhead_p95_ms"] = _mean([x["overhead_p95_ms"] for x in s])
    p = os.path.join(run_dir, "agent_status.json")
    if os.path.exists(p):
        st = _load(p)
        stats = st["stats"]
        out.update(decision_latency_avg_ms=stats["avg_latency_ms"], decision_latency_max_ms=stats["max_latency_ms"],
                   agent_requests=stats["requests"], agent_failures=sum((stats.get("failures_by_kind") or {}).values()),
                   agent_held_stale=stats.get("held_stale_state", 0),
                   safety_fallbacks=len(st["manager"].get("safety_events") or []))
    d = os.path.join(run_dir, "decisions.jsonl")
    if os.path.exists(d):
        rows = [json.loads(l) for l in open(d, encoding="utf-8") if l.strip()]
        real = [r for r in rows if r.get("decision_id")]
        out["decisions"] = len(real)
        out["decisions_accepted"] = sum(1 for r in real if r.get("accepted"))
        out["decisions_rejected"] = sum(1 for r in real if not r.get("accepted") and r.get("proposed_strategy"))
        rew = [r["reward"] for r in real if r.get("reward") is not None]
        out["reward_mean"] = _mean(rew)
        out["reward_override_windows"] = sum(1 for r in real if r.get("reward_override"))
    return out


def compute_run_metrics(run_dir: str, matrix: dict) -> Optional[dict]:
    """Metrics of one completed run, or None if the run is not complete."""
    if not os.path.exists(os.path.join(run_dir, "done.json")):
        return None
    meta = _load(os.path.join(run_dir, "run_metadata.json"))
    lg = _load(os.path.join(run_dir, "loadgen.json"))
    exp = matrix["experiments"][meta["experiment"]]
    warm, scen_s = matrix["warmup_s"], exp["duration_s"]
    wins = measurement_windows(lg, warm, scen_s)
    pool = meta["config"]["pool"]
    weights = {b["name"]: float(b["weight"]) for b in pool}
    lb = sorted(lg.get("lb") or [], key=lambda x: x["t"])
    m = {"run_id": meta["run_id"], "experiment": meta["experiment"], "arm": meta["arm"],
         "policy": meta["config"]["policy"], "rep": meta["repetition"], "interval_s": meta["config"].get("interval_s"),
         "commit": meta["git"]["commit"][:10], "git_dirty": meta["git"]["dirty"],
         "generator_ok": bool(lg["summary"]["generator_ok"]),
         "schedule_lag_p99_ms": lg["summary"]["schedule_lag_p99_ms"],
         "dropped_by_generator": lg["summary"]["dropped_by_generator"]}
    m.update(latency_and_throughput(wins, scen_s))
    m["throughput_ok_rps"] = m["ok_rps"]
    m.update(imbalance(lb, weights, warm, scen_s))
    m.update(adaptation(lg["seconds"], warm, exp.get("change"), matrix["criteria"]))
    if exp.get("change") and exp["change"].get("failure_backend"):
        m.update(failure_recovery(lb, lg.get("faults_applied") or [], exp["change"], weights))
    m.update(switching(lb, warm, scen_s))
    m.update(resources(os.path.join(run_dir, "resources.csv"), lg["generator"]["start_unix"], warm, scen_s,
                       [b["name"] for b in pool]))
    m.update(controller(run_dir, lb, warm, scen_s))
    if "affinity" in lg["summary"]:
        m["stickiness"] = lg["summary"]["affinity"]["stickiness"]
        m["clients_multi_backend"] = lg["summary"]["affinity"]["clients_served_by_multiple_backends"]
    m["valid"] = bool(m["generator_ok"])
    return m
