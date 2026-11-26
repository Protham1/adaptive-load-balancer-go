"""Execute ONE experiment run: start the isolated stack, select the policy mode,
run the seeded workload (warm-up is part of the scenario and excluded by the analysis),
collect measurements, write raw records and metadata.

Output files in <results>/<experiment>/<run_id>/ :
  run_metadata.json    commit, hardware, Go/Python versions, configuration, seed, date, scenario
  window_metrics.csv   per-window throughput, p50/p95/p99, errors, CPU/RAM, per-backend load, active strategy
  decisions.jsonl      (AI arms) state version, decision id, action, model version, reward components, accepted/rejected
  request_samples.csv  ~1% sample of requests: latency, status, backend (no bodies, no client identity)
  loadgen.json         raw generator export (offered/completed per second, histograms, LB samples, applied faults)
  resources.csv        per-process CPU/RAM samples (1 Hz)
  agent_status.json    Go-side agent counters at the end of the run (AI arms)
  done.json            written last; its presence marks a complete run
"""
from __future__ import annotations

import csv
import json
import os
import shutil
import subprocess
import time
import traceback
from typing import Dict, Optional

from ..analysis.windows import build_window_rows, write_csv
from . import metadata
from .stack import EXE, LB_URL, ROOT, Stack, http

POLICY_ACTION = {"rr": 0, "lc": 1, "swrr": 2, "ch": 3, "rule": 4}
ACTION_STRATEGY = {0: "Round-Robin", 1: "Least-Connections", 2: "Smooth-Weighted-Round-Robin",
                   3: "Consistent-Hash", 4: "Adaptive-Weighted-Round-Robin"}


def load_matrix() -> dict:
    with open(os.path.join(ROOT, "experiments", "scenarios", "experiments.json"), encoding="utf-8") as f:
        return json.load(f)


def resolve(matrix: dict, exp_key: str, arm: str, interval_s: Optional[float]) -> dict:
    """Turn (experiment, arm) into a concrete run configuration."""
    exp = matrix["experiments"][exp_key]
    arms = matrix.get("ablation_arms", {})
    spec = arms.get(arm, {"policy": arm})
    policy = spec["policy"]
    ai = dict(matrix["ai_defaults"])
    env: Dict[str, str] = {}
    if interval_s:  # E9: total control interval = settle + measure; min policy age = interval
        ai.update(settle_s=interval_s / 3.0, measure_s=interval_s * 2.0 / 3.0, min_policy_age_s=interval_s,
                  step_s=min(1.0, interval_s / 2.0))
    if policy == "ai":
        env.update({"LB_AGENT_SETTLE_S": str(round(ai["settle_s"], 3)), "LB_AGENT_MEASURE_S": str(round(ai["measure_s"], 3)),
                    "LB_AGENT_MIN_POLICY_AGE_S": str(ai["min_policy_age_s"]),
                    "LB_AGENT_SAFETY_COOLDOWN_S": str(ai["safety_cooldown_s"]), "LB_AGENT_STEP_S": str(ai["step_s"])})
    env.update(exp.get("env", {}))
    env.update(spec.get("env", {}))
    agent = None
    if policy == "ai":
        agent = {"algo": spec.get("agent_algo", ai["algo"]), "alpha": ai["alpha"], "context": ai["context"]}
    return {"policy": policy, "arm": arm, "agent": agent, "agent_env": env, "ai": ai, "interval_s": interval_s,
            "pool": matrix["pools"][exp["pool"]], "pool_name": exp["pool"], "affinity": bool(exp.get("affinity")),
            "scenario": exp["scenario"], "duration_s": exp["duration_s"]}


def merge_decisions(go_log: str, learner_log: str, out_path: str) -> int:
    """One line per Go-side decision (accepted or rejected), enriched with the learner's view
    (scores, explored) and the reward components of its evaluated window."""
    def read(p):
        if not os.path.exists(p):
            return []
        with open(p, encoding="utf-8") as f:
            return [json.loads(l) for l in f if l.strip()]
    go, learner = read(go_log), read(learner_log)
    dec = {e["decision_id"]: e for e in learner if e.get("event") == "decision"}
    out = {e["target"]: e for e in learner if e.get("event") == "outcome"}
    ign = {e["target"]: e for e in learner if e.get("event") == "outcome_ignored"}
    n = 0
    with open(out_path, "w", encoding="utf-8") as f:
        for g in go:
            did = g.get("decision_id", "")
            line = {"decision_id": did, "at": g.get("at"), "mode": g.get("mode"), "state_version": g.get("state_version"),
                    "current_strategy": g.get("current_strategy"), "proposed_strategy": g.get("proposed_strategy"),
                    "diagnostic": g.get("diagnostic"), "model_version": g.get("model_version"),
                    "policy_decision": g.get("reason"), "accepted": bool(g.get("applied")),
                    "failure": g.get("failure"), "latency_ms": g.get("latency_ms")}
            d = dec.get(did)
            if d:
                line.update(learner_action=d.get("action"), learner_scores=d.get("scores"), explored=d.get("explored"))
            o = out.get(did)
            if o:
                bd = o.get("breakdown") or {}
                line.update(reward=o.get("reward"), reward_components=bd.get("contributions"),
                            reward_normalized=bd.get("normalized"), reward_raw=bd.get("raw"),
                            reward_override=bd.get("override"))
            elif did in ign:
                line["outcome_ignored"] = ign[did].get("reason")
            f.write(json.dumps(line) + "\n")
            n += 1
    return n


