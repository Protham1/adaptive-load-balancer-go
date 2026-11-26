"""Run Phase 13 fault-injection scenarios against a real, process-isolated stack.

  python scripts/run_scenarios.py --scenarios all --baseline 2 --out results/phase13
  python scripts/run_scenarios.py --scenarios backend_failure --agent --scale 1

Per scenario it starts: 3 standalone backends (separate processes, fault control on
:9081-:9083), the load balancer (LB_NO_DUMMY=1, 1 s health checks), optionally the
Python decision service, then runs cmd/loadgen and derives observations from its
output (health-detection time, traffic share per phase, error rate / p95 per phase,
offered vs completed throughput, generator health).
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = ".exe" if os.name == "nt" else ""
BACKENDS = [("Backend-1", 8081, 24), ("Backend-2", 8082, 16), ("Backend-3", 8083, 8)]  # conc ~ 3:2:1
ALL = ["cpu_pressure", "latency_degradation", "error_injection", "backend_failure", "recovery",
       "flash_crowd", "long_requests"]


def build(bin_dir):
    os.makedirs(bin_dir, exist_ok=True)
    for name, pkg in (("lb", "."), ("backend", "./cmd/backend"), ("loadgen", "./cmd/loadgen")):
        subprocess.run(["go", "build", "-o", os.path.join(bin_dir, name + EXE), pkg], cwd=ROOT, check=True)


def http(method, url, body=None, timeout=3):
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"{}")


def wait_ready(url, timeout=15):
    end = time.time() + timeout
    while time.time() < end:
        try:
            http("GET", url)
            return
        except Exception:
            time.sleep(0.2)
    raise RuntimeError(f"{url} did not become ready")


class Stack:
    def __init__(self, bin_dir, log_dir, agent=False, env_extra=None, safe_action=2):
        self.procs, self.bin, self.logs, self.agent = [], bin_dir, log_dir, agent
        self.env_extra, self.safe_action = env_extra or {}, safe_action
        os.makedirs(log_dir, exist_ok=True)

    def _spawn(self, cmd, name, env=None):
        f = open(os.path.join(self.logs, name + ".log"), "w", encoding="utf-8")
        p = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, cwd=ROOT, env=env)
        self.procs.append((p, f))
        return p

    def __enter__(self):
        for name, port, conc in BACKENDS:
            self._spawn([os.path.join(self.bin, "backend" + EXE), "-port", str(port), "-name", name,
                         "-conc", str(conc)], name)
        env = dict(os.environ, LB_NO_DUMMY="1", LB_HEALTH_INTERVAL_MS="1000", LB_SAFE_ACTION=str(self.safe_action))
        env.update(self.env_extra)
        if self.agent:
            for stale in ("model.json", "decisions.jsonl"):  # every run starts from an untrained model
                try:
                    os.remove(os.path.join(self.logs, stale))
                except FileNotFoundError:
                    pass
            self._spawn([sys.executable, "-W", "ignore", "-m", "ai.server", "--port", "8090",
                         "--model", os.path.join(self.logs, "model.json"),
                         "--log", os.path.join(self.logs, "decisions.jsonl")], "agent", dict(os.environ))
            wait_ready("http://127.0.0.1:8090/v1/health")
            env["LB_AGENT_URL"] = "http://127.0.0.1:8090"
        time.sleep(0.5)
        self._spawn([os.path.join(self.bin, "lb" + EXE)], "lb", env)
        wait_ready("http://localhost:8080/api/status")
        return self

    def __exit__(self, *a):
        for p, f in self.procs:
            try:
                p.terminate()
                p.wait(timeout=5)
            except Exception:
                p.kill()
            f.close()
        time.sleep(0.5)


# --------------------------------------------------------------------------- analysis
def _avg(xs):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else None


def phase_stats(res, lo, hi):
    secs = [s for s in res["seconds"] if lo <= s["t"] < hi]
    comp = sum(s["completed"] for s in secs)
    return {"window_s": [lo, hi], "offered_rps": _avg([s["offered"] for s in secs]),
            "completed_rps": _avg([s["completed"] for s in secs]),
            "error_rate": (sum(s["errors"] for s in secs) / comp) if comp else None,
            "p95_ms_mean": _avg([s["p95_ms"] for s in secs if s["completed"]]),
            "p99_ms_mean": _avg([s["p99_ms"] for s in secs if s["completed"]])}


def share(res, backend, lo, hi):
    """Fraction of LB-routed requests that went to `backend` in [lo, hi) (from /api/status counters)."""
    lb = [s for s in res["lb"] if lo <= s["t"] <= hi and s["backends"]]
    if len(lb) < 2:
        return None
    a, b = lb[0]["backends"], lb[-1]["backends"]
    tot = sum(b[k]["total_requests"] - a[k]["total_requests"] for k in b if k in a)
    return (b[backend]["total_requests"] - a[backend]["total_requests"]) / tot if tot > 0 else None


def first_time(res, backend, alive, after):
    for s in res["lb"]:
        if s["t"] >= after and backend in s["backends"] and s["backends"][backend]["alive"] == alive:
            return s["t"] - after
    return None


def analyze(name, res):
    sc, dur = res["scenario"], res["summary"]["duration_s"]
    faults = sorted(res.get("faults_applied") or [], key=lambda f: f["actual_s"])
    obs = {"scenario": name, "summary": res["summary"]}
    strategies = sorted({s["strategy"] for s in res["lb"] if s["strategy"]})
    obs["strategies_seen"] = strategies
    obs["policy_versions"] = [min((s["policy_version"] for s in res["lb"]), default=0),
                              max((s["policy_version"] for s in res["lb"]), default=0)]
    obs["faults_ok"] = all(f["ok"] for f in faults)
    if faults:
        b = faults[0]["backend"]
        t1 = faults[0]["actual_s"]
        t2 = faults[1]["actual_s"] if len(faults) > 1 else dur
        obs["fault_backend"] = b
        obs["phases"] = {"before": phase_stats(res, 5, t1), "during": phase_stats(res, t1 + 6, t2),
                         "after": phase_stats(res, t2 + 8, dur - 1) if len(faults) > 1 else None}
        obs["traffic_share_of_fault_backend"] = {"before": share(res, b, 5, t1), "during": share(res, b, t1 + 6, t2),
                                                 "after": share(res, b, t2 + 8, dur - 1) if len(faults) > 1 else None}
        if faults[0]["backend"] and any(v is True for v in [faults[0].get("ok")]) and "down" in sc["faults"][0]["set"]:
            if sc["faults"][0]["set"]["down"]:
                obs["seconds_until_marked_down"] = first_time(res, b, False, t1)
            if len(faults) > 1 and sc["faults"][1]["set"].get("down") is False:
                obs["seconds_until_marked_up_after_restore"] = first_time(res, b, True, t2)
    else:
        obs["phases"] = {"base": phase_stats(res, 5, 15), "peak": phase_stats(res, 30, 50),
                         "end": phase_stats(res, dur - 12, dur - 2)}
    return obs


def run_scenario(name, args, bin_dir, out_dir):
    ldir = os.path.join(out_dir, name + ("_agent" if args.agent else f"_action{args.baseline}"))
    env_extra = {}
    if args.agent:
        env_extra = {"LB_AGENT_SETTLE_S": "5", "LB_AGENT_MEASURE_S": "10", "LB_AGENT_MIN_POLICY_AGE_S": "15",
                     "LB_AGENT_SAFETY_COOLDOWN_S": "20", "LB_AGENT_STEP_S": "1"}
    with Stack(bin_dir, ldir, agent=args.agent, env_extra=env_extra, safe_action=args.safe_action):
        if not args.agent and args.baseline is not None:
            http("POST", "http://localhost:8080/api/policy", {"action": args.baseline})
        out = os.path.join(ldir, "loadgen.json")
        p = subprocess.run([os.path.join(bin_dir, "loadgen" + EXE), "-scenario", os.path.join(ROOT, "scenarios", name + ".json"),
                            "-target", "http://localhost:8080", "-out", out, "-scale", str(args.scale)] +
                           (["-seed", str(args.seed)] if args.seed else []), capture_output=True, text=True, cwd=ROOT)
        print(p.stdout.strip(), p.stderr.strip())
        res = json.load(open(out, encoding="utf-8"))
        agent_status = None
        if args.agent:
            try:
                agent_status = http("GET", "http://localhost:8080/api/agent")
            except Exception:
                pass
    obs = analyze(name, res)
    if agent_status:
        obs["agent"] = {"stats": agent_status["stats"], "manager_reasons": agent_status["manager"]["reason_counts"],
                        "safety_events": agent_status["manager"]["safety_events"]}
    with open(os.path.join(ldir, "observations.json"), "w", encoding="utf-8") as f:
        json.dump(obs, f, indent=1)
    return obs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--scenarios", default="all", help="comma list or 'all'")
    ap.add_argument("--baseline", type=int, default=2, help="strategy action applied before load (ignored with --agent)")
    ap.add_argument("--safe-action", type=int, default=2)
    ap.add_argument("--agent", action="store_true", help="enable the Go<->Python AI loop")
    ap.add_argument("--scale", type=float, default=1.0)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--out", default=os.path.join(ROOT, "results", "phase13"))
    ap.add_argument("--no-build", action="store_true")
    ap.add_argument("--clean", action="store_true", help="delete the built binaries afterwards")
    args = ap.parse_args()
    bin_dir = os.path.join(args.out, "bin")
    if not args.no_build:
        build(bin_dir)
    names = ALL if args.scenarios == "all" else args.scenarios.split(",")
    allobs = []
    for n in names:
        print(f"=== {n} ===", flush=True)
        allobs.append(run_scenario(n, args, bin_dir, args.out))
    with open(os.path.join(args.out, "all_observations.json"), "w", encoding="utf-8") as f:
        json.dump(allobs, f, indent=1)
    if args.clean:
        shutil.rmtree(bin_dir, ignore_errors=True)


if __name__ == "__main__":
    main()
