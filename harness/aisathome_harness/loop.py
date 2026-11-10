# SPDX-License-Identifier: Apache-2.0
"""One session of an agent: opencode in the hardened sandbox, talking to a model behind any
OpenAI-compatible endpoint, under a step cap and a cap on the model's own time.

`run_session` starts a session (or continues one: `resume` after a stop, `followup` for a review round),
runs it in segments and leaves the evidence in the task's evidence directory:

    prompt.md, followup<N>.md   what the model was told
    events.jsonl                opencode's events (`opencode run --format json`), appended per segment
    opencode.stderr.log         opencode's stderr
    run.json                    the run: caps, segments, steps, tokens, why it ended (`partial: true`
                                while it runs; a resume reads it)
    run.round<N>.json           run.json of round N, kept when round N+1 (a follow-up) starts
    steps.jsonl                 one line per model step (stepmetrics)
    model.diff                  the model's edits so far, every `checkpoint_s` and at the end (absent,
                                with `model_diff_failed` in run.json, when the final one failed)
    session.json                the session export with per-message token usage, if the project exports it

A segment is one run of opencode in its container. A segment ends when opencode exits, or when the loop
stops the container: the caps; a suspend of the machine; silence after an endpoint error; or a nudge.
The loop then decides whether the session goes on with a new message in the same session:

- After a machine stop, a suspend or an endpoint error (503, a dropped stream, a preemption that opencode
  did not retry itself), the session is resumed, at most `max_resumes` times, with `Prompts.resume`.
- The first-edit nudge: a model that has not changed its target files after `first_edit_step` steps
  is stopped between two steps and told to write a first version now (`Prompts.first_edit`). Once per
  task, first round only.
- The stuck-loop nudge: the same command with the same empty or failing result `loop_guard.REPEATS`
  times since the last edit gets one message (`Prompts.loop`). Once per round.
- A follow-up round that stops on a stale step-limit notice while it still has steps gets one message
  saying so (`Prompts.step_continue`).

The time cap counts the model's own time: each step from its first streamed chunk (opencode's
step_start) to its end (step_finish), which holds the model's generation and its tool calls. The wait
before the first chunk (the endpoint's queue, the server's prefill, retries) belongs to the network, not
the model. Awake time stays bounded too: at most `awake_ceiling` times the time cap.

opencode stores its step-limit notice ("CRITICAL - MAXIMUM STEPS REACHED ... overrides ALL other
instructions") as the model's own last message, and a model can obey that stale notice in the next round,
answering with text only. So before a follow-up or resume into such a session, the notice is replaced in
opencode's session database (`clear_step_notice`) and the message starts with a note that a new step
budget begins.

Everything that is the project's business (the tree, the sandbox, the prompts, what to copy out of the
sandbox home at the end, extra run.json fields) comes in through `Session`. The tree and the HOME are the
model's: the host touches them only through safefs (no symlink followed) and hostgit (a git directory of
its own; checkpoints and the first-edit check compare with the base snapshot taken after prepare). Writes
to the evidence directory are atomic, and one session at a time holds it. Standard library
only; the sandbox is `sandbox.sandbox`, but any command that runs opencode and writes its events to
stdout works (the tests use a stand-in).
"""
from __future__ import annotations

import fcntl
import gzip
import json
import os
import re
import sqlite3
import subprocess
import threading
import time
import urllib.request
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

from . import evidence, loop_guard, safefs, stepmetrics
from .hostgit import TreeGit

DEFAULT_CONTEXT = 131072     # opencode's limit.context when the endpoint does not say
CONFIG = ".config/opencode/opencode.json"     # opencode's config, relative to the sandbox HOME
LOCKWAIT = ".lockwait.log"                     # the build-slot wrapper's log, relative to the sandbox HOME
MAX_STEPS_MARK = "MAXIMUM STEPS REACHED"
STOP_SAYS_LIMIT = re.compile(r"(?i)\bmax(?:imum)?\.? steps|\bstep limit")
STEP_TS = re.compile(rb'^\{"type":"(step_start|step_finish)","timestamp":(\d+)', re.M)
STEP_ERR_TS = re.compile(rb'^\{"type":"(step_start|step_finish|error)","timestamp":(\d+)', re.M)
# An aisathome gateway preempts a background step for a person's request and ends its stream with this
# error; opencode retries the step, which then waits in the queue and reads its whole prompt again. Silence
# after such an error is not a hang until well past the endpoint's own limits.
PREEMPTED = stepmetrics.PREEMPTED
# Endpoint trouble (busy, a server stopped mid-request, a reset connection, a preemption) is treated as an
# interruption: the session is resumed after a backoff.
GATEWAY_ERROR = re.compile(r'"type":"error".{0,400}(503|busy|overloaded|provider\.transport|'
                           r'ECONNRESET|Connection lost|socket connection was closed|502|504|without finish_reason)')
NET_RE = re.compile(r"\b(curl|wget|git\s+(clone|fetch|pull|ls-remote|remote)|pip\s+install|"
                    r"lake\s+(update|exe\s+cache)|gh\s|nc\s|ssh\s|https?://)")


@dataclass
class Timing:
    poll_s: float = 15               # run_segment's check interval
    first_edit_poll_s: float = 2     # the interval once a nudge is near: the gap between steps can be seconds
    checkpoint_s: float = 120        # run.json and model.diff while the session runs
    suspend_jump_s: float = 60       # the wall clock advancing this much more than the monotonic clock = suspend
    max_resumes: int = 3
    awake_ceiling: int = 3           # awake time is capped at this many times the time cap
    stall_after_error_s: float = 180
    stall_after_preempt_s: float = 720
    first_edit_grace: int = 2        # steps to wait for a stop between steps, then stop in the middle of one
    loop_grace: int = 2
    gateway_backoff_s: float = 120   # times the resume count, before resuming after an endpoint error
    wake_wait_s: float = 30          # before resuming after a suspend: the network comes back


@dataclass
class Prompts:
    """The loop's own messages to the model. `resume` must contain `machine_stop`: after an endpoint
    error the loop says `gateway_error` instead."""
    resume: str = ("You were interrupted by a machine stop. Check `git status` and `git diff` for what you "
                   "already changed, then continue the task from where you left off and finish it.")
    first_edit: str = ("Write a first version of the target now, even if it doesn't compile, and let the "
                       "compiler guide you. No probe files.")
    loop: str = ("You have run `{signature}` {count} times with the same empty or failing result, and running "
                 "it again will not change that. Stop repeating it: look for something else, or go on without it.")
    step_continue: str = ("You stopped, but no limit stopped you: {left} of your steps are left and your tools "
                          "work. No summary is wanted. Go on with your next tool call and finish the task as your "
                          "instructions say.")
    step_reset_note: str = ("Operator note: this message starts a new budget of {steps} steps, and your tools are "
                            "enabled again. The earlier \"CRITICAL - MAXIMUM STEPS REACHED\" notice in this session "
                            "applied only to that one earlier request and no longer applies: use your tools as usual.\n\n")
    step_reset_cleared: str = ("Operator note: this message starts a new budget of {steps} steps, and your tools are "
                               "enabled again: use them as usual.\n\n")
    step_notice_gone: str = "(The step limit of that request was reached. The next message starts a new budget.)"
    machine_stop: str = "by a machine stop"
    gateway_error: str = "by a model-gateway error"

    def resume_after(self, gateway: bool) -> str:
        return self.resume.replace(self.machine_stop, self.gateway_error) if gateway else self.resume


