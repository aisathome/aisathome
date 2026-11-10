# SPDX-License-Identifier: Apache-2.0
"""The session loop against scripted sessions (fake_opencode.py): what run_session leaves behind
(run.json, steps.jsonl, events, the messages it sent, opencode.json, prompt and follow-up files),
normalised and compared with fixtures/loop_characterisation.json. Offline: no container, no model, no
network (the endpoint URL is a closed local port).

Scenarios: a plain run; the first-edit nudge; the stuck-loop nudge; an endpoint error and the resume
after it; the time cap; a resume of a finished run; a follow-up round into a session that ended on the
step limit (the step-notice note, then the one "go on" message).
"""
from __future__ import annotations

import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import loop

HERE = Path(__file__).resolve().parent
FAKE = HERE / "fake_opencode.py"
FIXTURE = HERE / "fixtures" / "loop_characterisation.json"
TID = "task-1"
URL = "http://127.0.0.1:9/v1"      # closed: gateway_context falls back to its default
TARGET = "Foo/Bar.lean"


def step(cmd: str = "ls", out: str = "x", code: int = 0, ms: int = 1000, edit: dict | None = None,
         text: str | None = None) -> dict:
    s = {"tools": [{"tool": "bash", "command": cmd, "output": out, "exit": code}], "ms": ms}
    if edit:
        s["edit"] = edit
    if text:
        s["text"] = text
    return {"step": s}


def talk(text: str, ms: int = 500) -> dict:
    return {"step": {"text": text, "ms": ms}}


EDIT = {"path": TARGET, "text": "theorem bar : True := trivial\n"}
LIMIT = ("CRITICAL - MAXIMUM STEPS REACHED\n\nThe maximum number of steps allowed for this task has been reached. "
         "Tools are disabled until next user input. Respond with text only.")
SCENARIOS = {
    # name: (session fields, wall_min, steps, [run kinds], invocations)
    "plain": ({}, 10, 30, ["fresh"], [
        {"actions": [step("ls"), step("cat Foo/Bar.lean", edit=EDIT), step("lake build Foo.Bar", "Build completed"),
                     talk("RESULT: done")], "exit": 0}]),
    "first_edit_nudge": ({"first_edit_step": 2, "first_edit_targets": [TARGET]}, 10, 30, ["fresh"], [
        {"actions": [step("ls"), step("grep -rn foo ."), step("grep -rn bar ."), {"wait": 30}], "exit": 0},
        {"actions": [step("true", edit=EDIT), talk("RESULT: done")], "exit": 0}]),
    "loop_guard": ({}, 10, 30, ["fresh"], [
        {"actions": [step("grep -rn 'class Finite' .", "", 1)] * 5 + [{"wait": 30}], "exit": 0},
        {"actions": [step("ls"), talk("RESULT: done")], "exit": 0}]),
    "gateway_error": ({}, 10, 30, ["fresh"], [
        {"actions": [step("ls"), {"error": {"name": "APIError", "data": {"message": "503 busy", "statusCode": 503}}}],
         "exit": 1},
        {"actions": [step("ls"), talk("RESULT: done")], "exit": 0}]),
    "time_cap": ({}, 1, 30, ["fresh"], [
        {"actions": [step("lake build Foo.Bar", "Build completed", ms=70000), {"wait": 30}], "exit": 0}]),
    "resume": ({}, 10, 30, ["fresh", "resume"], [
        {"actions": [step("ls")], "exit": 0},
        {"actions": [step("git status"), talk("RESULT: done")], "exit": 0}]),
    "followup_step_limit": ({}, 10, 5, ["fresh", "followup"], [
        {"actions": [step("ls"), step("ls -la"), talk(LIMIT)], "exit": 0},
        {"actions": [talk("I have reached the maximum steps, so here is my summary.")], "exit": 0},
        {"actions": [step("ls"), talk("RESULT: done")], "exit": 0}]),
}
# run.json fields that depend on the clock or the machine, not on the loop's decisions
VOLATILE = {"finished_at", "updated_at", "active_s", "wall_s", "suspended_s", "started_at", "awake_s"}
SEG_VOLATILE = {"active_s", "wall_s", "suspended_s", "started_at", "model_s", "open_step_s"}
STEP_KEEP = ("round", "step", "message_id", "segment", "model_s", "tool_s", "build_s", "prompt_tokens",
             "completion_tokens", "finish_reason", "errors", "preempted", "retries", "resumed")


