# SPDX-License-Identifier: Apache-2.0
"""How a run spent its steps: the tool mix, searches over library source, builds, queries to the
compiler, the step of the first write to the target file, long turns.

`round_behaviour` reads a round from events.jsonl (from the round's byte offset) and steps.jsonl;
`session_behaviour` reads an opencode session export. What counts as the target file and as library
source is the project's (`target`, `library`); the defaults fit a Lean project with Mathlib.
Standard library only.
"""
from __future__ import annotations

import json
import re
from collections import Counter
from pathlib import Path

SEARCH_PROGS = {"grep", "rg", "sed", "find", "cat", "head", "tail", "ls", "wc", "awk"}
EDIT_TOOLS = {"edit", "write", "patch", "apply_patch", "multiedit"}
LONG_TURN = 8000          # completion tokens: a turn this long is mostly thinking
LIBRARY = (".lake/packages", "Mathlib/")
BUILD = re.compile(r"\blake\s+build\b")
QUERY = re.compile(r"\blake\s+env\s+lean\b")


def _progs(cmd: str) -> list[str]:
    out = []
    for stage in re.split(r"\|\||&&|;|\||\n", cmd):
        w = stage.strip().split()
        while w and "=" in w[0] and not w[0].startswith("-"):
            w = w[1:]
        if w:
            out.append(Path(w[0]).name)
    return out


def classify_call(ev: dict, target: str | None = None, library: tuple[str, ...] = LIBRARY,
                  library_workdir: str = "packages/mathlib", target_cmd: str | None = None) -> dict:
    """One tool call: {search, library, build, lean_query, writes_target}. `target` is a path fragment
    of the file the task is about: a write or edit tool on such a path counts, and so does a shell
    redirect (`>`, `tee`) to a path matching `target_cmd` (a regex; default: the fragment itself)."""
    p = ev.get("part") or {}
    tool = p.get("tool")
    inp = (p.get("state") or {}).get("input") or {}
    inp = inp if isinstance(inp, dict) else {}
    text = json.dumps(inp, ensure_ascii=False)
    cmd = inp.get("command")
    c = {"search": False, "library": False, "build": False, "lean_query": False, "writes_target": False}
    lib = any(m in text for m in library) or (library_workdir and library_workdir in (inp.get("workdir") or ""))
    if tool in ("grep", "glob", "read"):
        c["search"] = True
    elif cmd:
        ps = _progs(cmd)
        c["build"] = bool(BUILD.search(cmd))
        c["lean_query"] = bool(QUERY.search(cmd))
        c["search"] = bool(not ("lake" in ps or "lean" in ps) and SEARCH_PROGS & set(ps))
        if target:
            c["writes_target"] = bool(re.search(r"(>|tee\s+(-a\s+)?)\s*\S*" + (target_cmd or re.escape(target)), cmd))
    if target and tool in ("write", "edit") and target in str(inp.get("filePath") or inp.get("path") or inp.get("file_path") or ""):
        c["writes_target"] = True
    c["library"] = c["search"] and bool(lib)
    return c


def round_behaviour(events: Path, start: int = 0, steps_file: Path | None = None, round_no: int | None = None,
                    target: str | None = None, library: tuple[str, ...] = LIBRARY, long_turn: int = LONG_TURN,
                    target_cmd: str | None = None) -> dict:
    """A round's behaviour from events.jsonl (from byte `start`) and steps.jsonl (rows of `round_no`):
    tool calls, searches over library source, builds, compiler queries, the step of the first write to
    the target (1-based within the round), long turns and their model minutes."""
    calls, step = [], 0
    try:
        with open(events, "rb") as fh:
            fh.seek(start or 0)
            lines = fh.read().decode(errors="replace").splitlines()
    except OSError:
        lines = []
    for line in lines:
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        if ev.get("type") == "step_start":
            step += 1
        elif ev.get("type") == "tool_use":
            calls.append({**classify_call(ev, target, library, target_cmd=target_cmd), "step": step})
    long_turns, long_s = 0, 0.0
    if steps_file and steps_file.exists():
        for line in steps_file.read_text().splitlines():
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if round_no is not None and r.get("round") != round_no:
                continue
            if (r.get("completion_tokens") or 0) >= long_turn:
                long_turns += 1
                long_s += r.get("model_s") or 0
    n = len(calls)
    lib = sum(c["library"] for c in calls)
    return {"tool_calls": n, "library_search_calls": lib, "library_search_share": round(lib / n, 2) if n else None,
            "builds": sum(c["build"] for c in calls), "lean_queries": sum(c["lean_query"] for c in calls),
            "first_write_step": next((c["step"] for c in calls if c["writes_target"]), None),
            "long_turns": long_turns, "long_turn_model_min": round(long_s / 60, 1)}


def session_behaviour(export: dict, build: re.Pattern = re.compile(r"lake (build|env)|check\.sh")) -> dict | None:
    """From an opencode session export: model calls, the tool mix, the first edit (call number and
    minute), builds and checks run, the largest single generation. None for a session without answers."""
    msgs = [m for m in export.get("messages", []) if m.get("type") == "assistant"]
    if not msgs:
        return None
    t0 = msgs[0]["time"]["created"]
    tools: Counter = Counter()
    first_edit = None
    builds = biggest = 0
    for i, m in enumerate(msgs, 1):
        biggest = max(biggest, (m.get("tokens") or {}).get("output", 0))
        for p in m.get("content", []):
            if p.get("type") != "tool":
                continue
            name = p.get("name")
            tools[name] += 1
            inp = json.dumps(p.get("state", {}).get("input", {}))
            if name in EDIT_TOOLS or (name == "shell" and re.search(r"cat\s*>|tee\s|sed\s+-i|python3?\s+-c.*write", inp)):
                if first_edit is None:
                    first_edit = (i, round((m["time"]["created"] - t0) / 60000, 1))
            if name == "shell" and build.search(inp):
                builds += 1
    return {"model_calls": len(msgs), "tools": dict(tools), "edits": sum(tools[k] for k in EDIT_TOOLS),
            "first_edit": first_edit, "builds": builds, "largest_generation": biggest}
