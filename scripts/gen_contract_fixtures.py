"""Generate the shared Go/Python contract fixtures in testdata/contract/.

Both sides load every file here:
  valid/request_*.json   must validate        valid/response_*.json   must validate (against request_basic)
  invalid/request_*.json must be rejected     invalid/response_*.json must be rejected (against request_basic)
Files may contain literal NaN/Infinity (invalid JSON) on purpose.
"""
import copy
import json
import os

ROOT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "testdata", "contract")


def backend(i, **kw):
    b = {"id": f"Backend-{i}", "healthy": 1.0, "cpu_utilization": 0.4, "memory_utilization": 0.3,
         "active_connections_normalized": 0.1 * i, "backend_rps_normalized": 0.2,
         "p95_latency_normalized": 0.05, "error_rate": 0.0, "capacity_ratio": [0.5, 0.33, 0.17][i - 1]}
    b.update(kw)
    return b


REQ = {
    "schema_version": 1,
    "decision_id": "window-00042",
    "state_version": 1,
    "observed_at": "2026-10-13T16:00:00Z",
    "window_seconds": 30,
    "current_strategy": "round_robin",
    "allowed_strategies": ["round_robin", "least_connections", "static_swrr", "rule_based_adaptive"],
    "features": {
        "global": {"rps_normalized": 0.4, "request_duration_p95_normalized": 0.1,
                   "long_request_ratio": 0.0, "global_error_rate": 0.0},
        "backends": [backend(1), backend(2), backend(3)],
    },
}
OUTCOME = {"decision_id": "window-00041", "window_seconds": 20, "requests": 2400, "errors": 12,
           "p95_ms": 85.5, "clean_exposure": True,
           "backends": [{"id": "Backend-1", "requests": 1200, "healthy": True, "weight": 3},
                        {"id": "Backend-2", "requests": 800, "healthy": True, "weight": 2},
                        {"id": "Backend-3", "requests": 400, "healthy": True, "weight": 1}]}
RESP = {"schema_version": 1, "decision_id": "window-00042", "strategy": "least_connections",
        "model_version": "linucb-v1+12u", "diagnostic": "ok"}


def mut(base, fn):
    d = copy.deepcopy(base)
    fn(d)
    return d


def w(sub, name, obj, raw=None):
    d = os.path.join(ROOT, sub)
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, name + ".json"), "w", encoding="utf-8", newline="\n") as f:
        f.write(raw if raw is not None else json.dumps(obj, indent=1) + "\n")


def main():
    w("valid", "request_basic", REQ)
    w("valid", "request_with_outcome", mut(REQ, lambda d: d.update(previous_outcome=OUTCOME)))
    w("valid", "request_single_backend", mut(REQ, lambda d: d["features"].update(backends=[backend(1)])))
    w("valid", "response_basic", RESP)
    w("valid", "response_with_weights_scores", mut(RESP, lambda d: d.update(
        strategy="rule_based_adaptive", weights={"Backend-1": 0.5, "Backend-2": 0.3, "Backend-3": 0.2},
        scores=[0.1, 0.2, None, None, 0.3], explored=True)))

    bad_req = {
        "schema_version_2": lambda d: d.update(schema_version=2),
        "state_version_2": lambda d: d.update(state_version=2),
        "decision_id_spaces": lambda d: d.update(decision_id="window 42"),
        "decision_id_too_long": lambda d: d.update(decision_id="x" * 65),
        "decision_id_empty": lambda d: d.update(decision_id=""),
        "unknown_current_strategy": lambda d: d.update(current_strategy="random_walk"),
        "unknown_allowed_strategy": lambda d: d.update(allowed_strategies=["round_robin", "magic"]),
        "empty_allowed": lambda d: d.update(allowed_strategies=[]),
        "duplicate_allowed": lambda d: d.update(allowed_strategies=["round_robin", "round_robin"]),
        "window_zero": lambda d: d.update(window_seconds=0),
        "window_huge": lambda d: d.update(window_seconds=99999),
        "missing_observed_at": lambda d: d.pop("observed_at"),
        "bad_observed_at": lambda d: d.update(observed_at="yesterday"),
        "feature_above_one": lambda d: d["features"]["global"].update(rps_normalized=1.5),
        "feature_negative": lambda d: d["features"]["backends"][0].update(cpu_utilization=-0.1),
        "feature_string": lambda d: d["features"]["backends"][0].update(error_rate="0.1"),
        "backends_unsorted": lambda d: d["features"]["backends"].reverse(),
        "backends_duplicate": lambda d: d["features"]["backends"].__setitem__(1, backend(1)),
        "backends_empty": lambda d: d["features"].update(backends=[]),
        "backends_too_many": lambda d: d["features"].update(
            backends=[backend(1, id=f"B-{i:03d}") for i in range(65)]),
        "backend_missing_feature": lambda d: d["features"]["backends"][0].pop("capacity_ratio"),
        "unknown_field": lambda d: d.update(extra="x"),
        "outcome_errors_exceed_requests": lambda d: d.update(previous_outcome=dict(OUTCOME, errors=99999)),
        "outcome_negative_p95": lambda d: d.update(previous_outcome=dict(OUTCOME, p95_ms=-1)),
    }
    for name, fn in bad_req.items():
        w("invalid", f"request_{name}", mut(REQ, fn))
    # literal NaN / Infinity are not JSON and must never be accepted
    raw = json.dumps(REQ, indent=1).replace('"rps_normalized": 0.4', '"rps_normalized": NaN')
    w("invalid", "request_nan", None, raw)
    raw = json.dumps(REQ, indent=1).replace('"window_seconds": 30', '"window_seconds": Infinity')
    w("invalid", "request_infinity", None, raw)

    bad_resp = {
        "unknown_strategy": lambda d: d.update(strategy="least_connection"),
        "display_name_not_wire_name": lambda d: d.update(strategy="Least-Connections"),
        "stale_decision_id": lambda d: d.update(decision_id="window-00041"),
        "missing_model_version": lambda d: d.pop("model_version"),
        "empty_diagnostic": lambda d: d.update(diagnostic=""),
        "weights_above_one": lambda d: d.update(weights={"Backend-1": 2.0}),
        "weights_negative": lambda d: d.update(weights={"Backend-1": -0.1}),
        "weights_bad_id": lambda d: d.update(weights={"bad id": 0.5}),
        "scores_wrong_length": lambda d: d.update(scores=[0.1, 0.2]),
        "unknown_field": lambda d: d.update(note="hi"),
        "schema_version_2": lambda d: d.update(schema_version=2),
    }
    for name, fn in bad_resp.items():
        w("invalid", f"response_{name}", mut(RESP, fn))
    raw = json.dumps(RESP, indent=1).replace('"diagnostic": "ok"', '"diagnostic": "ok", "scores": [NaN, 0, 0, 0, 0]')
    w("invalid", "response_nan_score", None, raw)
    w("invalid", "response_not_json", None, "not json at all")
    w("invalid", "response_trailing_data", None, json.dumps(RESP) + '{"x":1}')


if __name__ == "__main__":
    main()