def git(tree: Path, *args: str):
    subprocess.run(["git", *args], cwd=tree, check=True, capture_output=True)


def norm_run(m: dict) -> dict:
    out = {k: v for k, v in m.items() if k not in VOLATILE}
    out["segments"] = [{k: v for k, v in s.items() if k not in SEG_VOLATILE} for s in m.get("segments", [])]
    if "errors" in out:
        out["errors"] = [re.sub(r'"timestamp": \d+', '"timestamp": 0', e) for e in out["errors"]]
    return out


def norm_events(path: Path) -> list:
    out = []
    for line in path.read_text().splitlines():
        ev = json.loads(line)
        ev.pop("timestamp", None)
        st = (ev.get("part") or {}).get("state") or {}
        st.pop("time", None)
        out.append(ev)
    return out


def play(name: str, prompts: dict) -> dict:
    """Run one scenario through run_session in a scratch directory; return the normalised records."""
    fields, wall_min, steps, kinds, invocations = SCENARIOS[name]
    with tempfile.TemporaryDirectory() as d:
        root = Path(d)
        tree, raw, home, state = root / "tree", root / "raw" / TID, root / "runs" / TID / "home", root / "state"
        tree.mkdir()
        (tree / "README").write_text("base\n")
        git(tree, "init", "-q")
        git(tree, "add", "-A")
        git(tree, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
        raw.mkdir(parents=True)
        home.mkdir(parents=True)
        scen = root / "scenario.json"
        scen.write_text(json.dumps({"invocations": invocations}))

        def command(message, session):
            return [sys.executable, str(FAKE), str(scen), str(state), "--cwd", str(tree),
                    *(["--session", session] if session else []), message]

        def stop():
            if (state / "pid").exists():
                os.kill(int((state / "pid").read_text()), signal.SIGTERM)

        def session() -> loop.Session:
            return loop.Session(
                task_id=TID, tree=tree, raw=raw, home=home, state=root / "runs" / TID / "state", command=command,
                prompt=lambda s, m: f"PROMPT {s} steps {m} min",
                prompts=loop.Prompts(resume=prompts["RESUME_PROMPT"], first_edit=prompts["FIRST_EDIT_NUDGE"],
                                     loop=prompts["LOOP_NUDGE"]),
                timing=loop.Timing(poll_s=0.05, first_edit_poll_s=0.05, checkpoint_s=0.2),
                provider=loop.Provider(id="test", name="Test endpoint", key_path="/run/test/key"),
                stop=stop, export=lambda sid: "{}", **fields)
        runs_meta = []
        with mock.patch.object(loop.time, "sleep", lambda s: None):
            for kind in kinds:
                kw = {"resume": True} if kind == "resume" else {"followup": "FOLLOWUP: fix it."} if kind == "followup" else {}
                loop.run_session(session(), URL, None, "agent", wall_min, steps, **kw)
                runs_meta.append(norm_run(json.loads((raw / "run.json").read_text())))
        calls = [json.loads(x) for x in (state / "calls.jsonl").read_text().splitlines()]
        steps_rows = [{k: r.get(k) for k in STEP_KEEP} for r in map(json.loads, (raw / "steps.jsonl").read_text().splitlines())]
        files = {p.name: p.read_text() for p in sorted(raw.iterdir())
                 if p.name in ("prompt.md", "session.json") or p.name.startswith("followup") or p.name.startswith("run.round")}
        files = {k: (norm_run(json.loads(v)) if k.startswith("run.round") else v) for k, v in files.items()}
        cfg = json.loads((home / ".config" / "opencode" / "opencode.json").read_text())
        return {"runs": runs_meta, "calls": calls, "steps": steps_rows, "events": norm_events(raw / "events.jsonl"),
                "files": files, "opencode_config": cfg, "tree_diff": subprocess.run(
                    ["git", "status", "--porcelain"], cwd=tree, capture_output=True, text=True).stdout}


class Characterisation(unittest.TestCase):
    maxDiff = None

    def test_scenarios(self):
        ref = json.loads(FIXTURE.read_text())
        self.assertEqual(sorted(ref["scenarios"]), sorted(SCENARIOS))
        for name in SCENARIOS:
            with self.subTest(name):
                self.assertEqual(json.loads(json.dumps(play(name, ref["prompts"]))), ref["scenarios"][name])


if __name__ == "__main__":
    unittest.main()
