# SPDX-License-Identifier: Apache-2.0
"""One task to its end: the seam a queue or scheduler calls.

    run_task(project, task, endpoint, caps)

prepares the task when it starts (or starts over), runs the session (loop.run_session), checks each round
(project.check), gives feedback for fix rounds (project.feedback) and keeps the state in the task's
evidence directory (rounds.run_rounds: idempotent, so the same call continues after a stop). It holds no
global state: several tasks run at once in several processes or threads, on one host or several, as long
as their workspaces differ (each Project's workspace paths are per task) and every session's sandbox key is
unique on its host. Which tasks run, where and when, is the caller's.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

from . import evidence, loop, rounds
from .project import Project, Task


@dataclass
class Endpoint:
    """Any OpenAI-compatible endpoint: base URL (…/v1), model name, and a file holding the API key."""
    base_url: str
    model: str
    key_file: str | None = None


@dataclass
class Caps:
    steps: int = 30              # round 1: steps
    minutes: int = 30            # round 1: minutes of the model's own time
    fix_rounds: int = 2
    fix_steps: int = 20
    fix_minutes: int = 20
    infra_retry: bool = True     # a round the endpoint's failures consumed runs again once


def run_task(project: Project, task: Task, endpoint: Endpoint, caps: Caps = Caps(),
             probe: Callable[[], bool] | None = None, room: Callable[[], None] | None = None,
             wait: Callable[[Callable[[], bool]], bool] = rounds.wait_for_gateway) -> dict:
    """The task to its end; returns rounds.json (with `final`). `probe` says whether the endpoint answers
    (default: never asked); `room` waits for room before a fix round (a queue's courtesy)."""
    ws = project.workspace(task)
    e = endpoint

    def session():
        return project.session(task, ws, e)

    def start():
        project.prepare(task)
        loop.run_session(session(), e.base_url, e.key_file, e.model, caps.minutes, caps.steps)

    def resume():
        loop.run_session(session(), e.base_url, e.key_file, e.model, caps.minutes, caps.steps, resume=True)

    def followup(msg: str):
        loop.run_session(session(), e.base_url, e.key_file, e.model, caps.fix_minutes, caps.fix_steps, followup=msg)

    def retry():
        loop.run_session(session(), e.base_url, e.key_file, e.model, caps.minutes, caps.steps, resume=True, retry=True)

    def check(rm: dict, n: int) -> dict:
        # a follow-up that made no tool call never ran: a tooling problem, not the model's result
        return {"round": n, **project.check(task, ws, rm, n).record(),
                "no_tool_calls": bool(rm.get("followup_no_tool_calls"))}

    def feedback(rec: dict, n: int, total: int) -> str | None:
        from .project import Verdict
        known = {"outcome", "detail", "unscored", "round", "try", "run_finished_at", "events_size", "retried",
                 "no_tool_calls"}
        v = Verdict(rec["outcome"], rec.get("detail"), rec.get("unscored"), {k: x for k, x in rec.items() if k not in known})
        return project.feedback(task, v, n, total)

    # the task's lock from prepare to the end: a second caller cannot reset a running task's tree or HOME
    with loop.task_lock(ws.raw):
        return _run(project, task, ws, caps, start, resume, retry, followup, check, feedback, probe, room, wait)


def _run(project, task, ws, caps, start, resume, retry, followup, check, feedback, probe, room, wait) -> dict:
    return rounds.run_rounds(rounds.Rounds(raw=ws.raw, fix_rounds=caps.fix_rounds, infra_retry=caps.infra_retry,
                                           start=start, resume=resume, retry=retry, followup=followup, check=check,
                                           feedback=feedback, probe=probe, room=room, wait=wait, label=task.id,
                                           archive=lambda k: evidence.archive_try(
                                               ws.raw, k, keep=(evidence.ROUNDS, *project.keep_on_retry))))
