"""Model persistence (Phase 19) and the append-only decision log.

Model file (format 2, JSON, written atomically):
  format_version, learner, config, meta, counters, state, extra, created_at, saved_at, checksum

* meta carries what must match on restart: context mode, dimension, State version, backend ids,
  the FEATURE SCHEMA hash and the REWARD CONFIGURATION (and its hash). A model trained under a
  different feature layout or reward definition is refused, never silently reused.
* checksum = SHA-256 over the canonical JSON of (learner, config, meta, counters, state, extra):
  detects truncation/corruption that valid JSON would hide.
* Every save first keeps the previous good checkpoint as <path>.prev; if the primary file is
  corrupt on load, the previous checkpoint is used automatically (reported in the load info).
* Format 1 (Phases 8-10, no checksum) is still readable.
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
from datetime import datetime, timezone
from typing import Dict, Optional, Tuple

from .bandit import Learner

FORMAT_VERSION = 2
READABLE_FORMATS = (1, 2)


class PersistenceError(RuntimeError):
    pass


def _meta_ok(meta: dict, expect: dict) -> Optional[str]:
    for k, v in expect.items():
        if meta.get(k) != v:
            return f"{k}: saved {meta.get(k)!r} != current {v!r}"
    return None


def canonical(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), allow_nan=False)


def hash_json(obj) -> str:
    return hashlib.sha256(canonical(obj).encode("utf-8")).hexdigest()


def _body(doc: dict) -> dict:
    return {k: doc.get(k) for k in ("learner", "config", "meta", "counters", "state", "extra")}


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def save_model(path: str, learner: Learner, meta: dict, counters: dict, extra: Optional[dict] = None) -> None:
    """Atomic write (temp file + fsync + rename); the previous checkpoint becomes <path>.prev."""
    doc = {
        "format_version": FORMAT_VERSION,
        "learner": learner.name,
        "config": learner.config(),
        "meta": meta,  # context_mode, dim, state_version, backend_ids, feature_schema_hash, reward_config, ...
        "counters": counters,  # decision counter, updates, ...
        "state": learner.state_dict(),
        "extra": extra or {},  # e.g. drift-monitor state
    }
    created = now_iso()
    if os.path.exists(path):
        try:
            with open(path, "r", encoding="utf-8") as f:
                created = json.load(f).get("created_at", created)
        except (OSError, ValueError):
            pass
    doc["created_at"], doc["saved_at"] = created, now_iso()
    doc["checksum"] = hash_json(_body(doc))
    d = os.path.dirname(os.path.abspath(path))
    os.makedirs(d, exist_ok=True)
    if os.path.exists(path):
        shutil.copy2(path, path + ".prev")
    fd, tmp = tempfile.mkstemp(dir=d, suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(doc, f, allow_nan=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise


def _read_valid(path: str) -> dict:
    """Parse + integrity-check one model file (raises PersistenceError with a precise reason)."""
    try:
        with open(path, "r", encoding="utf-8") as f:
            doc = json.load(f)
    except (OSError, ValueError) as e:
        raise PersistenceError(f"unreadable model file {os.path.basename(path)}: {e}") from e
    fv = doc.get("format_version")
    if fv not in READABLE_FORMATS:
        raise PersistenceError(f"unsupported model format {fv!r}")
    if fv >= 2 and doc.get("checksum") != hash_json(_body(doc)):
        raise PersistenceError(f"checksum mismatch in {os.path.basename(path)} (corrupt or edited)")
    return doc


def load_model_ex(path: str, learner: Learner, expect_meta: dict) -> Tuple[dict, dict]:
    """Load into `learner` after validating integrity, algorithm and metadata.

    Returns (counters, info). If the primary file is corrupt and <path>.prev is valid, the
    previous checkpoint is restored and info['restored_from_backup'] is True. A model whose
    algorithm, dimension, feature schema, reward configuration, State version or backend set
    differs from the current one is REFUSED (PersistenceError) - never silently mixed.
    """
    info: Dict[str, object] = {"path": path, "restored_from_backup": False}
    try:
        doc = _read_valid(path)
    except PersistenceError as primary:
        prev = path + ".prev"
        if os.path.exists(prev):
            try:
                doc = _read_valid(prev)
                info.update(restored_from_backup=True, primary_error=str(primary))
            except PersistenceError:
                raise primary
        else:
            raise
    if doc.get("learner") != learner.name:
        raise PersistenceError(f"model is for {doc.get('learner')!r}, not {learner.name!r}")
    bad = _meta_ok(doc.get("meta", {}), expect_meta)
    if bad:
        raise PersistenceError(f"incompatible model ({bad})")
    try:
        learner.load_state_dict(doc["state"])
    except (KeyError, ValueError) as e:
        raise PersistenceError(f"corrupt model state: {e}") from e
    info.update(format_version=doc["format_version"], created_at=doc.get("created_at"), saved_at=doc.get("saved_at"),
                extra=doc.get("extra") or {}, meta=doc.get("meta", {}))
    return dict(doc.get("counters", {})), info


def load_model(path: str, learner: Learner, expect_meta: dict) -> dict:
    """Backward-compatible wrapper returning only the counters."""
    return load_model_ex(path, learner, expect_meta)[0]


def archive_model(path: str) -> Optional[str]:
    """Move a model file (and its .prev) aside with a timestamp suffix; returns the archive path."""
    if not os.path.exists(path):
        return None
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S")
    dest = f"{path}.{stamp}.archived"
    os.replace(path, dest)
    if os.path.exists(path + ".prev"):
        os.replace(path + ".prev", dest + ".prev")
    return dest


class DecisionLog:
    """Append-only JSONL log; also keeps records in memory for tests/experiments."""

    def __init__(self, path: Optional[str] = None, keep_in_memory: bool = True, max_memory: Optional[int] = None):
        self.path = path
        self.records: list = [] if keep_in_memory else None
        self.max_memory = max_memory
        if path:
            os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)

    def append(self, record: dict) -> None:
        if self.records is not None:
            self.records.append(record)
            if self.max_memory and len(self.records) > self.max_memory:
                del self.records[: len(self.records) - self.max_memory]
        if self.path:
            with open(self.path, "a", encoding="utf-8") as f:
                f.write(json.dumps(record, default=float) + "\n")
