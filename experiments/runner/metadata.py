"""run_metadata.json content: everything needed to reproduce and interpret a run."""
from __future__ import annotations

import hashlib
import platform
import subprocess
import sys
from datetime import datetime, timezone

import psutil

from .stack import ROOT


def _run(cmd) -> str:
    try:
        return subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, timeout=20).stdout.strip()
    except Exception:  # noqa: BLE001
        return ""


def git_info() -> dict:
    status = _run(["git", "status", "--porcelain", "--untracked-files=no"])
    return {"commit": _run(["git", "rev-parse", "HEAD"]), "branch": _run(["git", "rev-parse", "--abbrev-ref", "HEAD"]),
            "dirty": bool(status), "dirty_files": status.splitlines()[:20]}


def cpu_name() -> str:
    if platform.system() == "Windows":
        out = _run(["wmic", "cpu", "get", "name"])
        lines = [l.strip() for l in out.splitlines() if l.strip() and l.strip().lower() != "name"]
        if lines:
            return lines[0]
    return platform.processor() or platform.machine()


def hardware() -> dict:
    return {"cpu": cpu_name(), "logical_cores": psutil.cpu_count(logical=True),
            "physical_cores": psutil.cpu_count(logical=False),
            "ram_gb": round(psutil.virtual_memory().total / 2 ** 30, 1), "os": platform.platform(),
            "machine": platform.machine()}


def versions() -> dict:
    return {"go": _run(["go", "version"]), "python": sys.version.split()[0], "numpy": _numpy_version()}


def _numpy_version() -> str:
    try:
        import numpy
        return numpy.__version__
    except Exception:  # noqa: BLE001
        return ""


def sha256_file(path: str) -> str:
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def build(run_id: str, exp_key: str, arm: str, rep: int, seed: int, scenario_path: str, scenario: dict,
          config: dict) -> dict:
    return {
        "run_id": run_id, "experiment": exp_key, "arm": arm, "repetition": rep, "seed": seed,
        "date_utc": datetime.now(timezone.utc).isoformat(), "git": git_info(), "hardware": hardware(),
        "versions": versions(), "scenario": {"file": scenario_path.replace("\\", "/").split("/experiments/")[-1],
                                              "name": scenario.get("name"), "sha256": sha256_file(scenario_path),
                                              "definition": scenario},
        "config": config,
    }