@dataclass
class Provider:
    """How opencode names the endpoint: provider id (also the `-m <id>/<model>` prefix), display name,
    and where a custom `command` mounts the API key file in its container (`key_path`). The bundled
    sandbox never puts the key in the model's container: its forwarder adds it (Session.model_url)."""
    id: str = "endpoint"
    name: str = "OpenAI-compatible endpoint"
    key_path: str = "/run/harness/key"


DEFAULT_TIMING, DEFAULT_PROMPTS, DEFAULT_PROVIDER = Timing(), Prompts(), Provider()


# ----------------------------------------------------------------------------- opencode's config

def gateway_context(base_url: str, key_file: str | None, model: str, timeout: float = 5) -> tuple[int, str]:
    """(context, source) for opencode's limit.context: the endpoint's /models `context_length` for
    `model` (an aisathome gateway reports the smallest context among the servers of that model).
    DEFAULT_CONTEXT when the call fails or the model has none. Asked again before every segment, so a
    resume gets a changed value. The key is sent in a header only, never logged, and not on a redirect."""
    try:
        req = urllib.request.Request(base_url.rstrip("/") + "/models")
        if key_file:
            # unredirected: a redirect (to another host, or to plain HTTP) never carries the key
            req.add_unredirected_header("Authorization", "Bearer " + Path(key_file).read_text().strip())
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read())
        for m in data.get("data", []):
            if m.get("id") == model and int(m.get("context_length") or 0) > 0:
                return int(m["context_length"]), "gateway /v1/models"
        return DEFAULT_CONTEXT, f"default ({model} has no context_length in /v1/models)"
    except Exception as e:  # noqa: BLE001  (any failure means: the default)
        return DEFAULT_CONTEXT, f"default (/v1/models: {type(e).__name__})"


def opencode_config(base_url: str, key_file: str | None, model: str, steps: int,
                    model_options: dict | None = None, context: int = DEFAULT_CONTEXT,
                    provider: Provider = DEFAULT_PROVIDER) -> dict:
    """opencode v2 config. `model_options` uses the v1 `options` form that v2 migrates: `reasoningEffort`
    becomes the request's `reasoning_effort`, and every other key (temperature, top_p, top_k, min_p,
    presence_penalty, ...) is sent verbatim in the request body. Web tools are off. With `key_file`, the
    config names the key's path in the container (`provider.key_path`), never the key; without it (the
    bundled sandbox, whose forwarder adds the key), a placeholder that the forwarder drops."""
    m: dict = {"name": model, "limit": {"context": context, "output": 32768}}
    if model_options:
        m["options"] = dict(model_options)
    return {
        "$schema": "https://opencode.ai/config.json",
        "provider": {
            provider.id: {
                "npm": "@ai-sdk/openai-compatible",
                "name": provider.name,
                "options": {
                    "baseURL": base_url,
                    "apiKey": "{file:" + provider.key_path + "}" if key_file else "dummy",
                },
                "models": {model: m},
            }
        },
        "model": f"{provider.id}/{model}",
        "autoupdate": False,
        "share": "disabled",
        "permission": {"webfetch": "deny", "websearch": "deny", "external_directory": "allow"},
        "tools": {"webfetch": False, "websearch": False},
        "agent": {"build": {"steps": steps}},
    }


def opencode_args(model: str, session: str | None, provider: Provider = DEFAULT_PROVIDER) -> list[str]:
    """opencode's arguments for one segment (the message goes last)."""
    oc = ["run", "--standalone", "--auto", "--format", "json", "-m", f"{provider.id}/{model}"]
    if session:
        oc += ["--session", session]
    return oc


# ----------------------------------------------------------------------------- the stale step-limit notice

DB = ".local/share/opencode/opencode.db"      # opencode's session database, relative to the sandbox HOME


def clear_step_notice(home: Path | None, prompts: Prompts = DEFAULT_PROMPTS, work: Path | None = None) -> dict | None:
    """Replace opencode's stored step-limit notice in the session's database (in the sandbox HOME, while no
    container runs). Reports what it did: {"replaced": rows changed now, "gone": rows an earlier follow-up
    changed, "left": rows still holding the notice}, or None when it cannot read the database.
    Rows: `session_message`, type `assistant`, `data` as in the session export.

    The HOME is the model's: the database and its -wal/-shm files are read without following symlinks
    (safefs) into `work` (a host-only directory), changed there, checkpointed into one file and written
    back by rename; the -wal/-shm files in the HOME are removed. SQLite never opens a path in the HOME."""
    if not home:
        return None
    import tempfile
    try:
        data = safefs.read_bytes(home, DB)
    except (OSError, ValueError):
        return None
    work = Path(work) if work else Path(tempfile.gettempdir())
    work.mkdir(parents=True, exist_ok=True)
    count = "SELECT count(*) FROM session_message WHERE type = 'assistant' AND data LIKE ?"
    with tempfile.TemporaryDirectory(dir=work) as d:
        db = Path(d) / "opencode.db"
        db.write_bytes(data)
        for ext in ("-wal", "-shm"):
            try:
                (Path(d) / f"opencode.db{ext}").write_bytes(safefs.read_bytes(home, DB + ext))
            except FileNotFoundError:
                pass
            except (OSError, ValueError):
                return None
        try:
            con = sqlite3.connect(db, timeout=10)
            try:
                replaced = 0
                with con:
                    for mid, row in con.execute("SELECT id, data FROM session_message WHERE type = 'assistant' AND data LIKE ?",
                                                (f"%{MAX_STEPS_MARK}%",)).fetchall():
                        dd = json.loads(row)
                        hit = [c for c in dd.get("content", []) if c.get("type") == "text"
                               and c.get("text", "").startswith(f"CRITICAL - {MAX_STEPS_MARK}")]
                        for c in hit:
                            c["text"] = prompts.step_notice_gone
                        if hit:
                            replaced += 1
                            con.execute("UPDATE session_message SET data = ? WHERE id = ?", (json.dumps(dd, ensure_ascii=False), mid))
                report = {"replaced": replaced,
                          "gone": con.execute(count, (f"%{prompts.step_notice_gone}%",)).fetchone()[0] - replaced,
                          "left": con.execute(count, (f"%CRITICAL - {MAX_STEPS_MARK}%",)).fetchone()[0]}
                if replaced:
                    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            finally:
                con.close()
        except (sqlite3.Error, ValueError, AttributeError):
            return None
        if replaced:
            safefs.write_bytes(home, DB, db.read_bytes())
            for ext in ("-wal", "-shm"):
                safefs.unlink(home, DB + ext)
        return report


