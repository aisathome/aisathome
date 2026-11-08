# SPDX-License-Identifier: Apache-2.0
"""Per-step metrics of a session: one JSON line per model step in <evidence>/steps.jsonl, written by
the session loop as each step ends (at its next poll) and once more when a segment ends.

    python3 -m aisathome_harness.stepmetrics <evidence dir>   # print the rows from the events
                                                             # (no segment starts: events-only rule)

Source: opencode's events.jsonl. Per row:
  step              the session's n-th step_finish (1-based, over all rounds); the dedup key
  message_id        opencode's message id (joins the session export)
  round             1 = the first round, 2 = the first follow-up, ...; null for a step of a segment
                    the loop did not record
  segment           index of the loop's segment in its round; null if unknown
  start_utc         the request was sent: the previous step's end, or the segment's start
  first_chunk_utc   the step's last step_start (opencode's first streamed chunk of the request
                    that answered; an earlier step_start was a retry's)
  end_utc           step_finish
  wall_s            end - start
  wait_s            first chunk - start: the endpoint's queue, preemption and retries, the
                    server's prefill and the network. The first step of a segment also holds the
                    container's and opencode's start-up (a few seconds)
  model_s           end - first chunk: what the time cap counts (loop.model_time_s, per step)
  decode_s          model_s - tool_s
  tool_s            sum of the step's tool calls (parallel calls overlap, so it can exceed model_s)
  build_s           the part of tool_s in shell calls that build or check (BUILD_CMD; a project
                    with its own wrapper scripts sets its own pattern)
  build_lock_wait_s waits for a build slot that began in the step (a `<asked> <got>` epoch-seconds
                    log, one line per wait); null without the log path
  prompt_tokens     input + cache.read + cache.write; cached_tokens = cache.read
  completion_tokens output + reasoning; reasoning_tokens on their own
  finish_reason     opencode's (tool-calls, stop, length)
  errors            error events since the previous step's end; preempted: one of them is a
                    preemption by the endpoint (PREEMPTED)
  retries           step_starts of this step before the one that answered
  resumed           the first step of a segment that continued an interrupted session (machine
                    stop, suspend, endpoint error); null if the segment is unknown
  node, queue_s     always null here: which server answered and how long the request queued are
                    known only to the endpoint (an aisathome gateway logs both)
Without segment starts (`segments=None`) the wait runs from the previous step's end, so after a stop
it includes the downtime. Standard library only.
"""
from __future__ import annotations

import gzip
import json
import math
import re
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path

FILE = "steps.jsonl"
PREEMPTED = "preempted for a person"      # the aisathome gateway's stream error when a person's request preempts a step
# a shell command that builds or checks; a project with its own wrapper scripts sets its own pattern
BUILD_CMD = re.compile(r"(?:^|[\s;&|(])lake\s+(?:build|env|exe|lean|test|check)\b")
SHELL_TOOLS = {"shell", "bash"}


