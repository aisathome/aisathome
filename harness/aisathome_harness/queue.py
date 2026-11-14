# SPDX-License-Identifier: Apache-2.0
"""A client for a gateway's science task queue: one project's tasks on an aisathome gateway.

    c = QueueClient("https://api.aisathome.org", key_file="~/.config/x/project.key", project="myproject")
    code, body = c.claim("host-1", lease_seconds=600, kinds=["other"])

The gateway URL, the credentials (a key file, read on first use, or the key itself) and the project id are
passed explicitly; nothing is read from the environment. The key goes only into the Authorization header
(never across a redirect) and nothing here prints it.

Every call returns a `Reply`, a (status, body) tuple: `code, body = c.get("t1")`. 2xx is the call's answer,
any other status the gateway's refusal with `{"error": {"message": ...}}` (`Reply.error`). `Reply.check()`
turns a refusal into a typed error (`BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`,
`Unavailable`, all `QueueError`), for callers that prefer exceptions. A network failure raises OSError
(urllib's URLError is one) after `retries` more tries, `retry_wait_s` doubling between them (default: no
retry, the caller decides).

The endpoints (gateway/tasks.go; all with the project's key, except the public queue):

    create(task)                          POST /v1/projects/{id}/tasks (idempotent by the task's id: 201 made, 200 exists)
    list(state, kind, exported)           GET  .../tasks
    get(task)                             GET  .../tasks/{task}             with its spec and result
    claim(worker, lease_seconds, kinds, task, max_class, hold_seconds)
                                          POST .../tasks/claim              200 {task, lease}, 204 nothing queued
    heartbeat(task, worker, lease)        POST .../tasks/{task}/heartbeat   renews the lease (409: lost)
    complete(task, worker, lease, result) POST .../tasks/{task}/complete
    fail(task, worker, lease, reason, requeue, fix)
                                          POST .../tasks/{task}/fail        requeue=True gives it back (its try kept)
    give_back(task, worker, lease, reason) fail with requeue
    cancel(task, reason), set_max_tries(task, n), review(task, verdict, by, ...), decide(task, decision, by, ...),
    exported(task, seen), continue_round(task, by, note)
    public_queue()                        GET  /v1/projects/{id}/queue      no key: the public view, when the project has one

Compatibility: a gateway from before a newer optional result field (`cached_tokens`, 2026-11-14) refuses it
with 400 `unknown field`; `complete` then sends the same result once more without it. `hold_seconds` at a
claim is sent only when given; a gateway from before it (2026-11-14) refuses the field, so `claim` sends the
same claim once more without it and leaves it out from then on (`hold_refused`: the gateway's 10-minute hold
applies).

A model request may name the task it works for in the `TASK_HEADER` header (the gateway's account pages show
it when it is one of the project's running tasks); `task_headers(task)` gives that header.

Standard library only.
"""
from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import NamedTuple

TASK_HEADER = "X-Aisathome-Task"
DEFAULT_LEASE_S = 600
# The gateway's claim classes (taskClass): lower is claimed first.
CLASS_FIX_REVIEW, CLASS_FIX_PREREVIEW, CLASS_FIX_QWEN, CLASS_REVIEW, CLASS_ATTEMPT, CLASS_OTHER = 1, 2, 3, 4, 5, 6
SOURCE_CLASS = {"review": CLASS_FIX_REVIEW, "prereview": CLASS_FIX_PREREVIEW, "prereview-qwen": CLASS_FIX_QWEN}
KIND_CLASS = {"fix_round": CLASS_FIX_REVIEW, "followup": CLASS_ATTEMPT, "review": CLASS_REVIEW,
              "attempt": CLASS_ATTEMPT, "other": CLASS_OTHER}
# Result fields a gateway may not know yet; `complete` drops one it refuses and sends again.
OPTIONAL_RESULT_FIELDS = ("cached_tokens",)
# The gateway's `secretLike` (gateway/projstatus.go): its checkText refuses a reason, outcome, title or note
# that matches (a long run of key characters, and a path of 32 is one, or a key prefix).
SECRET_LIKE = re.compile(r"[A-Za-z0-9_+/=-]{32,}|(?i:\b(?:ak|sk|ghp|gho|ghs|ghu|github_pat|xox[abp])[-_][a-z0-9])"
                         r"|\bAKIA[0-9A-Z]{16}\b|(?i:\bbearer\s)|-----BEGIN", re.ASCII)


def task_class(kind: str, source: str = "") -> int:
    """As the gateway: a fix round's or follow-up's class from its findings source, else from the kind."""
    if source in SOURCE_CLASS and kind in ("fix_round", "followup"):
        return SOURCE_CLASS[source]
    return KIND_CLASS.get(kind, CLASS_OTHER)


