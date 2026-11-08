# SPDX-License-Identifier: Apache-2.0
"""The project contract: what an outside project gives the harness, its status, and the submit lock.

A project is an adapter (a Python class) plus a PROJECT.md beside it. The harness runs tasks; the
project knows what a task is, how to set up its tree, what to tell the model, how to check the result,
what feedback to give, and how to submit. Submitting is the one step that reaches the outside project,
so it is locked: `Project.submit` refuses unless PROJECT.md says the status is `active` AND names the
maintainers' agreement (where and when they agreed that agent contributions may be submitted). Every
other step works in any status, so a project can run locally while nobody has agreed to anything.

PROJECT.md starts with a block of `key: value` lines (the rest is free text for people):

    # Project name

    status: active | paused | local-only | ended
    since: 2026-01-31
    agreement: <where the maintainers agreed, with its date>   (or: none, optionally with why)

Standard library only.
"""
from __future__ import annotations

import enum
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class Status(enum.Enum):
    ACTIVE = "active"            # runs, and may submit (with an agreement)
    PAUSED = "paused"            # the maintainers asked to stop, or we did: nothing is submitted
    LOCAL_ONLY = "local-only"    # runs and checks locally; never submits
    ENDED = "ended"


class SubmitRefused(RuntimeError):
    """Submit was called for a project that may not submit (status or agreement)."""


@dataclass(frozen=True)
class ProjectStatus:
    status: Status
    since: str | None = None
    agreement: str | None = None      # None: no agreement recorded

    @property
    def may_submit(self) -> bool:
        return self.status is Status.ACTIVE and bool(self.agreement)

    def why_not(self) -> str | None:
        if self.status is not Status.ACTIVE:
            return f"status is {self.status.value}" + (f" (since {self.since})" if self.since else "")
        if not self.agreement:
            return "no maintainers' agreement is recorded in PROJECT.md"
        return None


FIELD = re.compile(r"^([a-z][a-z_-]*):\s*(.*?)\s*$")
NONE = {"", "none", "no", "-", "n/a"}


def parse_status(text: str) -> ProjectStatus:
    """The status block of a PROJECT.md: the `key: value` lines before the first line that is neither
    one, a heading nor blank. A missing or unknown status is an error (a project without one must not
    run as if it were active)."""
    fields: dict[str, str] = {}
    for line in text.splitlines():
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        m = FIELD.match(s)
        if not m:
            break
        fields.setdefault(m.group(1).lower(), m.group(2))
    raw = fields.get("status")
    if raw is None:
        raise ValueError("PROJECT.md has no `status:` line")
    try:
        status = Status(raw.split()[0].lower())
    except ValueError:
        raise ValueError(f"PROJECT.md: unknown status {raw!r}; one of {[s.value for s in Status]}") from None
    return ProjectStatus(status, fields.get("since") or None, _agreement(fields.get("agreement")))


def _agreement(value: str | None) -> str | None:
    """The recorded agreement, or None for a missing one or one that starts with none/no/-/n/a (so
    `none (withdrawn on <date>)` keeps its reason for people and still counts as none)."""
    if value is None:
        return None
    first = (value.split() or [""])[0].lower().strip("(),;:.")
    return None if value.strip().lower() in NONE or first in NONE else value


def read_status(path: Path) -> ProjectStatus:
    return parse_status(Path(path).read_text())


@dataclass
class Task:
    id: str
    data: dict = field(default_factory=dict)      # the project's own fields


@dataclass
class Workspace:
    """A task's paths. `tree` and `home` are the model's (mounted into its container; the host touches
    them only through safefs and hostgit); `raw` (evidence) and `state` (host-only working files: the
    tree's base snapshot, overlay layers, the sandbox's generated files) are not. Sessions running at
    the same time never share any of them."""
    tree: Path
    home: Path
    raw: Path
    state: Path


@dataclass
class Verdict:
    outcome: str                      # e.g. "proved", "compile_error", "refused", "no_solution"
    detail: str | None = None
    unscored: str | None = None       # None: the model's result; else why not (e.g. "infra"; see rounds.py)
    data: dict = field(default_factory=dict)    # the project's own fields, kept in rounds.json

    def record(self) -> dict:
        return {"outcome": self.outcome, "detail": self.detail, "unscored": self.unscored, **self.data}


class Project:
    """Subclass and implement the steps; `run_task` (task.py) drives them. `status` comes from PROJECT.md
    (path given to __init__); implement `_submit`, never override `submit`.

        select()                                the tasks
        workspace(task) -> Workspace            the task's paths (nothing created)
        prepare(task) -> Workspace              a fresh tree and an empty HOME; ends with loop.snapshot_base
        session(task, ws, endpoint) -> Session  the loop's Session (prompt, sandbox, caps' texts)
        check(task, ws, run, round) -> Verdict  the machine verdict on the round (run = its run.json)
        feedback(task, verdict, round, total)   the next round's message, or None to stop
        submit(task, ws)                        locked: see the module docstring
    """

    name = "project"
    keep_on_retry: tuple[str, ...] = ()     # evidence files a fresh try of round 1 keeps in place (rounds.json always)

    def __init__(self, project_md: Path):
        self.project_md = Path(project_md)

    @property
    def status(self) -> ProjectStatus:
        """Read on every call: a status change in PROJECT.md takes effect at once."""
        return read_status(self.project_md)

    def select(self) -> list[Task]:
        raise NotImplementedError

    def workspace(self, task: Task) -> Workspace:
        raise NotImplementedError

    def prepare(self, task: Task) -> Workspace:
        raise NotImplementedError

    def session(self, task: Task, ws: Workspace, endpoint):
        raise NotImplementedError

    def check(self, task: Task, ws: Workspace, run: dict, round: int) -> Verdict:
        raise NotImplementedError

    def feedback(self, task: Task, verdict: Verdict, round: int, total: int) -> str | None:
        """The next round's message, or None to stop."""
        raise NotImplementedError

    def submit(self, task: Task, ws: Workspace) -> Any:
        """The submit lock: refused unless the status is `active` and an agreement is recorded."""
        st = self.status
        why = st.why_not()
        if why:
            raise SubmitRefused(f"{self.name}: not submitting {task.id}: {why}")
        return self._submit(task, ws)

    def _submit(self, task: Task, ws: Workspace) -> Any:
        raise NotImplementedError
