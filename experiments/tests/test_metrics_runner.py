import json
import math
import os

import pytest

from experiments.analysis import hist as H
from experiments.analysis import metrics as M
from experiments.analysis.windows import build_window_rows
from experiments.generator import build_scenarios as G
from experiments.runner import matrix as MX
from experiments.runner.run import merge_decisions, resolve

CRIT = {"err_max": 0.01, "p95_factor": 2.0, "p95_margin_ms": 20.0, "stable_for_s": 4}
SCEN_DIR = os.path.join(os.path.dirname(__file__), "..", "scenarios")


def sec(t, p95=25.0, completed=100, errors=0):
    return {"t": t, "completed": completed, "errors": errors, "p95_ms": p95}


# --------------------------------------------------------------- adaptation / overload
def test_adaptation_time_hand_computed():
    s = [sec(t) for t in range(0, 12)]                       # pre-change: p95 25 -> limit max(50, 45) = 50
    s += [sec(t, p95=400) for t in range(12, 18)]            # violating 12..17
    s += [sec(t) for t in range(18, 36)]                     # stable again from 18
    r = M.adaptation(s, 8, {"at_s": 12, "end_s": 36}, CRIT)
    assert r["reference_p95_ms"] == pytest.approx(25.0)
    assert r["overload_s"] == 6 and r["adaptation_s"] == 6.0 and not r["censored"]


def test_adaptation_zero_when_never_degraded_and_censored_when_never_recovers():
    ok = [sec(t) for t in range(0, 36)]
    assert M.adaptation(ok, 8, {"at_s": 12, "end_s": 36}, CRIT)["adaptation_s"] == 0.0
    bad = [sec(t) for t in range(0, 12)] + [sec(t, errors=30) for t in range(12, 36)]  # 30% errors forever
    r = M.adaptation(bad, 8, {"at_s": 12, "end_s": 36}, CRIT)
    assert r["censored"] and math.isnan(r["adaptation_s"]) and r["overload_s"] == 24


def test_error_fraction_and_empty_seconds_count_as_violations():
    s = [sec(t) for t in range(0, 12)] + [sec(12, errors=2), sec(13, completed=0), sec(14)] + [sec(t) for t in range(15, 36)]
    r = M.adaptation(s, 8, {"at_s": 12, "end_s": 36}, CRIT)
    assert r["overload_s"] == 2  # 2% errors at t=12 (> 1%), nothing completed at t=13


# ------------------------------------------------------------------- imbalance / Jain
def lbs(t_end, totals, alive=True, switch=0, strat="s"):
    out = []
    for t in range(0, t_end + 1):
        out.append({"t": float(t), "strategy": strat, "policy_version": 1, "switch_count": switch if t < t_end else switch,
                    "overhead_p50_ms": 0.0, "overhead_p95_ms": 0.0,
                    "backends": {n: {"alive": alive, "active_conns": 0, "total_requests": int(r * t)} for n, r in totals.items()}})
    return out


def test_imbalance_uses_capacity_adjusted_rates_not_raw_counts():
    w = {"A": 3.0, "B": 2.0, "C": 1.0}
    perfect = M.imbalance(lbs(10, {"A": 300, "B": 200, "C": 100}), w, 0, 10)  # load proportional to capacity
    assert perfect["imbalance_cv"] == pytest.approx(0.0, abs=1e-9) and perfect["jain"] == pytest.approx(1.0)
    equal_counts = M.imbalance(lbs(10, {"A": 200, "B": 200, "C": 200}), w, 0, 10)  # RR on a 3:2:1 pool
    # x = (66.7, 100, 200): hand computed J = 366.7^2 / (3 * (4444+10000+40000)) = 0.8226
    assert equal_counts["jain"] == pytest.approx(0.8226, abs=1e-3)
    assert equal_counts["imbalance_cv"] > 0.4


