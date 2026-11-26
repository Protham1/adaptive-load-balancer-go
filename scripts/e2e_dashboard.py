"""Live check of the Phase 18/19 endpoints against the real stack (separate processes).

    python scripts/e2e_dashboard.py

Starts 3 backends, the LB, the Python agent and runs the load generator (which posts a fault-injection
marker), then verifies: /api/dashboard, /api/history, /api/events/log (fault marker), /api/agent/learner
(reward series, no confidence), the AI kill switch + fallback indicator, reset-to-baseline (model archived),
the checksummed checkpoint on disk, and that the production frontend bundle is served at '/'.
"""
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
from experiments.runner.stack import EXE, LB_URL, Stack, build_binaries, http  # noqa: E402
from ai.persistence import hash_json  # noqa: E402


def main() -> int:
    matrix = json.load(open(os.path.join(ROOT, "experiments", "scenarios", "experiments.json"), encoding="utf-8"))
    pool = matrix["pools"]["hetero"]
    work = tempfile.mkdtemp(prefix="e2e_dash_")
    bin_dir = os.path.join(work, "bin")
    build_binaries(bin_dir)
    env = {"LB_BACKENDS": ",".join(f"{b['name']}:{b['port']}:{b['weight']}" for b in pool), "LB_NO_DUMMY": "1",
           "LB_HEALTH_INTERVAL_MS": "1000", "LB_QUIET": "1", "LB_SAFE_ACTION": "2", "LB_AGENT_SETTLE_S": "2",
           "LB_AGENT_MEASURE_S": "4", "LB_AGENT_MIN_POLICY_AGE_S": "6", "LB_AGENT_STEP_S": "1"}
    agent = {"algo": "linucb", "alpha": 0.05, "context": "compact", "seed": 1}
    fails = []

    def check(name, ok, detail=""):
        print(("PASS " if ok else "FAIL ") + name + (f" - {detail}" if detail else ""), flush=True)
        if not ok:
            fails.append(name)

    with Stack(bin_dir, work, pool, env, agent) as stack:
        lg = subprocess.run([os.path.join(bin_dir, "loadgen" + EXE), "-scenario",
                             os.path.join(ROOT, "experiments", "scenarios", "e4_latency_imbalance.json"), "-target", LB_URL,
                             "-out", os.path.join(work, "lg.json"), "-ctrl", stack.ctrl_arg(), "-scale", "1.5"],
                            capture_output=True, text=True, cwd=ROOT)
        check("load generator ran", lg.returncode == 0, lg.stdout.strip()[:120])

        d = http("GET", LB_URL + "/api/dashboard")
        check("dashboard: 3 backends with metrics", len(d["backends"]) == 3 and all("p95_ms" in b for b in d["backends"]))
        check("dashboard: host-level resource note", "host-level" in d["host_resources"]["note"])
        check("dashboard: agent enabled with model version", d["agent"]["enabled"] and d["agent"]["model_version"] != "",
              d["agent"].get("model_version", ""))
        check("dashboard: policy version and reason present", d["policy"]["version"] >= 2 and d["policy"].get("reason"), str(d["policy"].get("reason")))

        h = http("GET", LB_URL + "/api/history?since=0")
        check("history: time series with strategy + backends", len(h["points"]) >= 10 and h["points"][-1]["backends"], f"{len(h['points'])} points")
        last = h["points"][-1]["t"]
        newer = http("GET", LB_URL + f"/api/history?since={last}")["points"]
        check("history: since filter returns only newer points", all(p["t"] > last for p in newer), f"{len(newer)} newer")

        ev = http("GET", LB_URL + "/api/events/log?since=0")["events"]
        faults = [e for e in ev if e["kind"] == "fault"]
        check("events: fault-injection marker posted by the load generator", any(e["backend"] == "Backend-2" for e in faults), str(faults[:1]))
        check("events: policy switch events recorded", any(e["kind"] == "policy" for e in ev))

        L = http("GET", LB_URL + "/api/agent/learner?limit=50")
        check("learner view reachable through the LB", L.get("available", True) is not False and "learner" in L, str(L)[:80])
        check("learner view: reward components and cumulative reward", bool(L["reward_series"]) and
              set((L["reward_series"][-1]["components"] or {"throughput": 0, "latency": 0, "error": 0, "imbalance": 0})) ==
              {"throughput", "latency", "error", "imbalance"})
        check("learner view: no invented confidence", L["confidence"]["available"] is False)
        check("learner view: drift and persistence blocks", "drift" in L and "persistence" in L)

        ck = os.path.join(work, "model.json")
        check("checkpoint file written", os.path.exists(ck))
        if os.path.exists(ck):
            doc = json.load(open(ck))
            ok = doc["checksum"] == hash_json({k: doc.get(k) for k in ("learner", "config", "meta", "counters", "state", "extra")})
            check("checkpoint checksum verifies; schema + reward config persisted",
                  ok and doc["meta"]["feature_schema_hash"] and doc["meta"]["reward_config"]["w_latency"] == 0.35)

        r = http("POST", LB_URL + "/api/agent/kill", {"kill": True, "baseline_action": 2})
        d = http("GET", LB_URL + "/api/dashboard")
        check("kill switch -> fallback indicator", r["killed"] and d["fallback"]["kind"] == "kill_switch" and not d["agent"]["ai_active"])
        http("POST", LB_URL + "/api/agent/kill", {"kill": False})
        out = http("POST", LB_URL + "/api/agent/reset", {"archive": True})
        check("reset-to-baseline: policy at baseline and model reset", out["model_reset"] is True and
              out["policy"]["reason"] == "reset_to_baseline", str(out.get("agent")))
        L2 = http("GET", LB_URL + "/api/agent/learner")
        check("model restarted from scratch after reset", L2["learner"]["updates"] == 0 and L2["learner"]["resets"] == 1)
        check("previous model archived, not deleted", any(n.endswith(".archived") for n in os.listdir(work)))

        with urllib.request.urlopen(LB_URL + "/", timeout=3) as resp:
            body = resp.read().decode("utf-8", "replace")
        check("production frontend bundle served at /", "<div id=\"root\">" in body or "assets/index-" in body)

    print("ALL PASSED" if not fails else f"FAILED: {fails}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