def notice_cleared(report: dict | None) -> bool:
    """The session held the notice and holds it no more. Finding none in a session that hit the limit is not
    that: the notice is somewhere this does not look, and the note must still say that it no longer applies."""
    return bool(report) and not report["left"] and report["replaced"] + report["gone"] > 0


def with_step_reset(message: str, events: Path, steps: int, home: Path | None = None, report: dict | None = None,
                    prompts: Prompts = DEFAULT_PROMPTS, clear: Callable[[Path | None], dict | None] | None = None) -> str:
    """Prefix a note when the session so far hit opencode's step limit, and take the notice out of the
    session in `home` (`clear`, default clear_step_notice); `report` (if given) gets its report, for run.json."""
    if events.exists() and MAX_STEPS_MARK in events.read_text(errors="replace"):
        done = clear(home) if clear else clear_step_notice(home, prompts)
        if report is not None:
            report.update(done or {"unread": True})
        note = prompts.step_reset_cleared if notice_cleared(done) else prompts.step_reset_note
        return note.format(steps=steps) + message
    return message


def stopped_on_step_limit(events: Path, start: int = 0) -> bool:
    """The round's last turn is a text-only answer that says the steps ran out, with no RESULT line."""
    try:
        with open(events, "rb") as fh:
            fh.seek(start or 0)
            lines = fh.read().decode(errors="replace").splitlines()
    except OSError:
        return False
    last = max((i for i, ln in enumerate(lines) if '"type":"step_start"' in ln), default=None)
    if last is None:
        return False
    text = []
    for ln in lines[last:]:
        try:
            ev = json.loads(ln)
        except ValueError:
            continue
        if ev.get("type") == "tool_use":
            return False
        if ev.get("type") == "text":
            text.append((ev.get("part") or {}).get("text") or "")
    said = "".join(text)
    return bool(STOP_SAYS_LIMIT.search(said[:800])) and not re.search(r"^RESULT: ", said, re.M)


# ----------------------------------------------------------------------------- the nudges' watchers

def tree_changed(git: TreeGit, targets: list[str], accept: Callable[[str], bool] = lambda path: True) -> bool:
    """A target file differs from the base (new files included); with no targets, any changed path that
    `accept`s. If git fails (no base, an error), True: never nudge on a doubt."""
    try:
        paths = git.changed(targets or [])
    except (subprocess.CalledProcessError, OSError):
        return True
    return bool(targets and paths) or any(accept(x) for x in paths)


def interruptions(segments: list[dict]) -> int:
    """Segments that continued after an interruption: every later one, except after a nudge's stop."""
    ours = ("first_edit_nudge", "loop_nudge", "step_continue")
    return max(0, len(segments) - 1 - sum(1 for s_ in segments if any(s_.get(k) for k in ours)))


class FirstEditWatch:
    """Follows a round's events (run_segment polls it): `poll` is True when the segment should stop for the
    nudge, at the first moment after `step` finished steps with no target changed when no step is open (the
    last event is a step_finish: every tool call of the session has ended), or `grace` steps later.
    `changed()` says whether the model has changed its targets."""

    def __init__(self, events: Path, step: int, changed: Callable[[], bool], targets: list[str] = (),
                 grace: int = DEFAULT_TIMING.first_edit_grace):
        self.events, self.step, self.changed, self.targets, self.grace = events, step, changed, list(targets), grace
        self.offset = self.steps = 0
        self.last = None
        self.edited = False

    def read(self):
        try:
            with open(self.events, "rb") as fh:
                fh.seek(self.offset)
                data = fh.read()
        except OSError:
            return
        end = data.rfind(b"\n") + 1
        self.offset += end
        for m in STEP_TS.finditer(data[:end]):
            self.last = m.group(1)
            self.steps += m.group(1) == b"step_finish"

    @property
    def near(self) -> bool:
        return not self.edited and self.steps >= self.step

    def poll(self) -> bool:
        if self.edited:
            return False
        self.read()
        if self.steps < self.step:
            return False
        if self.changed():
            self.edited = True
            return False
        return self.last == b"step_finish" or self.steps >= self.step + self.grace


class LoopWatch:
    """Follows a round's events from `offset` (run_segment polls it): `poll` is True once a command has come
    back the same `repeats` times, when no step is open, or `grace` steps later."""

    def __init__(self, events: Path, offset: int = 0, repeats: int = loop_guard.REPEATS,
                 grace: int = DEFAULT_TIMING.loop_grace):
        self.events, self.offset, self.repeats, self.grace = events, offset, repeats, grace
        self.tracker = loop_guard.Tracker(repeats)
        self.steps = self.hit_at = 0
        self.last = self.signature = None

    def read(self):
        try:
            with open(self.events, "rb") as fh:
                fh.seek(self.offset)
                data = fh.read()
        except OSError:
            return
        end = data.rfind(b"\n") + 1
        self.offset += end
        for line in data[:end].splitlines():
            m = STEP_TS.match(line)
            if m:
                self.last = m.group(1)
                self.steps += m.group(1) == b"step_finish"
            elif self.signature is None and b'"tool_use"' in line:
                try:
                    sig = self.tracker.see(json.loads(line))
                except ValueError:
                    continue
                if sig:
                    self.signature, self.hit_at = sig, self.steps

    @property
    def near(self) -> bool:
        return self.signature is not None

    def poll(self) -> bool:
        self.read()
        return self.near and (self.last == b"step_finish" or self.steps >= self.hit_at + self.grace)

    def record(self) -> dict:
        return {"fired": True, "signature": self.signature, "count": self.repeats, "at_step": self.steps}


# ----------------------------------------------------------------------------- time, errors, checkpoints

def model_time_s(events: Path, start: int = 0, now: float | None = None) -> float:
    """Seconds the model worked in events.jsonl from byte offset `start`: per step, the last
    step_start before its step_finish to that step_finish (a request retried after a queue timeout
    has an earlier step_start of its own). With `now`, a step still open counts up to now, unless an
    endpoint error came after its start: an attempt that failed is the endpoint's, not the model's, and
    the wait for opencode's retry is not model time either. A step left open by a stop (no step_finish
    before the next step_start) counts nothing here; the stopped segment's `open_step_s` holds it
    (model_used_s)."""
    try:
        with open(events, "rb") as fh:
            fh.seek(start)
            data = fh.read()
    except OSError:
        return 0.0
    total, open_ts = _closed_and_open(data)
    if now is not None and open_ts is not None:
        total += max(0.0, now - open_ts)
    return total


def _closed_and_open(data: bytes, pos: int = 0, endpos: int | None = None) -> tuple[float, float | None]:
    """The finished steps' seconds in data[pos:endpos], and the start of a step still open at its end
    (None when the last step finished, or an error event came after its start)."""
    total, open_ts, failed = 0.0, None, False
    for m in STEP_ERR_TS.finditer(data, pos, len(data) if endpos is None else endpos):
        ts = int(m.group(2)) / 1000
        if m.group(1) == b"step_start":
            open_ts, failed = ts, False
        elif m.group(1) == b"error":
            failed = True                  # a step that still finishes counts as before
        elif open_ts is not None:
            total += max(0.0, ts - open_ts)
            open_ts = None
    return total, None if failed else open_ts


