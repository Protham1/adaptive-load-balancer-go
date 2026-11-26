"""Generate the experimental matrix (Phase 15): loadgen scenario files + experiments.json.

    python -m experiments.generator.build_scenarios

experiments.json is the single source of truth read by the runner and the analysis:
backend pools, policies per experiment, the injected change (for adaptation time),
and the pre-defined acceptance criterion. Scenario files are plain `cmd/loadgen`
inputs (strictly parsed), so every run is reproducible from a scenario file and a
Git commit.
"""
from __future__ import annotations

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(os.path.dirname(HERE), "scenarios")

WINDOW_S = 4  # export window length; warm-up is a whole number of windows
WARMUP_S = 8  # first 8 s of every run are excluded from all metrics (identical for all policies)
BASE_SEED = 1000  # run seed = BASE_SEED + repetition

# Backend pools. conc x 1/service_time = capacity: hetero ~ 1180/790/390 rps (3:2:1).
POOLS = {
    "hetero": [
        {"name": "Backend-1", "port": 8081, "conc": 24, "weight": 3, "gomaxprocs": 0, "cpu_work_us": 300},
        {"name": "Backend-2", "port": 8082, "conc": 16, "weight": 2, "gomaxprocs": 0, "cpu_work_us": 300},
        {"name": "Backend-3", "port": 8083, "conc": 8, "weight": 1, "gomaxprocs": 0, "cpu_work_us": 300},
    ],
    "balanced": [
        {"name": f"Backend-{i}", "port": 8080 + i, "conc": 16, "weight": 1, "gomaxprocs": 0, "cpu_work_us": 300}
        for i in (1, 2, 3)
    ],
    # CPU-bound variant: 2 OS threads per backend and 1.5 ms of real CPU work per request,
    # so a CPU burner inside the backend process genuinely slows request handling.
    "hetero_cpu": [
        {"name": "Backend-1", "port": 8081, "conc": 24, "weight": 3, "gomaxprocs": 2, "cpu_work_us": 1500},
        {"name": "Backend-2", "port": 8082, "conc": 16, "weight": 2, "gomaxprocs": 2, "cpu_work_us": 1500},
        {"name": "Backend-3", "port": 8083, "conc": 8, "weight": 1, "gomaxprocs": 2, "cpu_work_us": 1500},
    ],
}

STANDARD_POLICIES = ["rr", "lc", "swrr", "rule", "ai"]  # Consistent Hash only on affinity workloads
AFFINITY_POLICIES = ["rr", "lc", "swrr", "ch", "rule", "ai"]

# Pre-defined acceptance criterion for adaptation time (per 1-s generator bucket, post-change):
# error fraction <= err_max AND p95 <= max(p95_factor * reference_p95, reference_p95 + p95_margin_ms),
# where reference_p95 is the run's own median per-second p95 between warm-up and the change.
CRITERIA = {"err_max": 0.01, "p95_factor": 2.0, "p95_margin_ms": 20.0, "stable_for_s": 4}


def fixed(d, rps):
    return {"type": "fixed", "duration_s": d, "rps": rps}


def ramp(d, a, b):
    return {"type": "ramp", "duration_s": d, "from_rps": a, "to_rps": b}


def fault(at, backend, **s):
    return {"at_s": at, "backend": backend, "set": s}


def scenario(name, desc, phases, faults=(), mix=None, **extra):
    d = {"name": name, "description": desc, "seed": BASE_SEED, "request_path": "/work", "arrivals": "poisson",
         "phases": phases, "mix": mix or {"long_fraction": 0.0, "long_delay_ms": 0, "short_delay_ms": 0},
         "faults": list(faults), "timeout_ms": 5000, "max_in_flight": 5000}
    d.update(extra)
    return d


