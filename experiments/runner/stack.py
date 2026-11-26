"""Process-isolated experiment stack: backends, load balancer, optional Python agent,
and a per-process CPU/RAM sampler."""
from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from typing import Dict, List, Optional

import psutil

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
EXE = ".exe" if os.name == "nt" else ""
LB_URL = "http://localhost:8080"
AGENT_PORT = 8090


def http(method: str, url: str, body=None, timeout: float = 3.0) -> dict:
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"{}")


def wait_http(url: str, timeout: float = 20.0) -> None:
    end = time.time() + timeout
    last = None
    while time.time() < end:
        try:
            http("GET", url)
            return
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(0.2)
    raise RuntimeError(f"{url} not ready: {last}")


def wait_port(port: int, timeout: float = 15.0) -> None:
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.3):
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"port {port} not listening")


def build_binaries(bin_dir: str) -> None:
    os.makedirs(bin_dir, exist_ok=True)
    for name, pkg in (("lb", "."), ("backend", "./cmd/backend"), ("loadgen", "./cmd/loadgen")):
        subprocess.run(["go", "build", "-o", os.path.join(bin_dir, name + EXE), pkg], cwd=ROOT, check=True)


class ResourceSampler(threading.Thread):
    """Samples CPU (% of one core) and RSS (MB) of each process once per second."""

    def __init__(self, pids: Dict[str, int], interval: float = 1.0):
        super().__init__(daemon=True)
        self.procs = {}
        for name, pid in pids.items():
            try:
                p = psutil.Process(pid)
                p.cpu_percent(None)  # prime
                self.procs[name] = p
            except psutil.Error:
                pass
        self.interval, self.rows, self._stop = interval, [], threading.Event()
        psutil.cpu_percent(None)  # prime the system-wide counter

    def run(self):
        while not self._stop.wait(self.interval):
            now = time.time()
            # system-wide CPU (% of all cores): lets the analysis flag runs disturbed by other host activity
            self.rows.append({"t_unix": now, "proc": "system", "cpu_pct": psutil.cpu_percent(None),
                              "rss_mb": psutil.virtual_memory().used / 1048576.0})
            for name, p in self.procs.items():
                try:
                    self.rows.append({"t_unix": now, "proc": name, "cpu_pct": p.cpu_percent(None),
                                      "rss_mb": p.memory_info().rss / 1048576.0})
                except psutil.Error:
                    pass

    def stop(self):
        self._stop.set()
        self.join(timeout=3)


class Stack:
    """Start/stop one isolated stack. `lb_env` and `agent` come from the run spec."""

    def __init__(self, bin_dir: str, run_dir: str, pool: List[dict], lb_env: Dict[str, str],
                 agent: Optional[dict], backend_seed: int = 1):
        self.bin, self.run_dir, self.pool, self.lb_env = bin_dir, run_dir, pool, lb_env
        self.agent, self.backend_seed = agent, backend_seed
        self._procs: list = []
        self.pids: Dict[str, int] = {}
        self.sampler: Optional[ResourceSampler] = None
        os.makedirs(os.path.join(run_dir, "logs"), exist_ok=True)

    def _spawn(self, name: str, cmd: list, env: dict):
        f = open(os.path.join(self.run_dir, "logs", name + ".log"), "w", encoding="utf-8")
        p = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, cwd=ROOT, env=env)
        self._procs.append((p, f))
        self.pids[name] = p.pid
        return p

    def __enter__(self):
        for b in self.pool:
            env = dict(os.environ)
            if b.get("gomaxprocs"):
                env["GOMAXPROCS"] = str(b["gomaxprocs"])
            self._spawn(b["name"], [os.path.join(self.bin, "backend" + EXE), "-port", str(b["port"]), "-name", b["name"],
                                    "-conc", str(b["conc"]), "-cpu-work", f"{int(b.get('cpu_work_us', 300))}us",
                                    "-seed", str(self.backend_seed)], env)
        for b in self.pool:
            wait_port(b["port"])
        env = dict(os.environ, **self.lb_env)
        if self.agent:
            a = self.agent
            cmd = [sys.executable, "-W", "ignore", "-m", "ai.server", "--port", str(AGENT_PORT), "--algo", a["algo"],
                   "--alpha", str(a["alpha"]), "--epsilon", "0.05", "--context", a["context"], "--backends", str(len(self.pool)),
                   "--seed", str(a["seed"]), "--model", os.path.join(self.run_dir, "model.json"),
                   "--log", os.path.join(self.run_dir, "learner_events.jsonl")]
            self._spawn("agent", cmd, dict(os.environ))
            wait_http(f"http://127.0.0.1:{AGENT_PORT}/v1/health")
            env["LB_AGENT_URL"] = f"http://127.0.0.1:{AGENT_PORT}"
        self._spawn("lb", [os.path.join(self.bin, "lb" + EXE)], env)
        wait_http(LB_URL + "/api/status")
        self.sampler = ResourceSampler(self.pids)
        self.sampler.start()
        return self

    def __exit__(self, *a):
        if self.sampler:
            self.sampler.stop()
        for p, f in reversed(self._procs):
            try:
                p.terminate()
                p.wait(timeout=5)
            except Exception:  # noqa: BLE001
                p.kill()
            f.close()
        time.sleep(0.4)  # let the ports free up before the next run

    @property
    def resources(self) -> list:
        return self.sampler.rows if self.sampler else []

    def ctrl_arg(self) -> str:
        return ",".join(f"{b['name']}=http://localhost:{b['port'] + 1000}" for b in self.pool)