def open_step_s(events: Path, start: int = 0, now: float | None = None) -> float:
    """Seconds, up to `now`, of the step still open at the end of events.jsonl from byte offset `start`
    (0 when the last step finished). run_segment records it for the step its segment's stop interrupted,
    which model_time_s alone never counts afterwards (the next step_start supersedes it)."""
    if now is None:
        return 0.0
    return max(0.0, model_time_s(events, start, now) - model_time_s(events, start))


def model_used_s(events: Path, start: int, segments: list[dict]) -> float:
    """The model time spent from byte offset `start`: its finished steps, and the steps that the segments
    from there were stopped in the middle of (their `open_step_s`). A segment's open step counts only if
    its events still end in an open step (a run that died after a checkpoint may have finished it, and
    then model_time_s counts it)."""
    total = model_time_s(events, start)
    segs = [sg for sg in segments if sg.get("events_offset", -1) >= start]
    if not any(sg.get("open_step_s") for sg in segs):
        return total
    try:
        data = events.read_bytes()
    except OSError:
        return total
    starts = sorted({sg["events_offset"] for sg in segs})
    for sg in segs:
        if not sg.get("open_step_s"):
            continue
        o = sg["events_offset"]
        end = next((x for x in starts if x > o), len(data))
        if _closed_and_open(data, o, end)[1] is not None:
            total += sg["open_step_s"]
    return total


def stall_limit(last_event: str, timing: Timing = DEFAULT_TIMING) -> float:
    """Seconds of silence after an error event before run_segment treats the run as hung."""
    return timing.stall_after_preempt_s if PREEMPTED in last_event else timing.stall_after_error_s


def recovered_after_error(events_tail: str) -> bool:
    """True if opencode went on with a new step after the last error event of the segment: it retried
    the request itself (a preemption, a dropped stream), and its exit code 1 only reports that an error
    happened (opencode v2 `run --format json` sets it on any failed step, also one it continued)."""
    lines = events_tail.splitlines()
    errs = [i for i, l in enumerate(lines) if '"type":"error"' in l]
    return bool(errs) and any('"type":"step_start"' in l for l in lines[errs[-1] + 1:])


def gateway_interrupted(seg: dict, tail: str) -> bool:
    """Whether a segment that ended (exit code, stall) on an endpoint error should be resumed.
    Not when opencode itself went on after the error (it retried, e.g. after a preemption)."""
    return bool(not seg["suspend_interrupted"] and not seg["timed_out"]
                and (seg["exit"] != 0 or seg.get("stalled_after_error"))
                and (seg.get("stalled_after_error") or not recovered_after_error(tail))
                and GATEWAY_ERROR.search(tail))


def checkpoint_diff(raw: Path, git: TreeGit) -> int:
    """The model's edits so far (against the base, new files included) to raw/model.diff, written
    atomically. Host-side git never uses the tree's own .git (hostgit). Returns the diff's size in
    bytes, or -1 if it could not be made (no base, a git error)."""
    try:
        d = git.diff()
    except (subprocess.CalledProcessError, OSError):
        return -1
    _atomic_write(raw / "model.diff", d)
    return len(d)


def _atomic_write(path: Path, text: str | bytes):
    tmp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    if isinstance(text, bytes):
        tmp.write_bytes(text)
    else:
        tmp.write_text(text)
    tmp.replace(path)


def step_segments(segments: list[dict], round_no: int, raw: Path | None = None) -> list[dict]:
    """Segments that recorded where they began (`events_offset`, `started_at`), for stepmetrics: this
    round's, and with `raw` the earlier rounds' (run.round<N>.json is round N). A segment after the first
    of its round continued an interrupted session (`resumed`)."""
    rounds = []
    if raw is not None:
        for r in range(1, round_no):
            try:
                rounds.append((r, json.loads((raw / f"run.round{r}.json").read_text()).get("segments") or []))
            except (OSError, ValueError):
                pass
    rounds.append((round_no, segments))
    return [{"offset": s["events_offset"], "t0": s["started_at"], "resumed": i > 0, "round": r}
            for r, segs in rounds for i, s in enumerate(segs)
            if s.get("events_offset") is not None and s.get("started_at")]


def read_lockwait(home: Path, rel: str = LOCKWAIT) -> list[tuple[int, int]]:
    """(asked, got) epoch seconds from the build-slot log in the sandbox HOME (read without symlinks)."""
    try:
        text = safefs.read_text(home, rel, limit=2**20)
    except (OSError, ValueError):
        return []
    out = []
    for line in text.splitlines():
        a = line.split()
        if len(a) >= 2 and a[0].isdigit() and a[1].isdigit():
            out.append((int(a[0]), int(a[1])))
    return out


def write_steps(raw: Path, task_id: str, segs: list[dict], lockwait: list[tuple[int, int]] | None = None,
                log: Callable[[str], None] = print):
    """Append the steps finished so far to raw/steps.jsonl (no step twice, also after a resume).
    Never stops the run."""
    try:
        stepmetrics.append_new(raw, task_id, segs, lockwait=lockwait)
    except Exception as e:  # noqa: BLE001
        log(f"steps.jsonl not updated: {type(e).__name__}: {e}")


def write_partial_run(raw: Path, meta: dict):
    """run.json while the session runs (`partial: true`); run_session overwrites it when the run ends.
    It carries what a resume reads: the session id and the segments so far, the running one marked as
    interrupted with its awake time up to now."""
    _atomic_write(raw / "run.json", json.dumps({**meta, "partial": True,
                                                 "updated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z")}, indent=1))


# ----------------------------------------------------------------------------- one segment