def mixed(k=1.0):
    """E8/E9 mixed workload: ramping load + 10 % long requests + CPU pressure + added latency + failure/recovery."""
    t = lambda x: round(x * k, 3)
    return scenario(
        "mixed" if k == 1.0 else "mixed_long",
        "Ramp 400->800 rps with 10% long requests; CPU burner on Backend-1, +100 ms on Backend-3, "
        "Backend-2 down then restored.",
        [fixed(t(12), 400), ramp(t(8), 400, 800), fixed(t(24), 800)],
        [fault(t(14), "Backend-1", cpu_burn=0.9, cpu_threads=2),
         fault(t(20), "Backend-3", delay_ms=100),
         fault(t(28), "Backend-2", down=True),
         fault(t(36), "Backend-2", down=False)],
        mix={"long_fraction": 0.1, "long_delay_ms": 300, "short_delay_ms": 0})


def experiments():
    D = 36
    sc = {}
    sc["e1_balanced"] = scenario("e1_balanced", "Equal backends, stable short requests, 300 rps.", [fixed(D, 300)])
    sc["e2_heterogeneous"] = scenario("e2_heterogeneous", "3:2:1 capacity pool at 1300 rps (RR would give each backend 433 rps; "
                                      "the smallest has ~390 rps capacity).", [fixed(D, 1300)])
    sc["e3_cpu_imbalance"] = scenario("e3_cpu_imbalance", "CPU-bound backends; sustained CPU burner (2 threads, 90% duty) "
                                      "inside Backend-1 from t=12 s.", [fixed(D, 600)],
                                      [fault(12, "Backend-1", cpu_burn=0.9, cpu_threads=2)])
    sc["e4_latency_imbalance"] = scenario("e4_latency_imbalance", "Backend-2 gains +150 ms from t=12 s (sustained).",
                                          [fixed(D, 600)], [fault(12, "Backend-2", delay_ms=150)])
    sc["e5_long_requests"] = scenario("e5_long_requests", "20% of requests hold a connection for 400 ms; 400 rps.",
                                      [fixed(D, 400)], mix={"long_fraction": 0.2, "long_delay_ms": 400, "short_delay_ms": 0})
    sc["e6_flash_crowd"] = scenario("e6_flash_crowd", "200 rps, ramp to 1400 rps over 8 s from t=10 s, hold 10 s, ramp down.",
                                    [fixed(10, 200), ramp(8, 200, 1400), fixed(10, 1400), ramp(8, 1400, 200)])
    sc["e7_failure_recovery"] = scenario("e7_failure_recovery", "Backend-2 down at t=12 s, restored at t=24 s; 600 rps.",
                                         [fixed(D, 600)], [fault(12, "Backend-2", down=True), fault(24, "Backend-2", down=False)])
    sc["e8_mixed"] = mixed(1.0)
    sc["e9_mixed_long"] = mixed(1.75)
    sc["a1_affinity"] = scenario("a1_affinity", "Equal backends, 400 rps from 200 synthetic clients; affinity (stickiness) matters.",
                                 [fixed(D, 400)], mix={"long_fraction": 0.0, "long_delay_ms": 0, "short_delay_ms": 0, "clients": 200},
                                 track_backend=True)
    return sc