def test_empty_or_partial_lb_samples_are_ignored_not_treated_as_zero():
    w = {"A": 3.0, "B": 2.0, "C": 1.0}
    s = lbs(10, {"A": 300, "B": 200, "C": 100})
    s[10]["backends"], s[10]["strategy"] = {}, ""          # last sample: status request timed out
    s[0]["backends"], s[0]["strategy"] = {}, ""            # first sample too
    r = M.imbalance(s, w, 0, 10)
    assert r["imbalance_cv"] == pytest.approx(0.0, abs=1e-9) and r["backends_in_fairness_set"] == 3
    assert M.switching(s, 0, 10)["switches_per_min"] == pytest.approx(0.0)


def test_imbalance_excludes_backends_that_were_down():
    w = {"A": 1.0, "B": 1.0, "C": 1.0}
    s = lbs(10, {"A": 100, "B": 100, "C": 100})
    for x in s[2:]:
        x["backends"]["C"]["alive"] = False
    r = M.imbalance(s, w, 0, 10)
    assert r["backends_in_fairness_set"] == 2


# ---------------------------------------------------------------- failure / recovery
def test_detect_and_reentry_times():
    w = {"A": 3.0, "B": 2.0, "C": 1.0}
    s = []
    tot = {"A": 0, "B": 0, "C": 0}
    for t in range(0, 41):
        alive_b = not (12 <= t < 14) and not (24 <= t < 26)         # marked down 2 s after the fault, up 2 s after restore
        rates = {"A": 300, "B": 200 if (t < 12 or t >= 27) else 0, "C": 100}  # share returns 3 s after restore
        for n in tot:
            tot[n] += rates[n]
        s.append({"t": float(t), "strategy": "x", "policy_version": 1, "switch_count": 0, "overhead_p50_ms": 0,
                  "overhead_p95_ms": 0, "backends": {n: {"alive": (alive_b if n == "B" else True), "active_conns": 0,
                                                         "total_requests": tot[n]} for n in tot}})
    # B is "down" in the sampler from t=12..25 inclusive in this fake series; rebuild explicit flags
    for x in s:
        x["backends"]["B"]["alive"] = not (12 <= x["t"] < 26 - 0) or x["t"] >= 26
        if 14 <= x["t"] < 26:
            x["backends"]["B"]["alive"] = False
        elif 12 <= x["t"] < 14:
            x["backends"]["B"]["alive"] = True
    faults = [{"backend": "B", "ok": True, "actual_s": 12.0}, {"backend": "B", "ok": True, "actual_s": 24.0}]
    r = M.failure_recovery(s, faults, {"failure_backend": "B"}, w)
    assert r["detect_s"] == pytest.approx(2.0)          # first sample with alive=False at t=14
    assert r["reentry_alive_s"] == pytest.approx(2.0)   # alive again at t=26
    assert r["reentry_share_s"] == pytest.approx(4.0, abs=1.0)  # share >= 80% of 1/3 for two samples from t=27/28


def test_switching_and_time_share():
    s = lbs(60, {"A": 1}, strat="a")
    for x in s[30:]:
        x["strategy"] = "b"
        x["switch_count"] = 3
    r = M.switching(s, 0, 60)
    assert r["switches_per_min"] == pytest.approx(3.0) and r["time_share_a"] == pytest.approx(30 / 61, abs=0.02)


# -------------------------------------------------------------- windows / latency export
def window(t0, ok, failed, lat_ms, offered=None):
    h = {}
    for v in lat_ms:
        h[str(H.index_of(v))] = h.get(str(H.index_of(v)), 0) + 1
    return {"t0": t0, "t1": t0 + 4, "offered": offered or ok + failed, "sent": ok + failed, "completed": ok + failed, "ok": ok,
            "failed": failed, "err_5xx": failed, "err_4xx": 0, "err_timeout": 0, "err_transport": 0,
            "lat_sum_ok_ms": float(sum(lat_ms)), "hist_ok": h, "long_completed": 0}