def run_segment(cmd: list[str], raw: Path, active_left_s: float, on_tick=None,
                model_left_s: float | None = None, nudge: FirstEditWatch | None = None,
                loop: LoopWatch | None = None, *, timing: Timing = DEFAULT_TIMING,
                before: Callable[[], object] | None = None, stop: Callable[[], object] | None = None,
                log: Callable[[str], None] = print) -> dict:
    """Run one opencode invocation (`cmd`), its stdout appended to raw/events.jsonl. `active_left_s` caps
    awake (monotonic) time; `model_left_s` (if given) caps the model's own time in this segment
    (model_time_s), the time cap proper. `nudge` (if given) can stop the segment for the first-edit nudge
    (`first_edit_nudge` in the result), `loop` (if given) for the stuck-loop nudge (`loop_nudge`).
    On a suspend (the wall clock jumps past the monotonic clock) the run is stopped after wake-up so the
    session can be continued; the model's edits in the tree are the checkpoint. `before()` runs first
    (remove a leftover container); `stop()` stops the container (default: terminate the process)."""
    if before:
        before()
    m0, w0 = time.monotonic(), time.time()
    mp, wp = m0, w0
    start_size = (raw / "events.jsonl").stat().st_size if (raw / "events.jsonl").exists() else 0
    suspended = 0.0
    seg = {"timed_out": False, "suspend_interrupted": False}
    with open(raw / "events.jsonl", "a") as out, open(raw / "opencode.stderr.log", "a") as err:
        p = subprocess.Popen(cmd, stdout=out, stderr=err)
        while True:
            try:
                p.wait(timeout=min(timing.poll_s, timing.first_edit_poll_s)
                       if (nudge and nudge.near) or (loop and loop.near) else timing.poll_s)
                break
            except subprocess.TimeoutExpired:
                pass
            if nudge and nudge.poll():
                seg["first_edit_nudge"] = True
                break
            if loop and loop.poll():
                seg["loop_nudge"] = True
                break
            mn, wn = time.monotonic(), time.time()
            jump = (wn - wp) - (mn - mp)
            mp, wp = mn, wn
            if jump > timing.suspend_jump_s:
                suspended += jump
                seg["suspend_interrupted"] = True
                log(f"machine was suspended for {jump / 60:.1f} min; checkpointing and continuing the session")
                break
            if mn - m0 >= active_left_s:
                seg["timed_out"] = True
                seg["cap"] = "awake ceiling" if model_left_s is not None else "time"
                break
            if model_left_s is not None and model_time_s(raw / "events.jsonl", start_size, time.time()) >= model_left_s:
                seg["timed_out"] = True
                seg["cap"] = "time"
                break
            if on_tick:
                on_tick(mn - m0)
            # opencode can hang after a stream error ("stream ended without finish_reason"): an error
            # event followed by silence is treated as an endpoint interruption. Only events written
            # during THIS segment count (a resumed session starts with the previous segment's error as
            # the file's last line).
            ev = raw / "events.jsonl"
            size = ev.stat().st_size
            if size > start_size and time.time() - ev.stat().st_mtime > timing.stall_after_error_s:
                with open(ev, "rb") as fh:
                    fh.seek(max(start_size, size - 3000))
                    last = fh.read().decode(errors="replace").strip().splitlines()[-1:]
                if last and '"type":"error"' in last[0] and time.time() - ev.stat().st_mtime > stall_limit(last[0], timing):
                    seg["stalled_after_error"] = True
                    break
        if p.poll() is None:
            if stop:
                stop()
            else:
                p.terminate()
            try:
                p.wait(timeout=60)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
    end = time.time()
    seg.update({"exit": p.returncode, "active_s": round(time.monotonic() - m0),
                "wall_s": round(end - w0), "suspended_s": round(suspended),
                "model_s": round(model_time_s(raw / "events.jsonl", start_size, end)),
                # the step the stop interrupted, without the time the machine slept: the time cap counts it
                "open_step_s": round(max(0.0, open_step_s(raw / "events.jsonl", start_size, end) - suspended), 1)})
    # A suspend can also surface only after the fact, as opencode aborting the session.
    with open(raw / "events.jsonl", "rb") as fh:
        fh.seek(start_size)
        tail = fh.read().decode(errors="replace")[-5000:]   # this segment's events only
    if not seg["suspend_interrupted"] and "Session interrupted: inactivity" in tail:
        seg["suspend_interrupted"] = True
    if gateway_interrupted(seg, tail):
        seg["gateway_interrupted"] = True
    return seg


# ----------------------------------------------------------------------------- statistics

def session_usage(path: Path) -> dict:
    """Sum per-message token usage from an opencode session export (as the endpoint reported it)."""
    try:
        d = json.loads(path.read_text())
    except (json.JSONDecodeError, OSError):
        return {}
    tot = {"assistant_messages": 0, "prompt": 0, "completion": 0, "reasoning": 0,
           "cached_read": 0, "max_prompt": 0}
    for msg in d.get("messages", []):
        info = msg.get("info", msg)
        tk = info.get("tokens")
        if not tk:
            continue
        tot["assistant_messages"] += 1
        tot["prompt"] += tk.get("input", 0)
        tot["completion"] += tk.get("output", 0)
        tot["reasoning"] += tk.get("reasoning", 0)
        cr = tk.get("cache", {}).get("read", 0)
        tot["cached_read"] += cr
        tot["max_prompt"] = max(tot["max_prompt"], tk.get("input", 0) + cr)
    return tot


def event_stats(path: Path) -> dict:
    """Steps, tool calls, tokens, errors and the shell commands that look like network use."""
    steps = tools = 0
    tin = tout = treason = tcache = 0
    errors = []
    shell_cmds = []
    for line in path.read_text(errors="replace").splitlines():
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        typ = ev.get("type")
        part = ev.get("part", {})
        if typ == "step_finish":
            steps += 1
            tk = part.get("tokens", {})
            tin += tk.get("input", 0)
            tout += tk.get("output", 0)
            treason += tk.get("reasoning", 0)
            tcache += tk.get("cache", {}).get("read", 0)
        elif typ == "tool_use":
            tools += 1
            inp = part.get("state", {}).get("input", {})
            if isinstance(inp, dict) and "command" in inp:
                shell_cmds.append(str(inp["command"]))
        elif typ == "error":
            errors.append(json.dumps(ev)[:500])
    net = [c[:300] for c in shell_cmds if NET_RE.search(c)]
    return {"steps": steps, "tool_calls": tools, "tokens_in": tin, "tokens_out": tout,
            "tokens_reasoning": treason, "tokens_cache_read": tcache, "errors": errors[:20],
            "shell_commands": len(shell_cmds), "network_like_commands": net}


# ----------------------------------------------------------------------------- the session

