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
reuses an older check. `stepmetrics` writes steps.jsonl. Standard library only.
"""
from __future__ import annotations

import json
import shutil
from pathlib import Path

from . import stepmetrics

RUN = "run.json"
ROUNDS = "rounds.json"
EVENTS = "events.jsonl"
STEPS = stepmetrics.FILE


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