def test_latency_and_throughput_from_merged_histograms():
    wins = [window(8, 3, 1, [10, 10, 10]), window(12, 3, 1, [10, 10, 200])]
    r = M.latency_and_throughput(wins, 20.0)
    assert r["error_rate"] == pytest.approx(2 / 8) and r["ok_rps"] == pytest.approx(6 / 8)
    assert r["p50_ms"] == pytest.approx(10, rel=0.06) and r["p99_ms"] == pytest.approx(200, rel=0.06)
    assert r["lat_mean_ms"] == pytest.approx((30 + 220) / 6)
    assert r["offered_rps"] == pytest.approx(8 / 8)


def test_measurement_windows_exclude_warmup_and_drain():
    lg = {"windows": [window(t, 1, 0, [5]) for t in range(0, 48, 4)]}
    w = M.measurement_windows(lg, 8, 36)
    assert [x["t0"] for x in w][0] == 8 and [x["t0"] for x in w][-1] == 32


def test_build_window_rows_schema():
    lg = {"generator": {"start_unix": 1000.0, "window_s": 4}, "summary": {"duration_s": 8.0},
          "windows": [window(0, 10, 0, [20] * 10), window(4, 8, 2, [30] * 8)],
          "lb": [{"t": 0.5, "strategy": "Round-Robin", "policy_version": 1, "switch_count": 0, "overhead_p95_ms": 0.1,
                  "backends": {"A": {"alive": True, "active_conns": 0, "total_requests": 0}}},
                 {"t": 4.5, "strategy": "Round-Robin", "policy_version": 1, "switch_count": 0, "overhead_p95_ms": 0.1,
                  "backends": {"A": {"alive": True, "active_conns": 0, "total_requests": 40}}},
                 {"t": 7.5, "strategy": "Least-Connections", "policy_version": 2, "switch_count": 1, "overhead_p95_ms": 0.1,
                  "backends": {"A": {"alive": True, "active_conns": 0, "total_requests": 100}}}]}
    res = [{"t_unix": 1001.0, "proc": "lb", "cpu_pct": 12.0, "rss_mb": 30.0}]
    rows = build_window_rows(lg, res, ["A"])
    assert len(rows) == 2 and rows[0]["lb_cpu_pct"] == 12.0 and rows[0]["strategy"] == "Round-Robin"
    assert rows[1]["failed"] == 2 and rows[1]["strategy"] == "Least-Connections" and rows[1]["switch_count"] == 1
    assert rows[0]["p95_ms"] == pytest.approx(20, rel=0.06) and "A_rps" in rows[0]


# --------------------------------------------------------------------- matrix / runner
MATRIX = G.matrix()


def test_plan_is_balanced_deterministic_and_rounded():
    exps = ["E1", "E4", "E7"]
    plan = MX.build_plan(MATRIX, exps, reps=3, seed=5)
    assert plan == MX.build_plan(MATRIX, exps, reps=3, seed=5)           # deterministic for a seed
    assert plan != MX.build_plan(MATRIX, exps, reps=3, seed=6)           # but randomized across seeds
    per_round = len(exps) * 5
    assert len(plan) == 3 * per_round
    for r in range(3):                                                     # every condition once per round
        block = plan[r * per_round:(r + 1) * per_round]
        assert {(s.exp, s.arm) for s in block} == {(x, p) for x in exps for p in G.STANDARD_POLICIES}
        assert all(s.rep == r for s in block)


def test_consistent_hash_only_on_affinity_workload():
    for k, e in MATRIX["experiments"].items():
        has_ch = "ch" in e["policies"]
        assert has_ch == (k == "A1"), k


def test_e9_and_e10_conditions():
    c9 = MX.conditions(MATRIX, "E9")
    assert ("swrr", None) in c9 and {i for a, i in c9 if a == "ai"} == {1, 5, 10, 30, 60}
    assert {a for a, _ in MX.conditions(MATRIX, "E10")} == set(MATRIX["experiments"]["E10"]["arms"])
    assert all(a in MATRIX["ablation_arms"] for a, _ in MX.conditions(MATRIX, "E10"))