@dataclass
class Session:
    """One task's session, as the project sets it up.

    Required: the paths and the two callables.
        command(message, session_id) -> argv   one segment: opencode in the sandbox, events on stdout
        prompt(steps, minutes) -> str          the first message
    `sandbox` (sandbox.container.Sandbox, or anything with the same methods) gives `command`, `before`,
    `stop`, `export` and `close` in one object; explicit callables win over it.

    Paths. `tree` and `home` are the model's (mounted into its container): the host reads and writes them
    only through safefs and hostgit. `raw` (evidence) and `state` (the tree's base snapshot, temporary
    copies) are host-only. Two sessions never share any of the four.
    """
    task_id: str
    tree: Path
    raw: Path
    home: Path
    state: Path
    prompt: Callable[[int, int], str]
    command: Callable[[str, str | None], list[str]] | None = None
    sandbox: object | None = None
    prompts: Prompts = field(default_factory=Prompts)
    timing: Timing = field(default_factory=Timing)
    provider: Provider = field(default_factory=Provider)
    model_options: dict | None = None
    first_edit_step: int | None = None            # None: no first-edit nudge
    first_edit_targets: list[str] = field(default_factory=list)
    first_edit_accept: Callable[[str], bool] = lambda path: True   # with no targets: which changes count
    loop_guard: bool = True
    before: Callable[[], object] | None = None   # remove a leftover container before a segment
    stop: Callable[[], object] | None = None     # stop the running one
    export: Callable[[str], str | None] | None = None     # session id -> the session export (JSON text)
    close: Callable[[], object] | None = None    # after the session (remove its network, volumes)
    collect: tuple[str, ...] = ()                 # files in `home` copied to `raw` at the end (safefs)
    meta: Callable[[], dict] | None = None        # the project's fields for run.json
    context: Callable[[str, str | None, str], tuple[int, str]] = gateway_context
    # base URL -> the URL in opencode's config, when something outside the model's container adds the key
    # (the bundled sandbox's forwarder): then the config holds no key path at all
    model_url: Callable[[str], str] | None = None
    usage: Callable[[Path], dict] = session_usage
    segment: Callable[..., dict] | None = None    # override run_segment (tests)
    log: Callable[[str], None] | None = None

    def __post_init__(self):
        sb = self.sandbox
        if sb is not None:
            self.command = self.command or sb.command
            self.before = self.before or sb.before
            self.stop = self.stop or sb.stop
            self.export = self.export or sb.export
            self.close = self.close or sb.close
            self.model_url = self.model_url or getattr(sb, "model_url", None)
        if self.command is None:
            raise ValueError("Session needs a command or a sandbox")

    @property
    def git(self) -> TreeGit:
        return TreeGit(self.tree, self.state / "git")

    def say(self, msg: str):
        (self.log or (lambda m: print(f"[{self.task_id}] {m}", flush=True)))(msg)

    def first_edit_watcher(self, events: Path, step: int) -> FirstEditWatch:
        git = self.git
        return FirstEditWatch(events, step, lambda: tree_changed(git, self.first_edit_targets, self.first_edit_accept),
                              self.first_edit_targets, self.timing.first_edit_grace)


def snapshot_base(tree: Path, state: Path) -> str:
    """Record the prepared tree as the base that checkpoints and the first-edit check compare with.
    Call it at the end of a project's prepare, before the model has run."""
    return TreeGit(tree, Path(state) / "git").init_base()


_HELD: dict[str, list] = {}           # evidence directory -> [owning thread, depth]
_HELD_LOCK = threading.Lock()


