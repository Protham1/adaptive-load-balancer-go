"""Build window_metrics.csv rows from a loadgen export plus resource samples."""
from __future__ import annotations

import csv
import math
from typing import Dict, List, Sequence

from . import hist as H


def _sample_at(samples: Sequence[dict], t: float):
    """Last LB sample taken at or before t (else the first sample)."""
    best = None
    for s in samples:
        if s["t"] <= t:
            best = s
        else:
            break
    return best if best is not None else (samples[0] if samples else None)


def _mean(xs):
    xs = [x for x in xs if x is not None and not (isinstance(x, float) and math.isnan(x))]
    return sum(xs) / len(xs) if xs else float("nan")


def build_window_rows(lg: dict, resources: Sequence[dict], backend_names: Sequence[str]) -> List[dict]:
    """One row per export window: throughput, latency percentiles (from merged
    histograms), errors by category, per-backend load, active strategy, CPU/RAM."""
    t_unix0 = lg["generator"]["start_unix"]
    wlen = lg["generator"]["window_s"]
    duration = lg["summary"]["duration_s"]
    lb = sorted(lg.get("lb") or [], key=lambda s: s["t"])
    rows = []
    for w in lg["windows"]:
        if w["t0"] >= duration:
            break
        t0, t1 = w["t0"], min(w["t1"], duration)
        span = max(t1 - t0, 1e-9)
        hist = w["hist_ok"]
        row = {
            "window_start_s": round(t0, 3), "window_end_s": round(t1, 3),
            "offered": w["offered"], "sent": w["sent"], "completed": w["completed"], "ok": w["ok"],
            "failed": w["failed"], "err_5xx": w["err_5xx"], "err_4xx": w["err_4xx"],
            "err_timeout": w["err_timeout"], "err_transport": w["err_transport"],
            "offered_rps": round(w["offered"] / span, 2), "completed_rps": round(w["completed"] / span, 2),
            "ok_rps": round(w["ok"] / span, 2),
            "lat_mean_ms": round(w["lat_sum_ok_ms"] / w["ok"], 3) if w["ok"] else float("nan"),
            "p50_ms": round(H.percentile(hist, 0.50), 3), "p95_ms": round(H.percentile(hist, 0.95), 3),
            "p99_ms": round(H.percentile(hist, 0.99), 3),
        }
        a, b = _sample_at(lb, t0), _sample_at(lb, t1)
        win_samples = [s for s in lb if t0 <= s["t"] < t1]
        row["strategy"] = (win_samples[-1]["strategy"] if win_samples else (b["strategy"] if b else ""))
        row["policy_version"] = b["policy_version"] if b else ""
        row["switch_count"] = b["switch_count"] if b else ""
        row["overhead_p95_ms"] = round(_mean([s["overhead_p95_ms"] for s in win_samples]), 4)
        for name in backend_names:
            if a and b and name in a["backends"] and name in b["backends"] and b["t"] > a["t"]:
                d = b["backends"][name]["total_requests"] - a["backends"][name]["total_requests"]
                row[f"{name}_rps"] = round(d / (b["t"] - a["t"]), 2)
            else:
                row[f"{name}_rps"] = float("nan")
            row[f"{name}_alive"] = (round(_mean([1.0 if s["backends"].get(name, {}).get("alive") else 0.0
                                                 for s in win_samples]), 2) if win_samples else float("nan"))
        # resources (timestamps are unix seconds)
        res = [r for r in resources if t_unix0 + t0 <= r["t_unix"] < t_unix0 + t1]
        def agg(pred, key):
            return round(_mean([r[key] for r in res if pred(r["proc"])]), 2)
        row["lb_cpu_pct"] = agg(lambda p: p == "lb", "cpu_pct")
        row["lb_rss_mb"] = agg(lambda p: p == "lb", "rss_mb")
        row["backends_cpu_pct"] = round(sum(_mean([r["cpu_pct"] for r in res if r["proc"] == n])
                                            for n in backend_names if any(r["proc"] == n for r in res)), 2) if res else float("nan")
        row["backends_rss_mb"] = round(sum(_mean([r["rss_mb"] for r in res if r["proc"] == n])
                                           for n in backend_names if any(r["proc"] == n for r in res)), 2) if res else float("nan")
        row["agent_cpu_pct"] = agg(lambda p: p == "agent", "cpu_pct")
        row["agent_rss_mb"] = agg(lambda p: p == "agent", "rss_mb")
        rows.append(row)
    return rows


def write_csv(path: str, rows: Sequence[dict]) -> None:
    if not rows:
        open(path, "w").close()
        return
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
        w.writeheader()
        w.writerows(rows)
