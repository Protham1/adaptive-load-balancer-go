"""HTTP client for the Go load balancer (implements the controller's Environment)."""
from __future__ import annotations

import json
import urllib.error
import urllib.request

from .controller import ActionRejected


class HTTPEnvironment:
    def __init__(self, base_url: str = "http://localhost:8080", timeout_s: float = 5.0):
        self.base = base_url.rstrip("/")
        self.timeout = timeout_s

    def _get(self, path: str) -> dict:
        with urllib.request.urlopen(self.base + path, timeout=self.timeout) as r:
            return json.loads(r.read().decode("utf-8"))

    def get_state(self) -> dict:
        return self._get("/api/state")

    def get_metrics(self) -> dict:
        return self._get("/api/metrics")

    def get_policy(self) -> dict:
        return self._get("/api/policy")

    def apply_action(self, action: int, decision_id: str) -> None:
        body = json.dumps({"action": int(action), "decision_id": decision_id}).encode("utf-8")
        req = urllib.request.Request(self.base + "/api/policy", data=body, method="POST",
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                r.read()
        except urllib.error.HTTPError as e:
            if e.code == 400:
                raise ActionRejected(e.read().decode("utf-8", "replace").strip()) from e
            raise