def safe_text(text, limit: int) -> str:
    """One line of at most `limit` UTF-8 bytes that the gateway's checkText accepts: control characters
    become spaces, a long path keeps its last part, a key-like run its first 12 characters."""
    def shorter(m: re.Match) -> str:
        run = m.group()
        if len(run) < 32:
            return "[key-like]"
        tail = run.rstrip("/").rsplit("/", 1)[-1]
        return "\u2026/" + tail if "/" in run and len(tail) < 32 else run[:12] + "\u2026"
    t = " ".join(re.sub(r"[\x00-\x1f\x7f]", " ", str(text or "")).split())
    while SECRET_LIKE.search(t):
        t = SECRET_LIKE.sub(shorter, t)
    return t.encode()[:limit].decode(errors="ignore")


def error_text(body) -> str:
    """The gateway's error message in a refusal's body, or ""."""
    e = body.get("error") if isinstance(body, dict) else None
    return (e.get("message") if isinstance(e, dict) else "") or ""


def task_headers(task: str) -> dict:
    """The header a model request names its task with."""
    return {TASK_HEADER: task}


def last_note(task: dict) -> str:
    """The note of a task's newest event ("" without one)."""
    ev = task.get("events") or []
    return (ev[-1].get("note") or "") if ev else ""


# ----------------------------------------------------------------------------- errors

class QueueError(Exception):
    """A call the gateway refused: its status and message."""

    def __init__(self, status: int, message: str = "", body: dict | None = None):
        super().__init__(f"HTTP {status}{(' ' + message) if message else ''}")
        self.status, self.message, self.body = status, message, body or {}


class BadRequest(QueueError):
    """400: a field the gateway does not take, or a value it refuses (also secret-like text)."""


class Unauthorized(QueueError):
    """401: no key, or not a key the gateway knows."""


class Forbidden(QueueError):
    """403: another project's key."""


class NotFound(QueueError):
    """404: no such task (or project)."""


class Conflict(QueueError):
    """409: the lease is lost (expired, cancelled, another worker's) or the task is not in a state for the call."""


class Unavailable(QueueError):
    """503: the queue could not write its state; nothing changed, the call may be repeated."""


ERRORS = {400: BadRequest, 401: Unauthorized, 403: Forbidden, 404: NotFound, 409: Conflict, 503: Unavailable}


class Reply(NamedTuple):
    status: int
    body: dict

    @property
    def ok(self) -> bool:
        return 200 <= self.status < 300

    @property
    def error(self) -> str:
        return error_text(self.body)

    def check(self, *ok: int) -> dict:
        """The body when the status is one of `ok` (default: any 2xx), else the typed error."""
        if (self.status in ok) if ok else self.ok:
            return self.body
        raise ERRORS.get(self.status, QueueError)(self.status, self.error, self.body)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):         # the key never follows a redirect
        return None


# ----------------------------------------------------------------------------- the client

