# SPDX-License-Identifier: Apache-2.0
"""run_task, the seam a queue calls: a project's steps driven to the end, and two tasks of one project at
once (threads) without sharing anything."""
from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

from aisathome_harness import loop
from aisathome_harness.project import Project, Task, Verdict, Workspace
from aisathome_harness.task import Caps, Endpoint, run_task

EVENTS = ('import sys, time\nt = int(time.time() * 1000)\n'
          'sys.stdout.write(\'{"type":"step_start","timestamp":%d,"sessionID":"ses_%s"}\\n\' % (t, sys.argv[1]))\n'
          'sys.stdout.write(\'{"type":"tool_use","timestamp":%d,"sessionID":"ses_%s","part":{"tool":"bash","state":{"input":{"command":"ls"}}}}\\n\' % (t, sys.argv[1]))\n'
          'open(sys.argv[2], "a").write(sys.argv[3] + "\\n")\n'
          'sys.stdout.write(\'{"type":"step_finish","timestamp":%d,"sessionID":"ses_%s","part":{}}\\n\' % (t + 500, sys.argv[1]))\n')


TEXT_ONLY = ('import sys, time\nt = int(time.time() * 1000)\n'
             'sys.stdout.write(\'{"type":"step_start","timestamp":%d,"sessionID":"ses_x"}\\n\' % t)\n'
             'sys.stdout.write(\'{"type":"step_finish","timestamp":%d,"sessionID":"ses_x","part":{}}\\n\' % (t + 100))\n')


class Demo(Project):
    """Proved once the solution file has two lines."""

    name = "demo"

    def __init__(self, root: Path):
        root.mkdir(parents=True, exist_ok=True)
        (root / "PROJECT.md").write_text("# Demo\n\nstatus: local-only\nagreement: none\n")
        super().__init__(root / "PROJECT.md")
        self.root, self.calls = root, []

    def workspace(self, task):
        b = self.root / "work" / task.id
        return Workspace(b / "tree", b / "home", self.root / "raw" / task.id, b / "state")

    def prepare(self, task):
        self.calls.append(("prepare", task.id))
        ws = self.workspace(task)
        ws.tree.mkdir(parents=True, exist_ok=True)
        ws.home.mkdir(parents=True, exist_ok=True)
        (ws.tree / "sol.txt").write_text("")
        subprocess.run(["git", "init", "-q"], cwd=ws.tree, check=True)
        loop.snapshot_base(ws.tree, ws.state)
        return ws

    def session(self, task, ws, e):
        return loop.Session(task_id=task.id, tree=ws.tree, raw=ws.raw, home=ws.home, state=ws.state,
                            prompt=lambda s, m: "write sol.txt",
                            command=lambda msg, sid: [sys.executable, "-c", EVENTS, task.id, str(ws.tree / "sol.txt"), msg[:20]],
                            timing=loop.Timing(poll_s=0.05), context=lambda *a: (1000, "test"))

    def check(self, task, ws, run, round):
        n = len((ws.tree / "sol.txt").read_text().splitlines())
        return Verdict("proved" if n >= 2 else "incomplete", detail=f"{n} line(s)", data={"lines": n})

    def feedback(self, task, verdict, round, total):
        self.calls.append(("feedback", task.id, verdict.data))
        return f"Only {verdict.detail}; add a line."


class RunTask(unittest.TestCase):
    def test_to_the_end_then_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            p = Demo(Path(d))
            st = run_task(p, Task("t1"), Endpoint("https://api.example.org/v1", "m"), Caps(fix_rounds=2))
            self.assertEqual((st["final"]["outcome"], st["final"]["proved_in_round"]), ("proved", 2))
            self.assertEqual(p.calls, [("prepare", "t1"), ("feedback", "t1", {"lines": 1})])
            self.assertEqual([r["lines"] for r in st["rounds"]], [1, 2])
            self.assertTrue((Path(d) / "raw" / "t1" / "followup1.md").exists())
            again = run_task(p, Task("t1"), Endpoint("https://api.example.org/v1", "m"))
            self.assertEqual(again["final"], st["final"])
            self.assertEqual(len(p.calls), 2)

    def test_a_running_task_is_not_prepared_again(self):
        """Another caller holds the task (its session runs): run_task refuses before prepare touches the tree."""
        with tempfile.TemporaryDirectory() as d:
            p = Demo(Path(d))
            held, release = threading.Event(), threading.Event()

            def hold():
                with loop.task_lock(p.workspace(Task("t1")).raw):
                    held.set()
                    release.wait(10)
            t = threading.Thread(target=hold)
            t.start()
            held.wait(10)
            try:
                with self.assertRaises(RuntimeError):
                    run_task(p, Task("t1"), Endpoint("https://api.example.org/v1", "m"))
            finally:
                release.set()
                t.join()
            self.assertEqual(p.calls, [])

    def test_a_followup_without_tool_calls_is_not_scored(self):
        """The follow-up made no tool call (run.json followup_no_tool_calls): the task ends not scored."""
        with tempfile.TemporaryDirectory() as d:
            p = Demo(Path(d))
            base = p.session

            def session(task, ws, e):
                s = base(task, ws, e)
                if (ws.raw / "run.json").exists():      # a follow-up: text only, no tool call
                    s.command = lambda msg, sid: [sys.executable, "-c", TEXT_ONLY]
                return s
            p.session = session
            st = run_task(p, Task("t1"), Endpoint("https://api.example.org/v1", "m"), Caps(fix_rounds=2))
            self.assertEqual((st["final"]["scored"], st["final"].get("unscored"), st["final"]["rounds"]),
                             (False, "no_tool_calls", 2))
            self.assertTrue(st["rounds"][1]["no_tool_calls"])

    def test_two_tasks_at_once(self):
        with tempfile.TemporaryDirectory() as d:
            p = Demo(Path(d))
            out = {}
            ts = [threading.Thread(target=lambda i=i: out.__setitem__(i, run_task(
                p, Task(i), Endpoint("https://api.example.org/v1", "m"), Caps(fix_rounds=1)))) for i in ("a", "b")]
            for t in ts:
                t.start()
            for t in ts:
                t.join(60)
            self.assertEqual({k: v["final"]["outcome"] for k, v in out.items()}, {"a": "proved", "b": "proved"})
            for i in ("a", "b"):
                ev = (Path(d) / "raw" / i / "events.jsonl").read_text()
                self.assertIn(f"ses_{i}", ev)
                self.assertNotIn(f"ses_{'b' if i == 'a' else 'a'}", ev)


class TemplateAdapter(unittest.TestCase):
    def test_it_prepares_and_builds_a_session(self):
        import importlib.util
        import os
        from unittest import mock
        from aisathome_harness.host import Host
        with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {"MYPROJECT_WORK": f"{d}/work",
                                                                             "MYPROJECT_RAW": f"{d}/raw"}):
            spec = importlib.util.spec_from_file_location("tpl", Path(__file__).resolve().parent.parent / "template" / "adapter.py")
            m = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(m)
            p = m.MyProject(Host(state=Path(d) / "host"))
            task = p.select()[0]
            ws = p.prepare(task)
            s = p.session(task, ws, Endpoint("https://api.example.org/v1", "m"))
            self.assertTrue(s.git.has_base())
            self.assertEqual(s.sandbox.name, s.sandbox.name.lower())
            self.assertIn("write" if False else "At most 30 steps", s.prompt(30, 30))
            with self.assertRaises(Exception):
                p.submit(task, ws)


if __name__ == "__main__":
    unittest.main()
