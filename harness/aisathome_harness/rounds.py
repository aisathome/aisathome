# SPDX-License-Identifier: Apache-2.0
"""Fix rounds: check -> feedback -> resume, until the task is proved, out of rounds, or not fixable.

A task's first round is a session (loop.run_session). After it, the project's check gives a verdict.
When the verdict is fixable and rounds are left, the project's feedback becomes the next message in the
same session (a follow-up round, with its own step and time caps), and the check runs again.

Infrastructure is kept apart from the model. A round that the endpoint's failures consumed (its error
events and fewer than `infra_max_steps` steps) is `unscored: "infra"`: it does not count against the
model and is run again once after the endpoint answers again (round 1 from scratch, with the failed
try moved aside; a later round continued in its session with its caps counted afresh, `Rounds.retry`).
Other unscored kinds (a project's own, e.g. a request the endpoint refused by policy) end the task as not
scored. A follow-up that made no tool call never ran: a tooling problem, not a verdict on the model, so
it runs again once like an infrastructure round (trial 4's C12 lost its last round to one), and a second
one ends the task not scored (`unscored: "no_tool_calls"` in `final`).

`run_rounds` is idempotent: its state is <raw>/rounds.json (evidence.py), and the same call continues
after a stop at any point. Standard library only.
"""
from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from . import evidence

# Endpoint and server failures in opencode's error events. opencode v2 writes the HTTP status as
# `"status":503` (provider errors); the AI SDK's APIError carries it as `"statusCode":503`: both count.
INFRA_ERROR = re.compile(r'no node serving|no trusted node|all nodes serving|provider\.internal|'
                         r'"status(?:Code)?":\s*50[234]\b|ECONNRESET|socket connection was closed|Connection lost')
INFRA_MAX_STEPS = 5


def round_errors(raw: Path, rm: dict, refusal: str | None = None) -> dict:
    """The round's error events (from its events_start, or for a retried round from the retry's own start,
    `retry_from`): {"gateway": n, "refused": n}; `refusal` is a text that marks the endpoint refusing a
    request by policy (counted apart)."""
    out = {"gateway": 0, "refused": 0}
    start = (rm.get("retry_from") or {}).get("events_offset", rm.get("events_start")) or 0
    try:
        with open(raw / evidence.EVENTS, "rb") as fh:
            fh.seek(start)
            lines = fh.read().decode(errors="replace").splitlines()
    except OSError:
        return out
    for line in lines:
        if '"error"' not in line:
            continue
        try:
            if json.loads(line).get("type") != "error":
                continue
        except (ValueError, AttributeError):
            continue
        if refusal and refusal in line:
            out["refused"] += 1
        elif INFRA_ERROR.search(line):
            out["gateway"] += 1
    return out


def classify_round(raw: Path, rm: dict, outcome: str, refusal: str | None = None,
                   refused_kind: str = "refused_by_endpoint", infra_max_steps: int = INFRA_MAX_STEPS,
                   proved: str = "proved") -> dict:
    """{"unscored": kind or None, "gateway_errors", "refusals", "round_steps"}. `refused_kind` when the
    endpoint refused by policy; "infra" when endpoint errors consumed the round; None (scored) otherwise.
    A proved round is always scored. A retried round is judged on its retry alone (run.json's
    `round_steps` and the errors from `retry_from`), never on the failed attempt before it."""
    errs = round_errors(raw, rm, refusal)
    steps = rm.get("round_steps", rm.get("steps")) or 0
    kind = None
    if outcome != proved:
        if errs["refused"]:
            kind = refused_kind
        elif errs["gateway"] and steps < infra_max_steps:
            kind = "infra"
    return {"unscored": kind, "gateway_errors": errs["gateway"], "refusals": errs["refused"], "round_steps": steps}


