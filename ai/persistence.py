"""Model persistence and the append-only decision log."""
from __future__ import annotations

import json
import os
import tempfile
from typing import Optional

from .bandit import Learner

FORMAT_VERSION = 1


class PersistenceError(RuntimeError):
    pass


def _meta_ok(meta: dict, expect: dict) -> Optional[str]:
    for k, v in expect.items():
        if meta.get(k) != v:
            return f"{k}: saved {meta.get(k)!r} != current {v!r}"
    return None


def save_model(path: str, learner: Learner, meta: dict, counters: dict) -> None:
    """Atomic write (temp file + rename) so a crash never leaves a torn model."""
    doc = {
        "format_version": FORMAT_VERSION,
        "learner": learner.name,
        "config": learner.config(),
        "meta": meta,  # context_mode, dim, state_version, backend_ids
        "counters": counters,  # decision counter, updates, ...
        "state": learner.state_dict(),
    }
    d = os.path.dirname(os.path.abspath(path))
    os.makedirs(d, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=d, suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(doc, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise


def load_model(path: str, learner: Learner, expect_meta: dict) -> dict:
    """Load into `learner`. Refuses incompatible models (different dimension,
    context mode, state version, backend set, or algorithm). Returns counters."""
    with open(path, "r", encoding="utf-8") as f:
        doc = json.load(f)
    if doc.get("format_version") != FORMAT_VERSION:
        raise PersistenceError(f"unsupported model format {doc.get('format_version')!r}")
    if doc.get("learner") != learner.name:
        raise PersistenceError(f"model is for {doc.get('learner')!r}, not {learner.name!r}")
    bad = _meta_ok(doc.get("meta", {}), expect_meta)
    if bad:
        raise PersistenceError(f"incompatible model ({bad})")
    try:
        learner.load_state_dict(doc["state"])
    except (KeyError, ValueError) as e:
        raise PersistenceError(f"corrupt model state: {e}") from e
    return dict(doc.get("counters", {}))


class DecisionLog:
    """Append-only JSONL log; also keeps records in memory for tests/experiments."""

    def __init__(self, path: Optional[str] = None, keep_in_memory: bool = True):
        self.path = path
        self.records: list = [] if keep_in_memory else None
        if path:
            os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)

    def append(self, record: dict) -> None:
        if self.records is not None:
            self.records.append(record)
        if self.path:
            with open(self.path, "a", encoding="utf-8") as f:
                f.write(json.dumps(record, default=float) + "\n")