def test_resolve_ai_interval_and_ablation_env():
    r = resolve(MATRIX, "E9", "ai", 30)
    assert r["agent_env"]["LB_AGENT_MIN_POLICY_AGE_S"] == "30" and r["agent_env"]["LB_AGENT_SETTLE_S"] == "10.0"
    assert float(r["agent_env"]["LB_AGENT_MEASURE_S"]) == pytest.approx(20.0)
    nh = resolve(MATRIX, "E10", "ai_no_hysteresis", None)
    assert nh["agent_env"]["LB_AGENT_MIN_POLICY_AGE_S"] == "0" and nh["agent_env"]["LB_AGENT_SWITCH_MARGIN"] == "0"
    assert resolve(MATRIX, "E10", "ai_no_context", None)["agent"]["algo"] == "mean-eps-greedy"
    assert resolve(MATRIX, "E10", "ai_no_safety", None)["agent_env"]["LB_AGENT_MAX_ERROR_RATE"] == "2"
    assert resolve(MATRIX, "E1", "rr", None)["agent"] is None
    assert resolve(MATRIX, "A1", "ch", None)["affinity"] is True


def test_scenarios_match_generator_and_are_internally_consistent():
    for key, sc in G.experiments().items():
        with open(os.path.join(SCEN_DIR, key + ".json"), encoding="utf-8") as f:
            assert json.load(f) == sc, f"{key}.json is stale: re-run python -m experiments.generator.build_scenarios"
    with open(os.path.join(SCEN_DIR, "experiments.json"), encoding="utf-8") as f:
        assert json.load(f) == MATRIX
    for k, e in MATRIX["experiments"].items():
        sc = G.experiments()[e["scenario"].replace(".json", "")]
        names = {b["name"] for b in MATRIX["pools"][e["pool"]]}
        ports = [b["port"] for b in MATRIX["pools"][e["pool"]]]
        assert len(set(ports)) == len(ports)
        for f in sc["faults"]:
            assert f["backend"] in names and f["at_s"] >= MATRIX["warmup_s"], (k, f)  # faults only after warm-up
        assert e["duration_s"] == sum(p["duration_s"] for p in sc["phases"])
        assert MATRIX["warmup_s"] % MATRIX["window_s"] == 0
        ch = e.get("change")
        if ch:
            assert ch["at_s"] >= MATRIX["warmup_s"] and ch["end_s"] <= e["duration_s"] + 1e-9


def test_merge_decisions_joins_reward_components(tmp_path):
    go, lr = tmp_path / "go.jsonl", tmp_path / "lr.jsonl"
    go.write_text("\n".join(json.dumps(x) for x in [
        {"decision_id": "", "reason": "held_stale_or_invalid_state", "applied": False, "mode": "live", "state_version": 1},
        {"decision_id": "w1", "proposed_strategy": "least_connections", "reason": "applied", "applied": True,
         "model_version": "m", "mode": "live", "state_version": 1, "latency_ms": 7.0},
        {"decision_id": "w2", "proposed_strategy": "round_robin", "reason": "rejected_min_policy_age", "applied": False,
         "mode": "live", "state_version": 1}]), encoding="utf-8")
    lr.write_text("\n".join(json.dumps(x) for x in [
        {"event": "decision", "decision_id": "w1", "action": 1, "scores": [0.1, 0.2, None, None, 0.1], "explored": True},
        {"event": "outcome", "target": "w1", "reward": -0.12, "breakdown": {"contributions": {"latency": -0.1},
                                                                           "override": None, "raw": {"p95_ms": 50}, "normalized": {}}},
        {"event": "outcome_ignored", "target": "w2", "reason": "exposure not clean"}]), encoding="utf-8")
    out = tmp_path / "decisions.jsonl"
    assert merge_decisions(str(go), str(lr), str(out)) == 3
    rows = [json.loads(l) for l in out.read_text().splitlines()]
    assert rows[1]["accepted"] and rows[1]["reward"] == -0.12 and rows[1]["reward_components"] == {"latency": -0.1}
    assert rows[1]["learner_action"] == 1 and rows[1]["explored"] is True and rows[1]["state_version"] == 1
    assert not rows[2]["accepted"] and rows[2]["policy_decision"] == "rejected_min_policy_age"
    assert rows[2]["outcome_ignored"] == "exposure not clean"
