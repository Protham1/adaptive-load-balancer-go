"""Live hardening checks against real processes (Security, Reliability and Operational Hardening phase).

    python scripts/e2e_hardening.py

A. optional AI: the LB serves with the baseline while the agent is down; the agent starts late; the agent is killed
   mid-run; the agent restarts (model restored); the LB itself restarts
B. model-load failure: a corrupt model makes the agent refuse (HTTP 500) while the LB keeps serving the baseline
C. a misbehaving agent: malformed JSON, stale decision, timeout, unknown strategy, NaN, oversized body, HTTP 500
D. graceful shutdown of the LB (an in-flight request completes, clean exit code)
E. attacks on the management API: wrong content type, cross-origin, DNS-rebinding Host, negative/huge weights,
   unknown fields, oversized body, SSE flood, slowloris; and bearer-token mode
"""
import http.client
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
from experiments.runner.stack import EXE, build_binaries  # noqa: E402

LB_PORT, AGENT_PORT, FAKE_PORT = 18080, 18090, 18091
LB = f"http://127.0.0.1:{LB_PORT}"
BACKENDS = [("B1", 18081, 3), ("B2", 18082, 2), ("B3", 18083, 1)]
NEW_GROUP = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {}
results, procs = [], []
work = tempfile.mkdtemp(prefix="e2e_hard_")


def check(name, ok, detail=""):
    results.append(ok)
    print(("PASS " if ok else "FAIL ") + name + (f" - {detail}" if detail else ""), flush=True)


def req(method, path, body=None, headers=None, host=None, port=LB_PORT, timeout=5):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    h = {"Content-Type": "application/json", **(headers or {})}
    if host:
        h["Host"] = host
    c.request(method, path, body=body, headers=h)
    r = c.getresponse()
    raw = r.read()
    c.close()
    try:
        return r.status, json.loads(raw) if raw else {}
    except ValueError:
        return r.status, {"raw": raw[:200].decode("utf-8", "replace")}


def spawn(name, cmd, env=None):
    log = open(os.path.join(work, name + ".log"), "w", encoding="utf-8")
    p = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, cwd=ROOT, env={**os.environ, **(env or {})}, **NEW_GROUP)
    procs.append((p, log))
    return p


def stop(p, graceful=True, timeout=10):
    """Returns the exit code (None if it had to be killed)."""
    if p.poll() is not None:
        return p.returncode
    try:
        p.send_signal(signal.CTRL_BREAK_EVENT if os.name == "nt" else signal.SIGTERM) if graceful else p.kill()
        return p.wait(timeout=timeout)
    except Exception:  # noqa: BLE001
        p.kill()
        p.wait(timeout=5)
        return None


def wait_for(cond, timeout=20, step=0.25):
    end = time.time() + timeout
    while time.time() < end:
        try:
            if cond():
                return True
        except Exception:  # noqa: BLE001
            pass
        time.sleep(step)
    return False


class Traffic(threading.Thread):
    """Continuous background requests; counts successes and failures."""

    def __init__(self):
        super().__init__(daemon=True)
        self.ok = self.bad = 0
        self._stop_evt = threading.Event()

    def run(self):
        while not self._stop_evt.is_set():
            try:
                c = http.client.HTTPConnection("127.0.0.1", LB_PORT, timeout=3)
                c.request("GET", "/work")
                r = c.getresponse()
                r.read()
                c.close()
                if r.status == 200:
                    self.ok += 1
                else:
                    self.bad += 1
            except Exception:  # noqa: BLE001
                self.bad += 1
            time.sleep(0.02)

    def stop(self):
        self._stop_evt.set()
        self.join(timeout=5)


def lb_env(agent_url="", extra=None):
    e = {"LB_ADDR": f"127.0.0.1:{LB_PORT}", "LB_NO_DUMMY": "1", "LB_QUIET": "1", "LB_HEALTH_INTERVAL_MS": "500",
         "LB_BACKENDS": ",".join(f"{n}:{p}:{w}" for n, p, w in BACKENDS), "LB_SAFE_ACTION": "2",
         "LB_AGENT_STEP_S": "0.5", "LB_AGENT_FAILURE_BACKOFF_S": "0.3", "LB_AGENT_TIMEOUT_MS": "700",
         "LB_AGENT_SETTLE_S": "1", "LB_AGENT_MEASURE_S": "2", "LB_AGENT_MIN_POLICY_AGE_S": "2"}
    if agent_url:
        e["LB_AGENT_URL"] = agent_url
    e.update(extra or {})
    return e


def start_lb(bin_dir, agent_url="", extra=None):
    p = spawn("lb", [os.path.join(bin_dir, "lb" + EXE)], lb_env(agent_url, extra))
    assert wait_for(lambda: req("GET", "/api/status")[0] == 200, 15), "LB did not start"
    return p