@contextmanager
def task_lock(raw: Path):
    """One owner per evidence directory at a time, across processes and threads. Re-entrant for the
    thread that holds it, so run_task can hold it from prepare to the end and run_session inside it."""
    key, me = str(Path(raw).resolve()), threading.get_ident()
    with _HELD_LOCK:
        held = _HELD.get(key)
        if held and held[0] != me:
            raise RuntimeError(f"another session is running in {raw}")
        if held:
            held[1] += 1
    if held:
        try:
            yield
        finally:
            with _HELD_LOCK:
                held[1] -= 1
        return
    raw.mkdir(parents=True, exist_ok=True)
    # the lock is on the evidence directory itself, not on a file in it: nothing that moves or archives
    # files in the directory (a retry's archive_try) can move the lock away from under its holder
    fd = os.open(raw, os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_CLOEXEC", 0))
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError(f"another session is running in {raw}") from None
        with _HELD_LOCK:
            _HELD[key] = [me, 1]
        try:
            yield
        finally:
            with _HELD_LOCK:
                _HELD.pop(key, None)
    finally:
        os.close(fd)


# what a resume or a follow-up reads from run.json; a record without one is from an older harness
RESUME_FIELDS = ("session", "segments", "step_cap", "wall_cap_min", "events_start")
FOLLOWUP_FIELDS = ("followup_round", "base_steps", "round_tools")


def _is_followup(prev: dict) -> bool:
    """A follow-up round's record: any of its fields, or a pending first message, says so (then all of
    its fields are needed, so a record missing `followup_round` is not taken for round 1)."""
    return any(k in prev for k in FOLLOWUP_FIELDS + ("pending_message",))


def _require(s: "Session", prev: dict, fields: tuple[str, ...]):
    missing = [k for k in fields if k not in prev]
    if missing:
        raise SystemExit(f"[{s.task_id}] run.json from an older harness (no {', '.join(missing)}): "
                         "start the task fresh")


def _ungzip_events(raw: Path):
    if (raw / "events.jsonl.gz").exists() and not (raw / "events.jsonl").exists():
        (raw / "events.jsonl").write_bytes(gzip.open(raw / "events.jsonl.gz").read())
        (raw / "events.jsonl.gz").unlink()


def _size(p: Path) -> int:
    return p.stat().st_size if p.exists() else 0


def run_session(s: Session, base_url: str, key_file: str | None, model: str, wall_min: int, steps: int,
                resume: bool = False, followup: str | None = None, retry: bool = False) -> dict:
    """Run (or continue) the session; returns run.json's content, which is also written to s.raw.
    `retry` (with resume): the round runs again after the endpoint's failures consumed it, with its caps
    counted afresh from here (once; the round engine decides when)."""
    with task_lock(s.raw):
        try:
            return _run_session(s, base_url, key_file, model, wall_min, steps, resume, followup, retry)
        finally:
            try:
                if s.close:
                    s.close()
            finally:
                n = evidence.redact_key(s.raw, key_file)
                if n:
                    s.say(f"SECURITY: the endpoint's key was in {n} evidence file(s); masked")


def _run_session(s: Session, base_url, key_file, model, wall_min, steps, resume, followup, retry=False) -> dict:
    raw, home, T = s.raw, s.home, s.timing
    events = raw / "events.jsonl"
    if not home.is_dir():
        raise SystemExit(f"[{s.task_id}] not prepared")
    s.state.mkdir(parents=True, exist_ok=True)
    if s.before:
        # a container left by an earlier run (the harness stopped, its container did not) goes first: it
        # could still change the tree and HOME this run reads, edits and checkpoints, also when no segment
        # runs (a spent budget)
        s.before()
    git = s.git
    segments: list[dict] = []
    sid = None
    base_steps = base_tools = 0   # at this round's start (steps) and at this run's start (tool calls)
    round_tools = 0               # tool calls at this round's start (recorded, so a resume knows it)
    fu_round = None      # the follow-up round this run is (1, 2, ...), also when resuming one
    nudged = None        # the first-edit nudge's record, kept from the first round to the last
    looped = None        # this round's stuck-loop nudge (once per round; a follow-up has its own)
    told_on = False      # this round's step-limit stop was answered once (step_continue)
    notice: dict = {}    # what clear_step_notice did at this round's start
    ev_start = 0         # byte offset in events.jsonl where this round's events begin
    budget = None        # a retry's own start: {"segment", "events_offset", "steps"} (the caps count from it)

    def step_reset(message: str) -> str:
        return with_step_reset(message, events, steps, home, notice, s.prompts,
                               clear=lambda h: clear_step_notice(h, s.prompts, s.state))
    if followup:
        # A review round: continue the finished session with the review message, under a fresh time/step
        # budget. The round number comes from run.json itself (the last round's), so a stop between the
        # files below and the round's first checkpoint repeats the same round, never skips one.
        prev = json.loads((raw / "run.json").read_text())
        if prev.get("partial") or not prev.get("finished_at"):
            raise SystemExit(f"[{s.task_id}] the last round has not finished; resume it first")
        _require(s, prev, RESUME_FIELDS + ("tool_calls", "steps") + (FOLLOWUP_FIELDS if _is_followup(prev) else ()))
        sid = prev["session"]
        nudged = prev.get("first_edit_nudge")
        fu_round = (prev.get("followup_round") or 0) + 1
        _atomic_write(raw / f"run.round{fu_round}.json", json.dumps(prev, indent=1))
        _ungzip_events(raw)
        st0 = event_stats(events)
        base_steps, base_tools = st0["steps"], st0["tool_calls"]
        round_tools = base_tools
        ev_start = _size(events)
        message = step_reset(followup)
        _atomic_write(raw / f"followup{fu_round}.md", message)
        # the new round is on record before anything runs: a stop from here on resumes this round, with
        # its own caps, and its first message is still to be sent (`pending_message`)
        write_partial_run(raw, {"task": s.task_id, "model": model, "base_url": base_url, "session": sid,
                                "wall_cap_min": wall_min, "step_cap": steps, "events_start": ev_start,
                                **({"first_edit_nudge": nudged} if nudged else {}),
                                "followup_round": fu_round, "base_steps": base_steps, "round_tools": round_tools,
                                "segments": [], "pending_message": message})
        resume = True
    elif resume:
        # Continue an interrupted run: its events, session and checkpointed tree are kept. A record without
        # a field the resume needs is refused, never defaulted (fields that say something happened, a
        # nudge, a retry, a pending message, are absent when it did not).
        prev = json.loads((raw / "run.json").read_text())
        _require(s, prev, RESUME_FIELDS + (FOLLOWUP_FIELDS if _is_followup(prev) else ()))
        sid = prev["session"]
        nudged = prev.get("first_edit_nudge")
        looped = prev.get("loop_nudge")
        told_on = bool(prev.get("step_continue"))
        _ungzip_events(raw)
        # everything about the round comes from its record, not from this call: its caps, where it began
        segments = prev["segments"]
        steps, wall_min = prev["step_cap"], prev["wall_cap_min"]
        if prev.get("step_notice"):
            notice.update(prev["step_notice"])
        if prev.get("followup_round"):
            # an interrupted follow-up round: its own budget, counted from its start
            fu_round = prev["followup_round"]
            base_steps = prev["base_steps"]
            ev_start = prev["events_start"]
            round_tools = prev["round_tools"]
        base_tools = event_stats(events)["tool_calls"]
        if prev.get("pending_message") and not segments:
            message = prev["pending_message"]        # the round never started: its own first message
        else:
            message = step_reset(s.prompts.resume_after(bool(segments and segments[-1].get("gateway_interrupted"))))
        budget = prev.get("retry_from")
        if retry:
            budget = {"segment": len(segments), "events_offset": _size(events), "steps": event_stats(events)["steps"]}
    else:
        # A fresh start needs a freshly prepared tree: the base the project recorded (snapshot_base), or,
        # when it recorded none, the tree as it is now.
        if git.has_base():
            try:
                dirty = bool(git.changed())
            except (subprocess.CalledProcessError, OSError):
                dirty = True
            if dirty:
                raise SystemExit(f"[{s.task_id}] the tree differs from its prepared base; prepare it again")
        else:
            git.init_base()
        message = s.prompt(steps, wall_min)
        _atomic_write(raw / "prompt.md", message)
        for f in ("events.jsonl", "opencode.stderr.log", stepmetrics.FILE):
            (raw / f).write_text("")
        (raw / "model.diff").unlink(missing_ok=True)
    s.say(f"run: {model} via {base_url}, cap {wall_min} min of model time (at most "
          f"{T.awake_ceiling * wall_min} min awake) / {steps} steps"
          + (f" (resuming session {sid})" if resume else ""))
    resumes = interruptions(segments) if resume else 0
    round_no = (fu_round or 0) + 1
    nudge_at = s.first_edit_step if fu_round is None and not (nudged or {}).get("fired") else None
    nudge = None
    if nudge_at and nudge_at < steps:        # else the step cap comes first
        nudge = s.first_edit_watcher(events, nudge_at)
        nudged = {"step": nudge_at, "fired": False}
    loop = None
    if s.loop_guard and not looped:
        loop = LoopWatch(events, _size(events), grace=T.loop_grace)
    prior_segs = step_segments([], round_no, raw)       # the earlier rounds' segments
    segment = s.segment or (lambda cmd, raw_, left, **kw: run_segment(
        cmd, raw_, left, timing=T, before=s.before, stop=s.stop, log=s.say, **kw))

    def checkpoint() -> bool:
        if checkpoint_diff(raw, git) < 0:
            s.say("checkpoint: no diff (no base snapshot, or git failed)")
            return False
        return True
    while True:
        used_steps = event_stats(events)["steps"] - (budget["steps"] if budget else base_steps)
        left_steps = steps - used_steps
        if left_steps <= 0:                  # the step cap is spent: no further step, whatever the stops
            break
        ctx, ctx_src = s.context(base_url, key_file, model)
        inner_url, inner_key = (s.model_url(base_url), None) if s.model_url else (base_url, key_file)
        safefs.write_text(home, CONFIG, json.dumps(
            opencode_config(inner_url, inner_key, model, left_steps, s.model_options, ctx, s.provider), indent=1))
        active_used = sum(s_.get("active_s", 0) for s_ in segments[budget["segment"] if budget else 0:])
        model_left = wall_min * 60 - model_used_s(events, budget["events_offset"] if budget else ev_start, segments)
        left = T.awake_ceiling * wall_min * 60 - active_used
        if left <= 0 or model_left <= 0:
            break
        ck = {"sid": sid, "t": 0.0}
        # where this segment's events begin and when it started: run.json keeps both, so the per-step
        # metrics of a resumed session know each segment's start (stepmetrics)
        seg_meta = {"events_offset": _size(events), "started_at": round(time.time(), 3),
                    "context": ctx, "context_source": ctx_src}
        segs_now = prior_segs + step_segments(segments + [seg_meta], round_no)

        def tick(seg_active_s: float, done=list(segments), ck=ck, seg_meta=seg_meta, segs_now=segs_now):
            write_steps(raw, s.task_id, segs_now, read_lockwait(home), s.say)    # each step as it ends
            # run.json as soon as the session has an id, then every checkpoint_s with the diff
            try:
                if ck["sid"] is None:
                    m = re.search(r"\"sessionID\":\"(ses_[A-Za-z0-9]+)\"", events.read_text())
                    if not m:
                        return
                    ck["sid"] = m.group(1)
                elif time.monotonic() - ck["t"] < T.checkpoint_s:
                    return
                ck["t"] = time.monotonic()
                write_partial_run(raw, {
                    "task": s.task_id, "model": model, "base_url": base_url, "session": ck["sid"],
                    "wall_cap_min": wall_min, "step_cap": steps, "events_start": ev_start,
                    **({"first_edit_nudge": nudged} if nudged else {}),
                    **({"loop_nudge": looped} if looped else {}), **({"step_continue": True} if told_on else {}),
                    **({"followup_round": fu_round, "base_steps": base_steps, "round_tools": round_tools} if fu_round else {}),
                    **({"step_notice": notice} if notice else {}),
                    **({"retry_from": budget} if budget else {}),
                    "segments": done + [{"exit": None, "timed_out": False, "suspend_interrupted": True,
                                         "active_s": round(seg_active_s), "wall_s": round(seg_active_s),
                                         "suspended_s": 0, "in_progress": True,
                                         # if the run dies here, a resume still counts the open step
                                         "open_step_s": round(open_step_s(events, seg_meta["events_offset"],
                                                                          time.time()), 1),
                                         **seg_meta}]})
                checkpoint()
            except OSError as e:    # a checkpoint never stops the run
                s.say(f"checkpoint failed: {e}")

        seg = segment(s.command(message, sid), raw, left, on_tick=tick, model_left_s=model_left,
                      **({"nudge": nudge} if nudge else {}), **({"loop": loop} if loop else {}))
        seg.update(seg_meta)
        segments.append(seg)
        write_steps(raw, s.task_id, segs_now, read_lockwait(home), s.say)
        if sid is None:
            m = re.search(r"\"sessionID\":\"(ses_[A-Za-z0-9]+)\"", events.read_text())
            sid = m.group(1) if m else None
        if seg.get("first_edit_nudge") and sid:
            nudged = {"step": nudge.step, "fired": True, "at_step": nudge.steps, "between_steps": nudge.last == b"step_finish",
                      "model_s": round(model_used_s(events, ev_start, segments)), "targets": nudge.targets}
            s.say(f"no target file changed by step {nudge.steps}: first-edit nudge")
            nudge, message = None, s.prompts.first_edit
            continue
        if seg.get("loop_nudge") and sid:
            looped = loop.record()
            s.say(f"`{loop.signature}` repeated at step {loop.steps}: loop nudge")
            message = s.prompts.loop.format(signature=loop.signature, count=loop.repeats)
            loop = None
            continue
        left_now = steps - (event_stats(events)["steps"] - base_steps)
        if (fu_round and sid and not told_on and left_now > 1 and not seg["timed_out"] and seg.get("exit") == 0
                and stopped_on_step_limit(events, ev_start)):
            told_on, seg["step_continue"] = True, True
            s.say(f"the follow-up stopped on the step limit with {left_now} steps left: told to go on")
            message = s.prompts.step_continue.format(left=left_now)
            continue
        if ((seg["suspend_interrupted"] or seg.get("gateway_interrupted")) and not seg["timed_out"]
                and sid and resumes < T.max_resumes):
            resumes += 1
            if seg.get("gateway_interrupted"):
                message = s.prompts.resume_after(True)
                wait = T.gateway_backoff_s * resumes
                s.say(f"gateway error; resuming the session in {wait:.0f}s")
                time.sleep(wait)
            else:
                message = s.prompts.resume
                time.sleep(T.wake_wait_s)   # let the network come back after wake-up
            continue
        break
    # the tree as the session left it: model.diff matches what a check reads. If that fails, an earlier
    # checkpoint's model.diff would pass for the final tree: it is removed, and run.json says so.
    final_diff = checkpoint()
    if not final_diff:
        (raw / "model.diff").unlink(missing_ok=True)
        s.say("final checkpoint failed: model.diff removed (it showed an earlier tree)")
    # export the session (all messages, per-message token usage as reported by the endpoint)
    usage = {}
    if sid and s.export:
        text = s.export(sid)
        if text is not None:
            _atomic_write(raw / "session.json", text)
            usage = s.usage(raw / "session.json")
    for name in s.collect:
        safefs.copy_out(home, name, raw / Path(name).name)
    lock_wait = sum(max(0, b_ - a_) for a_, b_ in read_lockwait(home))
    active = sum(s_.get("active_s", 0) for s_ in segments)
    run_meta = {"task": s.task_id, "model": model, "base_url": base_url, "model_options": s.model_options,
                **(s.meta() if s.meta else {}),
                "active_s": round(active), "wall_s": round(sum(s_.get("wall_s", 0) for s_ in segments)),
                "suspended_s": round(sum(s_.get("suspended_s", 0) for s_ in segments)),
                "resumed": interruptions(segments), "segments": segments,
                "timed_out": any(s_.get("timed_out") for s_ in segments),
                # which cap ended it: "time" (the model's own time reached wall_cap_min) or
                # "awake ceiling" (awake_ceiling x wall_cap_min awake); the step cap shows in `steps`
                "cap": next((s_.get("cap", "time") for s_ in segments if s_.get("timed_out")), None),
                "model_s": round(model_used_s(events, ev_start, segments)),
                "events_start": ev_start, "awake_ceiling_min": T.awake_ceiling * wall_min,
                "exit": segments[-1].get("exit") if segments else None, "session": sid,
                "wall_cap_min": wall_min, "step_cap": steps, "lock_wait_s": lock_wait,
                "first_edit_nudge": nudged, "loop_nudge": looped, "step_continue": told_on,
                **({"model_diff_failed": True} if not final_diff else {}),
                **({"step_notice": notice} if notice else {}),
                **({"retry_from": budget} if budget else {}),
                "finished_at": time.strftime("%Y-%m-%dT%H:%M:%S%z")}
    run_meta.update(event_stats(events))
    # tool calls made in this run (since this call started); and for a follow-up round, in the whole round
    # (from its recorded start, across stops and resumes): none at all is a tooling problem to fix before
    # judging the model
    run_meta["new_tool_calls"] = run_meta["tool_calls"] - base_tools
    if fu_round:
        run_meta["followup_round"] = fu_round
        run_meta["base_steps"] = base_steps
        # a retried round is scored on its own attempt: the failed attempt's steps are not the retry's
        # (`round_steps_all` keeps the whole round's)
        run_meta["round_steps"] = run_meta["steps"] - (budget["steps"] if budget else base_steps)
        if budget:
            run_meta["round_steps_all"] = run_meta["steps"] - base_steps
        run_meta["round_tools"] = round_tools
        run_meta["round_tool_calls"] = run_meta["tool_calls"] - round_tools
        run_meta["followup_no_tool_calls"] = run_meta["round_tool_calls"] == 0
        if run_meta["followup_no_tool_calls"]:
            s.say("TOOLING: the follow-up made no tool call; do not count it against the model")
    run_meta["session_usage"] = usage
    _atomic_write(raw / "run.json", json.dumps(run_meta, indent=1))
    s.say(f"run done: {active / 60:.1f} min awake ({run_meta['suspended_s'] / 60:.0f} min "
          f"suspended, {run_meta['resumed']} resumes), steps {run_meta['steps']}, tokens in/out "
          f"{run_meta['tokens_in']}/{run_meta['tokens_out']}, timeout={run_meta['timed_out']}")
    return run_meta