class QueueClient:
    """One project's task queue on a gateway."""

    def __init__(self, gateway: str, key_file: str | Path | None, project: str, timeout: float = 30, *,
                 key: str | None = None, user_agent: str = "aisathome-harness-queue", retries: int = 0,
                 retry_wait_s: float = 1.0, sleep=time.sleep):
        if not project:
            raise ValueError("project is required")
        if key_file is None and key is None:
            raise ValueError("a key or a key file is required")
        self.gateway, self.project = gateway.rstrip("/"), project
        self.base = self.gateway + f"/v1/projects/{urllib.parse.quote(project, safe='')}/tasks"
        self.key_file = Path(key_file).expanduser() if key_file is not None else None
        self.timeout, self.user_agent = timeout, user_agent
        self.retries, self.retry_wait_s, self._sleep = retries, retry_wait_s, sleep
        self._key = key.strip() if key is not None else None
        self._open = urllib.request.build_opener(_NoRedirect()).open
        self.hold_refused = False      # the gateway refused `hold_seconds` once: claims go without it

    def _auth(self) -> str:
        if self._key is None:
            self._key = self.key_file.read_text().strip()
        return self._key

    def _send(self, url: str, method: str, body: dict | None, auth: bool) -> Reply:
        req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None, method=method,
                                     headers={"Content-Type": "application/json", "User-Agent": self.user_agent})
        if auth:
            req.add_unredirected_header("Authorization", "Bearer " + self._auth())
        for i in range(self.retries + 1):
            try:
                with self._open(req, timeout=self.timeout) as r:
                    raw = r.read()
                    return Reply(r.status, json.loads(raw) if raw else {})
            except urllib.error.HTTPError as e:
                try:
                    return Reply(e.code, json.loads(e.read() or b"{}"))
                except ValueError:
                    return Reply(e.code, {})
            except OSError:
                if i >= self.retries:
                    raise
                self._sleep(self.retry_wait_s * (2 ** i))
        raise AssertionError("unreachable")

    def call(self, method: str, path: str = "", body: dict | None = None) -> Reply:
        """One request to the project's tasks (`path` after .../tasks); (status, body)."""
        return self._send(self.base + path, method, body, auth=True)

    @staticmethod
    def _t(task: str) -> str:
        return "/" + urllib.parse.quote(task)

    # ------------------------------------------------------------------ the API, one call each

    def create(self, task: dict) -> Reply:
        return self.call("POST", "", task)

    def list(self, state: str = "", kind: str = "", exported: str = "") -> Reply:
        q = {k: v for k, v in (("state", state), ("kind", kind), ("exported", exported)) if v}
        return self.call("GET", "?" + urllib.parse.urlencode(q) if q else "")

    def get(self, task: str) -> Reply:
        return self.call("GET", self._t(task))

    def claim(self, worker: str, lease_seconds: int = DEFAULT_LEASE_S, kinds: list[str] | None = None,
              task: str | None = None, max_class: int | None = None, hold_seconds: int | None = None) -> Reply:
        """The next task and a lease (204: none), or with `task` that one only (a worker taking its own back).
        `hold_seconds`: how long the task, once given back, waits for this worker (the gateway's default: 10
        minutes); sent only when given, and not after a gateway refused it (`hold_refused`)."""
        body: dict = {"worker": worker, "lease_seconds": lease_seconds}
        if max_class:
            body["max_class"] = max_class
        if task:
            body["task"] = task
        elif kinds:
            body["kinds"] = list(kinds)
        if hold_seconds is None or self.hold_refused:
            return self.call("POST", "/claim", body)
        r = self.call("POST", "/claim", {**body, "hold_seconds": hold_seconds})
        if r.status == 400 and 'unknown field "hold_seconds"' in error_text(r.body):
            # a gateway from before hold_seconds: the same claim without it (nothing was claimed)
            self.hold_refused = True
            r = self.call("POST", "/claim", body)
        return r

    def heartbeat(self, task: str, worker: str, lease: str) -> Reply:
        """Renew the lease (its TTL from now)."""
        return self.call("POST", self._t(task) + "/heartbeat", {"worker": worker, "lease": lease})

    renew = heartbeat

    def complete(self, task: str, worker: str, lease: str, result: dict) -> Reply:
        path = self._t(task) + "/complete"
        r = self.call("POST", path, {"worker": worker, "lease": lease, "result": result})
        for field in OPTIONAL_RESULT_FIELDS:
            code, body = r
            if code == 400 and field in result and f'unknown field "{field}"' in error_text(body):
                # a gateway from before the field: the same result without it
                result = {k: v for k, v in result.items() if k != field}
                r = self.call("POST", path, {"worker": worker, "lease": lease, "result": result})
        return r

    def fail(self, task: str, worker: str, lease: str, reason: str = "", requeue: bool = False,
             fix: dict | None = None) -> Reply:
        return self.call("POST", self._t(task) + "/fail", {"worker": worker, "lease": lease, "reason": safe_text(reason, 500),
                                                            "requeue": requeue, **({"fix": fix} if fix else {})})

    def give_back(self, task: str, worker: str, lease: str, reason: str = "interrupted") -> Reply:
        """The task back to the queue with its try kept (an interruption: a stop, not the task's failure)."""
        return self.fail(task, worker, lease, reason, requeue=True)

    def cancel(self, task: str, reason: str = "") -> Reply:
        return self.call("POST", self._t(task) + "/cancel", {"reason": safe_text(reason, 500)} if reason else {})

    def set_max_tries(self, task: str, max_tries: int) -> Reply:
        return self.call("POST", self._t(task) + "/max_tries", {"max_tries": max_tries})

    def review(self, task: str, verdict: str, by: str, **kw) -> Reply:
        return self.call("POST", self._t(task) + "/review", {"verdict": verdict, "by": by,
                                                              **{k: v for k, v in kw.items() if v is not None}})

    def decide(self, task: str, decision: str, by: str, pr: int | None = None, note: str = "") -> Reply:
        body: dict = {"decision": decision, "by": by}
        if pr:
            body["pr"] = pr
        if note:
            body["note"] = note[:500]
        return self.call("POST", self._t(task) + "/decide", body)

    def exported(self, task: str, seen: str) -> Reply:
        """A finished task is in the project's own ledger (as it was at `seen`, its updated_at)."""
        return self.call("POST", self._t(task) + "/exported", {"seen": seen})

    def continue_round(self, task: str, by: str, note: str = "") -> Reply:
        """Schedule the round waiting on a task; the gateway has the recipe, so it is not sent."""
        return self.call("POST", self._t(task) + "/continue", {"created_by": by, **({"note": note} if note else {})})

    def public_queue(self) -> Reply:
        """The project's public queue view, without the key (404 when the project has none)."""
        return self._send(self.gateway + f"/v1/projects/{urllib.parse.quote(self.project, safe='')}/queue", "GET",
                          None, auth=False)