def matrix():
    def dur(sc):
        return sum(p["duration_s"] for p in sc["phases"])
    scs = experiments()
    def ex(key, title, question, pool, policies=STANDARD_POLICIES, change=None, **extra):
        sc = scs[key]
        e = {"title": title, "question": question, "scenario": key + ".json", "pool": pool,
             "duration_s": dur(sc), "policies": list(policies), "env": {}, "change": change}
        e.update(extra)
        return e
    E = {
        "E1": ex("e1_balanced", "Balanced baseline", "Does AI avoid unnecessary overhead or switching?", "balanced"),
        "E2": ex("e2_heterogeneous", "Heterogeneous capacity", "Does adaptation use capacity better than RR?", "hetero"),
        "E3": ex("e3_cpu_imbalance", "CPU imbalance", "Does the controller reduce overload duration?", "hetero_cpu",
                 change={"at_s": 12, "end_s": 36}),
        "E4": ex("e4_latency_imbalance", "Latency imbalance", "Does the chosen policy reduce tail latency?", "hetero",
                 change={"at_s": 12, "end_s": 36}),
        "E5": ex("e5_long_requests", "Long-running requests", "Does LC or another strategy perform better?", "hetero"),
        "E6": ex("e6_flash_crowd", "Flash crowd", "How quickly does the system adapt?", "hetero",
                 change={"at_s": 10, "end_s": 28}),
        "E7": ex("e7_failure_recovery", "Failure / recovery", "What are detection and recovery times?", "hetero",
                 change={"at_s": 12, "end_s": 24, "failure_backend": "Backend-2", "restore_at_s": 24}),
        "E8": ex("e8_mixed", "Mixed workload", "Does learned selection generalize?", "hetero_cpu",
                 change={"at_s": 14, "end_s": 44}),
        "E9": ex("e9_mixed_long", "Control-interval sweep", "Tradeoff between responsiveness and instability?", "hetero_cpu",
                 policies=["swrr", "ai"], change={"at_s": 24.5, "end_s": 77}, intervals_s=[1, 5, 10, 30, 60]),
        "E10": ex("e8_mixed", "Ablation", "Which components contribute?", "hetero_cpu", policies=[],
                  change={"at_s": 14, "end_s": 44},
                  arms=["ai_full", "ai_no_hysteresis", "ai_no_safety", "ai_no_context", "ai_no_ema",
                        "rule_full", "rule_no_ema", "rule_no_deadband"]),
        "A1": ex("a1_affinity", "Affinity workload (Consistent Hash)", "Is session affinity preserved, and at what cost?",
                 "balanced", policies=AFFINITY_POLICIES, affinity=True),
    }
    return {
        "version": 1, "window_s": WINDOW_S, "warmup_s": WARMUP_S, "base_seed": BASE_SEED, "criteria": CRITERIA,
        "pools": POOLS, "experiments": E,
        "policies": {
            "rr": "Round Robin (action 0)", "lc": "Least Connections (action 1)",
            "swrr": "Static Smooth Weighted Round Robin (action 2, declared capacity weights)",
            "ch": "Consistent Hash (action 3; affinity workload only)",
            "rule": "Rule-based adaptive controller (action 4)",
            "ai": "AI contextual bandit (LinUCB) with the Phase 11-12 policy manager and safety layer",
        },
        "ai_defaults": {"algo": "linucb", "alpha": 0.05, "context": "compact", "settle_s": 2, "measure_s": 4,
                        "min_policy_age_s": 6, "safety_cooldown_s": 12, "step_s": 1},
        "ablation_arms": {
            "ai_full": {"policy": "ai"},
            "ai_no_hysteresis": {"policy": "ai", "env": {"LB_AGENT_MIN_POLICY_AGE_S": "0", "LB_AGENT_SWITCH_MARGIN": "0"}},
            "ai_no_safety": {"policy": "ai", "env": {"LB_AGENT_MAX_ERROR_RATE": "2", "LB_AGENT_MAX_P99_MS": "1000000000"}},
            "ai_no_context": {"policy": "ai", "agent_algo": "mean-eps-greedy"},
            "ai_no_ema": {"policy": "ai", "env": {"LB_CONTROLLER_EMA_ALPHA": "1"}},
            "rule_full": {"policy": "rule"},
            "rule_no_ema": {"policy": "rule", "env": {"LB_CONTROLLER_EMA_ALPHA": "1"}},
            "rule_no_deadband": {"policy": "rule", "env": {"LB_CONTROLLER_DEADBAND": "0"}},
        },
    }


def main():
    os.makedirs(OUT, exist_ok=True)
    for key, sc in experiments().items():
        with open(os.path.join(OUT, key + ".json"), "w", encoding="utf-8", newline="\n") as f:
            json.dump(sc, f, indent=1)
            f.write("\n")
    with open(os.path.join(OUT, "experiments.json"), "w", encoding="utf-8", newline="\n") as f:
        json.dump(matrix(), f, indent=1)
        f.write("\n")
    print(f"wrote {len(experiments())} scenarios + experiments.json to {OUT}")


if __name__ == "__main__":
    main()
