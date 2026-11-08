# SPDX-License-Identifier: Apache-2.0
"""A stand-in for `opencode run --format json` in tests: it plays a scripted session and writes the
events opencode writes (one JSON object per line on stdout), so the session loop can be tested without
a container, a model or a network.

    python3 fake_opencode.py SCENARIO STATE [--cwd DIR] [--session ID] MESSAGE

SCENARIO is a JSON file {"invocations": [{"actions": [...], "exit": 0}, ...]}. Each run of this script
plays the next invocation (the count is kept in the directory STATE), appends what it was given to
STATE/calls.jsonl and keeps its process id in STATE/pid (a test's stand-in for stopping the container
kills that process). Actions:

    {"step": {"tools": [{"tool": "bash", "command": "...", "output": "...", "exit": 0}],
              "text": "...", "edit": {"path": "A.lean", "text": "..."}, "ms": 1000,
              "tokens": {"input": 100, "output": 10}}}
        one model step: step_start, its tool calls (an `edit` is written to the working directory,
        or to --cwd, first), its text, step_finish. `ms` is the step's length in event time.
    {"error": {...}}       an error event with this payload
    {"wait": 30}           sleep (a test stops the process here)

Event times start at the real clock and advance by each step's `ms`, so the time between a step's
start and finish is exact while the clock-based caps still see a recent step.
"""
from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path


def main(argv: list[str]) -> int:
    scenario, state = Path(argv[0]), Path(argv[1])
    rest = argv[2:]
    session, cwd = None, Path.cwd()
    if rest[:1] == ["--cwd"]:
        cwd, rest = Path(rest[1]), rest[2:]
    if rest[:1] == ["--session"]:
        session, rest = rest[1], rest[2:]
    message = rest[0] if rest else ""
    state.mkdir(parents=True, exist_ok=True)
    count_file = state / "count"
    n = int(count_file.read_text()) if count_file.exists() else 0
    count_file.write_text(str(n + 1))
    (state / "pid").write_text(str(os.getpid()))
    with open(state / "calls.jsonl", "a") as fh:
        fh.write(json.dumps({"n": n, "session": session, "message": message}) + "\n")
    invocations = json.loads(scenario.read_text())["invocations"]
    inv = invocations[n] if n < len(invocations) else {"actions": [], "exit": 0}
    sid = session or "ses_fake" + str(n)
    clock = [int(time.time() * 1000)]
    msg_no = [0]

    def emit(kind: str, part: dict | None = None, **extra):
        ev = {"type": kind, "timestamp": clock[0], "sessionID": sid, **extra}
        if part is not None:
            ev["part"] = part
        sys.stdout.write(json.dumps(ev, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    for act in inv.get("actions", []):
        if "wait" in act:
            time.sleep(act["wait"])
        elif "error" in act:
            emit("error", error=act["error"])
        elif "step" in act:
            st = act["step"]
            msg_no[0] += 1
            mid = f"msg_{n}_{msg_no[0]}"
            emit("step_start", {"type": "step-start", "messageID": mid})
            if st.get("edit"):
                p = cwd / st["edit"]["path"]
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text(st["edit"]["text"])
            for i, tc in enumerate(st.get("tools", [])):
                t0 = clock[0]
                emit("tool_use", {"type": "tool", "tool": tc.get("tool", "bash"), "callID": f"call_{n}_{msg_no[0]}_{i}",
                                  "messageID": mid,
                                  "state": {"status": "completed", "input": {"command": tc.get("command", "")},
                                            "output": tc.get("output", ""), "metadata": {"exit": tc.get("exit", 0)},
                                            "time": {"start": t0, "end": t0 + 100}}})
            if st.get("text"):
                emit("text", {"type": "text", "text": st["text"], "messageID": mid})
            clock[0] += int(st.get("ms", 1000))
            tk = {"input": 100, "output": 10, "reasoning": 0, "cache": {"read": 0, "write": 0}}
            tk.update(st.get("tokens") or {})
            emit("step_finish", {"type": "step-finish", "messageID": mid,
                                 "reason": "tool-calls" if st.get("tools") else "stop", "tokens": tk})
    return int(inv.get("exit", 0))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