def gateway_probe(base_url: str, key_file: str | None, model: str, timeout: float = 60) -> bool:
    """One tiny agent-shaped request (a tool definition, max_tokens 1) to the endpoint: True on 200.
    The key goes in a header only, never on a redirect, and is never printed."""
    body = {"model": model, "max_tokens": 1, "messages": [{"role": "user", "content": "ping"}],
            "tools": [{"type": "function", "function": {"name": "noop", "description": "does nothing",
                                                        "parameters": {"type": "object", "properties": {}}}}]}
    req = urllib.request.Request(base_url.rstrip("/") + "/chat/completions", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    if key_file:
        # unredirected: a redirect (to another host, or to plain HTTP) never carries the key
        req.add_unredirected_header("Authorization", "Bearer " + Path(key_file).read_text().strip())
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status == 200
    except (urllib.error.URLError, OSError, ValueError):
        return False


def wait_for_gateway(probe: Callable[[], bool], poll_s: int = 300, max_wait_s: int = 3 * 3600,
                     sleep=time.sleep) -> bool:
    """Poll `probe()` until it answers, at most `max_wait_s`. False when it never did."""
    t0 = time.monotonic()
    while True:
        if probe():
            return True
        if time.monotonic() - t0 + poll_s > max_wait_s:
            return False
        print(f"gateway does not answer; trying again in {poll_s} s", flush=True)
        sleep(poll_s)


@dataclass
class Rounds:
    """What the engine needs from a project for one task.

    start()          round 1: start the session (or start it over after `archive`)
    resume()         continue the current round's session (interrupted, or a round consumed by errors)
    followup(msg)    a fix round: continue the session with this message
    check(rm, n)     the verdict on round n as a record: {"outcome", "unscored", ...} (run.json is `rm`)
    feedback(rec, n, total) -> str | None    the next round's message; None: not fixable, stop
    """
    raw: Path
    fix_rounds: int
    start: Callable[[], None]
    resume: Callable[[], None]
    followup: Callable[[str], None]
    check: Callable[[dict, int], dict]
    feedback: Callable[[dict, int, int], str | None]
    infra_retry: bool = True
    proved: str = "proved"
    retry_kinds: tuple[str, ...] = ("infra",)
    archive: Callable[[int], object] | None = None   # default: evidence.archive_try(raw, k)
    retry: Callable[[], None] | None = None          # a later round again, its caps afresh (default: resume)
    probe: Callable[[], bool] | None = None          # is the endpoint up?
    room: Callable[[], None] | None = None           # wait for room (a queue's courtesy) before a fix round
    wait: Callable[[Callable[[], bool]], bool] = wait_for_gateway
    label: str = ""


def run_rounds(r: Rounds) -> dict:
    """The task to its end; returns rounds.json's content (with `final`)."""
    raw = r.raw
    say = lambda m: print(f"[{r.label}] {m}" if r.label else m, flush=True)
    st = evidence.load_rounds(raw)
    if st.get("final"):
        say(f"finished: {st['final']['outcome']}; not running again")
        return st
    total = 1 + r.fix_rounds

    def finish(outcome: str, scored: bool, rounds_run: int, proved_in: int | None = None,
               unscored: str | None = None) -> dict:
        st["final"] = {"outcome": outcome, "proved": outcome == r.proved, "scored": scored,
                       "rounds": rounds_run, "proved_in_round": proved_in,
                       **({"unscored": unscored} if unscored else {}),
                       "finished_at": time.strftime("%Y-%m-%dT%H:%M:%S%z")}
        evidence.save_rounds(raw, st)
        say(f"final: {outcome} after {rounds_run} round(s)" + ("" if scored else " (not scored)"))
        return st

    while True:
        rm = evidence.read_run(raw)
        if not evidence.run_finished(rm):
            if rm and rm.get("session"):
                r.resume()
            else:
                r.start()
            continue
        n = evidence.round_of(rm)
        ident = evidence.run_identity(st, rm, raw)
        rec = next((x for x in st["rounds"] if x["round"] == n and all(x.get(k) == v for k, v in ident.items())), None)
        if rec is None:
            rec = {**r.check(rm, n), **ident}
            st["rounds"].append(rec)
            evidence.save_rounds(raw, st)
        kind = rec.get("unscored")
        if kind and kind not in r.retry_kinds:
            return finish(kind, False, n)
        if kind:
            tries = sum(1 for x in st["rounds"] if x["round"] == n and x.get("unscored") == kind)
            if not r.infra_retry or tries > 1:
                return finish(kind, False, n)
            if r.probe and not r.wait(r.probe):
                return finish(kind, False, n)
            rec["retried"] = True
            evidence.save_rounds(raw, st)
            if n == 1:
                say("round 1 was consumed by gateway errors; starting it again")
                (r.archive or (lambda k: evidence.archive_try(raw, k)))(tries)
                st["try"] = st.get("try", 1) + 1
                evidence.save_rounds(raw, st)
            else:
                say(f"round {n} was consumed by gateway errors; running it again with its own caps")
                (r.retry or r.resume)()
            continue
        if rec["outcome"] == r.proved:
            return finish(r.proved, True, n, n)
        if rec.get("no_tool_calls"):
            # the follow-up never ran (no tool call): tooling, not the model. It runs again once, continued in
            # its session with its own caps, like a round the endpoint consumed; a second time, not scored.
            # Never judged by the project's feedback (a no-progress rule would read it as "unchanged").
            tries = sum(1 for x in st["rounds"] if x["round"] == n and x.get("no_tool_calls"))
            if not r.infra_retry or tries > 1 or (r.probe and not r.wait(r.probe)):
                return finish(rec["outcome"], False, n, unscored="no_tool_calls")
            rec["retried"] = True
            evidence.save_rounds(raw, st)
            say(f"round {n} made no tool call; running it again with its own caps")
            (r.retry or r.resume)()
            continue
        if n >= total:
            return finish(rec["outcome"], True, n)
        msg = r.feedback(rec, n, total)
        if msg is None:
            return finish(rec["outcome"], True, n)
        if r.room:
            r.room()
        if r.probe:
            r.wait(r.probe)
        say(f"round {n}: {rec['outcome']}; fix round {n} of {r.fix_rounds}")
        r.followup(msg)