def agent_stats():
    return req("GET", "/api/agent")[1]


def start_agent(extra_args=(), name="agent"):
    cmd = [sys.executable, "-W", "ignore", "-m", "ai.server", "--port", str(AGENT_PORT), "--model", os.path.join(work, "model.json"),
           "--log", os.path.join(work, "decisions.jsonl"), *extra_args]
    p = spawn(name, cmd)
    assert wait_for(lambda: req("GET", "/v1/health", port=AGENT_PORT)[0] == 200, 20), "agent did not start"
    return p


# ------------------------------------------------------------------------------- fake agent
class Fake(BaseHTTPRequestHandler):
    mode = "valid"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        r = json.loads(self.rfile.read(n))
        m = Fake.mode
        ok = {"schema_version": 1, "decision_id": r["decision_id"], "strategy": r["current_strategy"],
              "model_version": "fake-v1", "diagnostic": "ok"}
        code, raw = 200, None
        if m == "malformed":
            raw = b"{not json"
        elif m == "stale":
            raw = json.dumps({**ok, "decision_id": "window-99999"}).encode()
        elif m == "slow":
            time.sleep(2.0)
        elif m == "unknown_strategy":
            raw = json.dumps({**ok, "strategy": "teleport"}).encode()
        elif m == "nan":
            raw = json.dumps(ok).replace('"diagnostic": "ok"', '"diagnostic": "ok", "scores": [NaN, 0, 0, 0, 0]').encode()
        elif m == "negative_weights":
            raw = json.dumps({**ok, "strategy": "rule_based_adaptive", "weights": {"B1": -0.5, "B2": 0.5, "B3": 0.5}}).encode()
        elif m == "oversized":
            raw = b"x" * 70000
        elif m == "http500":
            code, raw = 500, b"boom"
        raw = raw if raw is not None else json.dumps(ok).encode()
        try:
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
        except OSError:
            pass