def iso(ts: float | None) -> str | None:
    if ts is None:
        return None
    return datetime.fromtimestamp(ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def read_events(path: Path) -> list[tuple[int, dict]]:
    """(byte offset, event) of each complete line of events.jsonl (or .gz); a line still being
    written (no newline yet) or not JSON is skipped."""
    if path.suffix == ".gz":
        data = gzip.open(path, "rb").read()
    else:
        try:
            data = path.read_bytes()
        except OSError:
            return []
    out, off = [], 0
    for line in data.splitlines(keepends=True):
        if line.endswith(b"\n") and line.strip():
            try:
                out.append((off, json.loads(line)))
            except ValueError:
                pass
        off += len(line)
    return out


def lock_waits(path: Path | None) -> list[tuple[int, int]] | None:
    """(asked, got) epoch seconds from tools/buildlock.sh's log; None without a path."""
    if path is None:
        return None
    out = []
    try:
        for line in path.read_text().splitlines():
            a = line.split()
            if len(a) >= 2 and a[0].isdigit() and a[1].isdigit():
                out.append((int(a[0]), int(a[1])))
    except OSError:
        pass
    return out


def step_rows(events: list[tuple[int, dict]], segments: list[dict] | None = None,
              lockwait: list[tuple[int, int]] | None = None) -> list[dict]:
    """One dict per finished step, raw floats (epoch seconds; `to_line` rounds). `segments`: the
    harness's segments in order, {"offset": byte offset in events.jsonl where it began,
    "t0": its start (epoch s), "resumed": bool, "round": int}."""
    segs = sorted(segments or [], key=lambda s: s["offset"])
    rows, prev_end, cur, si, n = [], None, None, -1, 0
    errors, preempted, first_in_seg = 0, False, False
    for off, e in events:
        if "timestamp" not in e:
            continue
        while si + 1 < len(segs) and off >= segs[si + 1]["offset"]:
            si += 1                       # a new segment: the previous one's open step is gone
            prev_end, cur, first_in_seg = segs[si]["t0"], None, True
        ts = e["timestamp"] / 1000
        typ = e.get("type")
        p = e.get("part") or {}
        if typ == "error":
            errors += 1
            preempted = preempted or PREEMPTED in json.dumps(e.get("error"))
        elif typ == "step_start":
            cur = {"first_chunk": ts, "tool_s": 0.0, "build_s": 0.0,
                   "retries": cur["retries"] + 1 if cur is not None else 0,
                   "wait_s": (ts - prev_end) if prev_end else None}
        elif typ == "tool_use" and cur is not None:
            st = p.get("state") or {}
            t = st.get("time") or {}
            if t.get("end"):
                d = (t["end"] - t["start"]) / 1000
                cur["tool_s"] += d
                inp = st.get("input")
                if p.get("tool") in SHELL_TOOLS and isinstance(inp, dict) and BUILD_CMD.search(str(inp.get("command", ""))):
                    cur["build_s"] += d
        elif typ == "step_finish" and cur is not None:
            n += 1
            tk = p.get("tokens") or {}
            c = tk.get("cache") or {}
            seg = segs[si] if si >= 0 else None
            lw = None
            if lockwait is not None:
                lo = math.floor(cur["first_chunk"])
                lw = float(sum(g - a for a, g in lockwait if lo <= a <= ts))
            rows.append({
                "round": seg["round"] if seg else None, "step": n, "message_id": p.get("messageID"),
                "segment": si if seg else None,
                "start": prev_end, "first_chunk": cur["first_chunk"], "end": ts,
                "wait_s": cur["wait_s"], "model_s": ts - cur["first_chunk"],
                "decode_s": ts - cur["first_chunk"] - cur["tool_s"], "tool_s": cur["tool_s"],
                "build_s": cur["build_s"], "build_lock_wait_s": lw,
                "prompt": tk.get("input", 0) + c.get("read", 0) + c.get("write", 0),
                "cached": c.get("read", 0), "completion": tk.get("output", 0) + tk.get("reasoning", 0),
                "reasoning": tk.get("reasoning", 0), "finish_reason": p.get("reason"),
                "errors": errors, "preempted": preempted, "retries": cur["retries"],
                "resumed": (bool(seg["resumed"]) and first_in_seg) if seg else None,
            })
            prev_end, cur, errors, preempted, first_in_seg = ts, None, 0, False, False
    return rows


def to_line(attempt: str, r: dict) -> dict:
    """A steps.jsonl record: UTC times, seconds to 3 decimals, missing values null."""
    r3 = lambda x: None if x is None else round(x, 3)
    return {"attempt": attempt, "round": r["round"], "step": r["step"], "message_id": r["message_id"],
            "segment": r["segment"], "start_utc": iso(r["start"]), "first_chunk_utc": iso(r["first_chunk"]),
            "end_utc": iso(r["end"]), "wall_s": r3(r["end"] - r["start"]) if r["start"] else None,
            "wait_s": r3(r["wait_s"]), "model_s": r3(r["model_s"]), "decode_s": r3(r["decode_s"]),
            "tool_s": r3(r["tool_s"]), "build_s": r3(r["build_s"]), "build_lock_wait_s": r["build_lock_wait_s"],
            "prompt_tokens": r["prompt"], "cached_tokens": r["cached"], "completion_tokens": r["completion"],
            "reasoning_tokens": r["reasoning"], "finish_reason": r["finish_reason"],
            "errors": r["errors"], "preempted": r["preempted"], "retries": r["retries"], "resumed": r["resumed"],
            "node": None, "queue_s": None}


def written_steps(path: Path) -> set[int]:
    out = set()
    try:
        for line in path.read_text().splitlines():
            try:
                out.add(int(json.loads(line)["step"]))
            except (ValueError, KeyError, TypeError):
                pass
    except OSError:
        pass
    return out


def append_new(raw: Path, attempt: str, segments: list[dict] | None = None,
               lockwait_log: Path | None = None, lockwait: list[tuple[int, int]] | None = None) -> int:
    """Append the steps finished since the last call to raw/steps.jsonl; idempotent (a step
    already in the file, by its number, is never written again, also after a resume). The build-slot
    waits come as a list (`lockwait`) or from a host-side log file (`lockwait_log`)."""
    out = raw / FILE
    have = written_steps(out)
    waits = lockwait if lockwait is not None else lock_waits(lockwait_log)
    rows = step_rows(read_events(raw / "events.jsonl"), segments, waits)
    new = [to_line(attempt, r) for r in rows if r["step"] not in have]
    if new:
        with open(out, "a") as f:
            f.write("".join(json.dumps(x, separators=(",", ":")) + "\n" for x in new))
    return len(new)


def read(raw: Path) -> list[dict]:
    try:
        return [json.loads(l) for l in (raw / FILE).read_text().splitlines() if l.strip()]
    except (OSError, ValueError):
        return []


def aggregate(lines: list[dict]) -> dict | None:
    """Per attempt: steps, median model s per step, cache hit (cached / prompt tokens) and the
    share of the steps' wall time spent waiting for the first chunk (rows with a known start)."""
    if not lines:
        return None
    ms = [x["model_s"] for x in lines if x.get("model_s") is not None]
    pr = sum(x.get("prompt_tokens") or 0 for x in lines)
    timed = [x for x in lines if x.get("wall_s") and x.get("wait_s") is not None]
    wall = sum(x["wall_s"] for x in timed)
    return {"steps": len(lines),
            "median_model_s": round(float(statistics.median(ms)), 1) if ms else None,
            "cache_hit": round(sum(x.get("cached_tokens") or 0 for x in lines) / pr, 3) if pr else None,
            "wait_share": round(sum(x["wait_s"] for x in timed) / wall, 3) if wall else None}


def main():
    for d in sys.argv[1:]:
        d = Path(d)
        ev = next((d / n for n in ("events.jsonl", "events.jsonl.gz") if (d / n).exists()), None)
        for r in step_rows(read_events(ev)) if ev else []:
            print(json.dumps(to_line(d.name, r)))


if __name__ == "__main__":
    main()
