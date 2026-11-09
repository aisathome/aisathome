# SPDX-License-Identifier: Apache-2.0
"""The evidence layout of one task: everything a session and its rounds leave, in one directory per task.

    <raw>/                     one directory per task (a project chooses where, e.g. raw/<date>/<task>/)
      prompt.md, followup<N>.md, events.jsonl, opencode.stderr.log, steps.jsonl, model.diff,
      session.json, run.json, run.round<N>.json      the session loop's files (see loop.py)
      rounds.json              the fix rounds' state (rounds.py): one record per checked round, and
                               `final` once the task has ended; written atomically
      infra-try<K>/            a first round that the endpoint's failures consumed, moved aside before
                               it is started again

Idempotent resume: a command that runs a task again reads rounds.json and run.json and goes on from
where the task stands (a session still running or interrupted is resumed, a finished round that has no
record is checked, a task with `final` is left alone). A record belongs to one run: it carries the run's
finish time, the size of events.jsonl and the round-1 try, so a resumed round or a fresh try never
reuses an older check. `stepmetrics` writes steps.jsonl.

No evidence file holds the endpoint's API key: the model never sees it (the sandbox's forwarder adds it),
and as a second wall `redact_key` masks it in every file of the evidence directory after each session
(same length, so byte offsets into events.jsonl stay valid). Standard library only.
"""
from __future__ import annotations

import gzip
import json
import os
import shutil
from pathlib import Path

from . import stepmetrics

RUN = "run.json"
ROUNDS = "rounds.json"
EVENTS = "events.jsonl"
STEPS = stepmetrics.FILE
MIN_SECRET = 8           # shorter "keys" (placeholders, test values) are not masked: they would wreck the evidence


def events_size(raw: Path) -> int:
    p = raw / EVENTS
    return p.stat().st_size if p.exists() else 0


def read_run(raw: Path) -> dict | None:
    """run.json, or None when there is none yet."""
    p = raw / RUN
    return json.loads(p.read_text()) if p.exists() else None


def run_finished(rm: dict | None) -> bool:
    """A run that ended (not a checkpoint of one still running or stopped hard)."""
    return bool(rm) and not rm.get("partial") and bool(rm.get("finished_at"))


def round_of(rm: dict) -> int:
    """The round a run.json describes: 1 for the first, 1 + N for follow-up round N."""
    return (rm.get("followup_round") or 0) + 1


def load_rounds(raw: Path) -> dict:
    f = raw / ROUNDS
    return json.loads(f.read_text()) if f.exists() else {"rounds": []}


def save_rounds(raw: Path, st: dict):
    tmp = raw / (ROUNDS + ".tmp")
    tmp.write_text(json.dumps(st, ensure_ascii=False, indent=1) + "\n")
    tmp.replace(raw / ROUNDS)


def run_identity(st: dict, rm: dict, raw: Path) -> dict:
    """What ties a round's record to one run: the round-1 try, run.json's finish time and the events' size
    (a resumed round appends events; a fresh try after `archive_try` counts as the next try)."""
    return {"try": st.get("try", 1), "run_finished_at": rm.get("finished_at"), "events_size": events_size(raw)}


def archive_try(raw: Path, k: int, keep: tuple[str, ...] = (ROUNDS,)) -> Path:
    """Move a first round's evidence to <raw>/infra-try<k>/ before it is started again; `keep` stays."""
    dest = raw / f"infra-try{k}"
    dest.mkdir(exist_ok=True)
    for p in list(raw.iterdir()):
        if p.name in keep or p.name.startswith("infra-try"):
            continue
        shutil.move(str(p), str(dest / p.name))
    return dest


def mask(secret: bytes) -> bytes:
    """The same number of bytes as `secret`, saying what it was."""
    return (b"[REDACTED]" + b"*" * len(secret))[:len(secret)]


def redact(raw: Path, secret: str | bytes) -> int:
    """Mask `secret` in every regular file under `raw` (also inside .gz files; symlinks are not followed),
    with a mask of the same length. Returns the number of files that held it."""
    s = secret.encode() if isinstance(secret, str) else secret
    if len(s) < MIN_SECRET or not Path(raw).is_dir():
        return 0
    found = 0
    for d, dirs, files in os.walk(raw):
        dirs[:] = [x for x in dirs if not os.path.islink(os.path.join(d, x))]
        for name in files:
            p = Path(d) / name
            if p.is_symlink() or not p.is_file():
                continue
            data = p.read_bytes()
            gz = name.endswith(".gz")
            if gz:
                try:
                    data = gzip.decompress(data)
                except (OSError, EOFError):
                    continue
            if s not in data:
                continue
            data = data.replace(s, mask(s))
            tmp = p.with_name(f".{name}.redact.tmp")
            tmp.write_bytes(gzip.compress(data) if gz else data)
            tmp.replace(p)
            found += 1
    return found


def redact_key(raw: Path, key_file: str | None) -> int:
    """`redact` with the endpoint's key read from `key_file` (stripped, as it is sent); 0 without one."""
    if not key_file:
        return 0
    try:
        key = Path(key_file).read_bytes().strip()
    except OSError:
        return 0
    return redact(raw, key)