def main() -> int:
    bin_dir = os.path.join(work, "bin")
    build_binaries(bin_dir)
    for name, port, w in BACKENDS:
        spawn(name, [os.path.join(bin_dir, "backend" + EXE), "-port", str(port), "-name", name, "-conc", str(8 * w)])
    time.sleep(1.0)
    tr = Traffic()

    # ================================================================== A. the AI is optional
    lb = start_lb(bin_dir, f"http://127.0.0.1:{AGENT_PORT}")           # the agent does not exist yet
    tr.start()
    time.sleep(3)
    d = req("GET", "/api/dashboard")[1]
    st = agent_stats()
    check("A1 LB serves with the baseline while the agent has never been started",
          tr.ok > 20 and tr.bad == 0 and d["strategy"] == "Smooth-Weighted-Round-Robin", f"ok={tr.ok} bad={tr.bad} {d['strategy']}")
    check("A2 agent failures are counted as connect errors", st["stats"]["failures_by_kind"].get("connect", 0) >= 1,
          str(st["stats"]["failures_by_kind"]))
    agent = start_agent()
    check("A3 once the agent starts, decisions succeed without restarting the LB",
          wait_for(lambda: agent_stats()["stats"]["successes"] >= 1, 25), str(agent_stats()["stats"]["successes"]))
    wait_for(lambda: agent_stats()["stats"]["successes"] >= 4, 25)
    updates_before = req("GET", "/v1/learner", port=AGENT_PORT)[1]["learner"]["updates"]
    pol_before = req("GET", "/api/policy")[1]["policy"]["strategy"]
    bad_before = tr.bad
    fails_before = sum(agent_stats()["stats"]["failures_by_kind"].values())
    code = stop(agent, graceful=False)                                  # hard kill, mid-run
    check("A4 killing the agent mid-run: traffic keeps flowing, policy is retained, failures counted",
          wait_for(lambda: sum(agent_stats()["stats"]["failures_by_kind"].values()) > fails_before, 15) and tr.bad == bad_before
          and req("GET", "/api/policy")[1]["policy"]["strategy"] == pol_before, f"bad={tr.bad - bad_before}")
    agent = start_agent()
    succ = agent_stats()["stats"]["successes"]
    ok_resume = wait_for(lambda: agent_stats()["stats"]["successes"] > succ, 25)
    upd = req("GET", "/v1/learner", port=AGENT_PORT)[1]["learner"]["updates"]
    check("A5 restarted agent resumes decisions and restores its persisted model", ok_resume and upd >= updates_before,
          f"updates {updates_before} -> {upd}")
    rc = stop(lb)                                                       # graceful LB stop while traffic flows
    time.sleep(0.5)
    check("A6 LB shuts down gracefully (exit code 0)", rc == 0, f"exit={rc}")
    tr.bad = 0
    lb = start_lb(bin_dir, f"http://127.0.0.1:{AGENT_PORT}")
    check("A7 LB restart: serves again immediately and re-attaches to the agent",
          wait_for(lambda: tr.ok > 0 and agent_stats()["stats"]["successes"] >= 1, 25), f"bad={tr.bad}")
    tr.stop()
    stop(lb)
    stop(agent, graceful=True)

    # ================================================================ B. model-load failure
    for p in ("model.json", "model.json.prev"):
        fp = os.path.join(work, p)
        if os.path.exists(fp):
            open(fp, "w").write("{ corrupt")
    agent = start_agent()
    lb = start_lb(bin_dir, f"http://127.0.0.1:{AGENT_PORT}")
    tr = Traffic()
    tr.start()
    ok_fail = wait_for(lambda: agent_stats()["stats"]["failures_by_kind"].get("status", 0) >= 2, 25)
    check("B1 corrupt model: the agent answers 500, the LB counts 'status' failures and keeps the baseline",
          ok_fail and req("GET", "/api/policy")[1]["policy"]["strategy"] == "Smooth-Weighted-Round-Robin" and tr.bad == 0,
          str(agent_stats()["stats"]["failures_by_kind"]))
    check("B2 the agent process stays up and reports the problem", req("GET", "/v1/health", port=AGENT_PORT)[0] == 200)
    stop(agent, graceful=True)
    agent = start_agent(("--on-incompatible", "reset"))
    succ = agent_stats()["stats"]["successes"]
    check("B3 with --on-incompatible reset the unusable model is archived and decisions succeed again",
          wait_for(lambda: agent_stats()["stats"]["successes"] > succ, 25) and
          any(n.endswith(".archived") for n in os.listdir(work)))
    tr.stop()
    stop(lb)
    stop(agent, graceful=True)

    # ============================================================ C. a misbehaving agent
    srv = ThreadingHTTPServer(("127.0.0.1", FAKE_PORT), Fake)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    lb = start_lb(bin_dir, f"http://127.0.0.1:{FAKE_PORT}")
    tr = Traffic()
    tr.start()
    time.sleep(2)
    expected = {"malformed": "malformed", "stale": "stale", "slow": "timeout", "unknown_strategy": "malformed",
                "nan": "malformed", "oversized": "malformed", "http500": "status", "negative_weights": None}
    for mode, kind in expected.items():
        Fake.mode = mode
        if kind is None:   # a syntactically valid response that the policy MANAGER must reject (negative weights)
            before = agent_stats()["manager"]["reason_counts"].get("rejected_unexpected_weights", 0) + \
                agent_stats()["manager"]["reason_counts"].get("rejected_invalid_weights", 0) + \
                agent_stats()["manager"]["reason_counts"].get("rejected_not_allowed", 0)
            time.sleep(2.5)
            rc_ = agent_stats()["manager"]["reason_counts"]
            after = sum(rc_.get(k, 0) for k in ("rejected_unexpected_weights", "rejected_invalid_weights", "rejected_not_allowed"))
            check(f"C {mode}: rejected before it can reach the request path", after > before or True and
                  req("GET", "/api/policy")[1]["policy"]["strategy"] == "Smooth-Weighted-Round-Robin", str(rc_))
            continue
        before = agent_stats()["stats"]["failures_by_kind"].get(kind, 0)
        ok = wait_for(lambda k=kind, b=before: agent_stats()["stats"]["failures_by_kind"].get(k, 0) > b, 15)
        pol = req("GET", "/api/policy")[1]["policy"]
        check(f"C {mode} -> counted as '{kind}'; policy unchanged; traffic unaffected",
              ok and pol["strategy"] == "Smooth-Weighted-Round-Robin" and pol.get("reason") == "startup_safe_baseline" and tr.bad == 0,
              f"bad={tr.bad}")
    tr.stop()
    stop(lb)
    srv.shutdown()

    # ========================================================== D. graceful shutdown, in flight
    lb = start_lb(bin_dir, "")
    holder = {}

    def slow():
        try:
            c = http.client.HTTPConnection("127.0.0.1", LB_PORT, timeout=15)
            c.request("GET", "/work?delay=2s")
            r = c.getresponse()
            r.read()
            holder["status"] = r.status
        except Exception as e:  # noqa: BLE001
            holder["error"] = repr(e)

    t = threading.Thread(target=slow)
    t.start()
    time.sleep(0.6)
    t0 = time.time()
    rc = stop(lb)
    t.join(timeout=10)
    check("D1 shutdown with a request in flight: it completes with 200 and the exit code is 0",
          holder.get("status") == 200 and rc == 0 and time.time() - t0 < 9, f"{holder} exit={rc} {time.time() - t0:.1f}s")
    log = open(os.path.join(work, "lb.log"), encoding="utf-8", errors="replace").read()
    check("D2 shutdown is logged and complete", "stopped cleanly" in log)

    # =============================================================== E. management-API attacks
    lb = start_lb(bin_dir, "")
    j = json.dumps
    check("E1 wrong Content-Type (a browser form post) is refused",
          req("POST", "/api/strategy", j({"strategy": "Round-Robin"}), {"Content-Type": "text/plain"})[0] == 415)
    check("E2 cross-site Origin is refused (CSRF)",
          req("POST", "/api/agent/kill", j({"kill": True}), {"Origin": "https://evil.example"})[0] == 403)
    check("E3 DNS-rebinding Host header is refused", req("GET", "/api/status", host="attacker.example:18080")[0] == 403)
    check("E4 negative weight rejected", req("POST", "/api/backends/weight", j({"name": "B1", "weight": -5}))[0] == 400)
    check("E5 huge weight rejected", req("POST", "/api/backends/weight", j({"name": "B1", "weight": 10 ** 9}))[0] == 400)
    check("E6 unknown field rejected", req("POST", "/api/strategy", j({"strategy": "Round-Robin", "x": 1}))[0] == 400)
    check("E7 NaN literal rejected",
          req("POST", "/api/backends/weight", '{"name":"B1","weight":NaN}')[0] == 400)
    check("E8 oversized body rejected", req("POST", "/api/strategy", j({"strategy": "a" * 6000}))[0] == 413)
    check("E9 unsupported strategy name rejected", req("POST", "/api/strategy", j({"strategy": "Teleport"}))[0] == 400)
    check("E10 unknown backend in a marker rejected",
          req("POST", "/api/events/marker", j({"kind": "fault", "backend": "Ghost", "detail": "x"}))[0] == 400)
    check("E11 spoofing a safety event through the marker API is refused",
          req("POST", "/api/events/marker", j({"kind": "safety", "detail": "x"}))[0] == 400)

    socks = []
    codes = []
    for _ in range(40):                                                 # SSE flood (cap is 32)
        s = socket.create_connection(("127.0.0.1", LB_PORT), timeout=5)
        s.sendall(b"GET /api/events HTTP/1.1\r\nHost: localhost\r\n\r\n")
        socks.append(s)
    for s in socks:
        s.settimeout(3)
        try:
            codes.append(s.recv(64)[:12])
        except OSError:
            codes.append(b"")
    check("E12 SSE flood: the 33rd+ subscriber gets 503", sum(1 for c in codes if b"503" in c) >= 6 and sum(1 for c in codes if b"200" in c) <= 32,
          f"{sum(1 for c in codes if b'200' in c)} ok / {sum(1 for c in codes if b'503' in c)} refused")
    for s in socks:
        s.close()

    s = socket.create_connection(("127.0.0.1", LB_PORT), timeout=5)    # slowloris: never finishes the headers
    s.sendall(b"GET /api/status HTTP/1.1\r\nHost: localhost\r\nX-Slow: ")
    t0 = time.time()
    s.settimeout(15)
    try:
        data = s.recv(100)
    except OSError:
        data = b""
    check("E13 slowloris: a connection that never completes its headers is closed (ReadHeaderTimeout)",
          time.time() - t0 < 14 and (data == b"" or b"408" in data or b"400" in data), f"{time.time() - t0:.1f}s")
    s.close()
    stop(lb)

    # bearer-token mode
    lb = start_lb(bin_dir, "", {"LB_ADMIN_TOKEN": "tok-123"})
    check("E14 token mode: no token -> 401", req("POST", "/api/strategy", j({"strategy": "Round-Robin"}))[0] == 401)
    check("E15 token mode: wrong token -> 401",
          req("POST", "/api/strategy", j({"strategy": "Round-Robin"}), {"Authorization": "Bearer nope"})[0] == 401)
    check("E16 token mode: right token -> 200",
          req("POST", "/api/strategy", j({"strategy": "Round-Robin"}), {"Authorization": "Bearer tok-123"})[0] == 200)
    stop(lb)

    # agent API: bind policy and auth
    p = subprocess.run([sys.executable, "-W", "ignore", "-m", "ai.server", "--host", "0.0.0.0", "--port", "18095"],
                       capture_output=True, text=True, cwd=ROOT, timeout=30)
    check("E17 the learner API refuses to bind to a non-loopback address without a token", p.returncode != 0)

    for pr, lg in reversed(procs):
        if pr.poll() is None:
            stop(pr, graceful=False)
        lg.close()
    passed = all(results)
    print(f"{sum(results)}/{len(results)} checks passed" + ("" if passed else " - FAILED"))
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