def write_resources(path: str, rows: list) -> None:
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=["t_unix", "proc", "cpu_pct", "rss_mb"])
        w.writeheader()
        for r in rows:
            w.writerow({k: (round(v, 3) if isinstance(v, float) else v) for k, v in r.items()})


def run_one(matrix: dict, exp_key: str, arm: str, rep: int, out_root: str, bin_dir: str,
            interval_s: Optional[float] = None, sample_rate: float = 0.01, force: bool = False) -> str:
    run_id = f"{exp_key}_{arm}_r{rep}" + (f"_i{int(interval_s)}" if interval_s else "")
    run_dir = os.path.join(out_root, exp_key, run_id)
    if os.path.exists(os.path.join(run_dir, "done.json")) and not force:
        return run_dir
    if os.path.exists(run_dir):
        shutil.rmtree(run_dir)
    os.makedirs(run_dir)
    cfg = resolve(matrix, exp_key, arm, interval_s)
    seed = matrix["base_seed"] + rep
    scenario_path = os.path.join(ROOT, "experiments", "scenarios", cfg["scenario"])
    with open(scenario_path, encoding="utf-8") as f:
        scenario = json.load(f)
    pool = cfg["pool"]
    lb_env = {
        "LB_BACKENDS": ",".join(f"{b['name']}:{b['port']}:{b['weight']}" for b in pool),
        "LB_NO_DUMMY": "1", "LB_HEALTH_INTERVAL_MS": "1000", "LB_QUIET": "1", "LB_SAFE_ACTION": "2",
        "LB_CONTROLLER_INTERVAL_S": "3", "LB_DECISION_LOG": os.path.join(run_dir, "go_decisions.jsonl"),
    }
    if cfg["affinity"]:
        lb_env["LB_AFFINITY_REQUIRED"] = "1"
    lb_env.update(cfg["agent_env"])
    agent = dict(cfg["agent"], seed=seed) if cfg["agent"] else None
    lg_out = os.path.join(run_dir, "loadgen.json")
    started = time.time()
    status = {"ok": False}
    try:
        with Stack(bin_dir, run_dir, pool, lb_env, agent, backend_seed=seed) as stack:
            if cfg["policy"] in POLICY_ACTION:
                http("POST", LB_URL + "/api/policy", {"action": POLICY_ACTION[cfg["policy"]]})
            active = http("GET", LB_URL + "/api/policy")["policy"]["strategy"]
            if cfg["policy"] in POLICY_ACTION:
                expected = ACTION_STRATEGY[POLICY_ACTION[cfg["policy"]]]
                if active != expected:
                    raise RuntimeError(f"policy mode not applied: active={active} expected={expected}")
            lg_args = [os.path.join(bin_dir, "loadgen" + EXE), "-scenario", scenario_path, "-target", LB_URL, "-out", lg_out,
                       "-seed", str(seed), "-window", str(matrix["window_s"]), "-sample", str(sample_rate),
                       "-samples-out", os.path.join(run_dir, "request_samples.csv"), "-ctrl", stack.ctrl_arg()]
            p = subprocess.run(lg_args, capture_output=True, text=True, cwd=ROOT, timeout=cfg["duration_s"] + 90)
            status["loadgen_stdout"] = p.stdout.strip()
            if p.returncode != 0:
                raise RuntimeError(f"loadgen failed: {p.stderr.strip()[:500]}")
            final = http("GET", LB_URL + "/api/policy")["policy"]
            status["final_strategy"], status["final_policy_reason"] = final["strategy"], final.get("reason")
            if agent:
                st = http("GET", LB_URL + "/api/agent")
                with open(os.path.join(run_dir, "agent_status.json"), "w", encoding="utf-8") as f:
                    json.dump(st, f, indent=1)
            resources = list(stack.resources)
        with open(lg_out, encoding="utf-8") as f:
            lg = json.load(f)
        write_resources(os.path.join(run_dir, "resources.csv"), resources)
        write_csv(os.path.join(run_dir, "window_metrics.csv"), build_window_rows(lg, resources, [b["name"] for b in pool]))
        raw = os.path.join(run_dir, "raw")
        os.makedirs(raw, exist_ok=True)
        n_dec = 0
        if agent:
            n_dec = merge_decisions(os.path.join(run_dir, "go_decisions.jsonl"), os.path.join(run_dir, "learner_events.jsonl"),
                                    os.path.join(run_dir, "decisions.jsonl"))
        for name in ("go_decisions.jsonl", "learner_events.jsonl"):
            if os.path.exists(os.path.join(run_dir, name)):
                shutil.move(os.path.join(run_dir, name), os.path.join(raw, name))
        status.update(ok=True, decisions=n_dec, generator_ok=lg["summary"]["generator_ok"])
    except Exception as e:  # noqa: BLE001
        status.update(error=repr(e), traceback=traceback.format_exc())
    meta = metadata.build(run_id, exp_key, arm, rep, seed, scenario_path, scenario,
                          {"policy": cfg["policy"], "agent": agent, "interval_s": interval_s, "pool_name": cfg["pool_name"],
                           "pool": pool, "lb_env": {k: v for k, v in lb_env.items() if k != "LB_DECISION_LOG"},
                           "window_s": matrix["window_s"], "warmup_s": matrix["warmup_s"], "sample_rate": sample_rate,
                           "ai_defaults": cfg["ai"] if cfg["policy"] == "ai" else None})
    meta["wall_clock_s"] = round(time.time() - started, 1)
    meta["status"] = status
    with open(os.path.join(run_dir, "run_metadata.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)
    with open(os.path.join(run_dir, "done.json" if status["ok"] else "failed.json"), "w", encoding="utf-8") as f:
        json.dump(status, f, indent=1)
    return run_dir
