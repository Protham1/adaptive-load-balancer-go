"""Live checks of the Phase 11-12 reliability requirements against the real stack.

  python scripts/e2e_checks.py          (builds binaries, starts backends + LB, no Python agent)

  A. agent unreachable      -> requests fail fast, failures are counted, last valid policy retained
  B. manual kill switch     -> baseline applied, AI decisions stop, resume works
  C. invalid baseline       -> kill switch refuses an action that is not valid
  D. safe baseline at start -> policy is the configured known-safe strategy before traffic
"""
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from run_scenarios import ROOT, Stack, build, http  # noqa: E402

LB = "http://localhost:8080"


def main() -> int:
    bin_dir = os.path.join(ROOT, "results", "phase13", "bin")
    build(bin_dir)
    fails = []

    def check(name, ok, detail=""):
        print(("PASS " if ok else "FAIL ") + name + (f" - {detail}" if detail else ""))
        if not ok:
            fails.append(name)

    env = {"LB_AGENT_URL": "http://127.0.0.1:1", "LB_AGENT_STEP_S": "1", "LB_AGENT_MIN_POLICY_AGE_S": "1",
           "LB_AGENT_TIMEOUT_MS": "500"}
    with Stack(bin_dir, os.path.join(ROOT, "results", "phase13", "e2e_checks"), agent=False, env_extra=env,
               safe_action=2):
        st = http("GET", LB + "/api/status")
        check("D. safe baseline active at startup", st["active_strategy"] == "Smooth-Weighted-Round-Robin",
              st["active_strategy"])
        pol = http("GET", LB + "/api/policy")["policy"]
        check("D. startup reason recorded", pol.get("reason") == "startup_safe_baseline", str(pol.get("reason")))
        # generate a little traffic so the state is valid and the loop asks the agent
        for _ in range(60):
            try:
                http("GET", LB + "/work", timeout=2)
            except Exception:
                pass
        time.sleep(14)
        for _ in range(60):
            try:
                http("GET", LB + "/work", timeout=2)
            except Exception:
                pass
        time.sleep(4)
        ag = http("GET", LB + "/api/agent")
        fails_by_kind = ag["stats"]["failures_by_kind"]
        check("A. agent outage counted as connect failure", fails_by_kind.get("connect", 0) >= 1, str(fails_by_kind))
        check("A. last valid policy retained", ag["policy"]["strategy"] == "Smooth-Weighted-Round-Robin",
              ag["policy"]["strategy"])
        check("A. failures logged in recent decisions", any(r["reason"] == "agent_unavailable" for r in ag["recent_decisions"]))
        check("A. backoff limits retries", ag["stats"]["requests"] <= 4, f"{ag['stats']['requests']} requests")

        r = http("POST", LB + "/api/agent/kill", {"kill": True, "baseline_action": 0})
        check("B. kill switch applies selected baseline", r["policy"]["strategy"] == "Round-Robin" and r["killed"])
        n0 = http("GET", LB + "/api/agent")["stats"]["requests"]
        time.sleep(13)
        ag2 = http("GET", LB + "/api/agent")
        check("B. no agent requests while killed", ag2["stats"]["requests"] == n0, f"{n0} -> {ag2['stats']['requests']}")
        check("B. kill event recorded", any(e["kind"] == "kill" for e in ag2["manager"]["safety_events"]))
        r = http("POST", LB + "/api/agent/kill", {"kill": False})
        check("B. resume", r["killed"] is False)
        try:
            http("POST", LB + "/api/agent/kill", {"kill": True, "baseline_action": 3})
            check("C. invalid baseline refused", False, "accepted")
        except Exception as e:
            check("C. invalid baseline refused", "400" in str(e), str(e))
    print("ALL PASSED" if not fails else f"FAILED: {fails}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
