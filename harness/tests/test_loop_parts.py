# SPDX-License-Identifier: Apache-2.0
"""The session loop's parts: model time, the step-limit notice, endpoint errors, opencode's config, the
watchers, checkpoints and one segment with a stand-in process."""
from __future__ import annotations

import json
import os
import signal
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from aisathome_harness import loop

SID = "ses_abc"


def line(kind: str, ts: int = 1_000_000, **kw) -> str:
    return json.dumps({"type": kind, "timestamp": ts, "sessionID": SID, **kw}, separators=(",", ":")) + "\n"


def tool(cmd: str, out: str = "", code: int = 0) -> str:
    return line("tool_use", part={"type": "tool", "tool": "bash", "state": {"input": {"command": cmd}, "output": out,
                                                                          "metadata": {"exit": code}}})


def git(tree: Path, *args: str):
    subprocess.run(["git", *args], cwd=tree, check=True, capture_output=True)


class Tmp(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.root = Path(self.d.name)
        self.ev = self.root / "events.jsonl"

    def tearDown(self):
        self.d.cleanup()


class ModelTime(Tmp):
    def test_steps_count_from_their_last_start(self):
        self.ev.write_text(line("step_start", 0) + line("step_start", 2000) + line("step_finish", 5000)
                           + line("step_start", 6000) + line("step_finish", 13000))
        self.assertAlmostEqual(loop.model_time_s(self.ev), 3 + 7)

    def test_an_open_step_counts_up_to_now_only_when_asked(self):
        self.ev.write_text(line("step_start", 0) + line("step_finish", 1000) + line("step_start", 10_000))
        self.assertAlmostEqual(loop.model_time_s(self.ev), 1)
        self.assertAlmostEqual(loop.model_time_s(self.ev, now=40), 31)

    def test_from_an_offset(self):
        first = line("step_start", 0) + line("step_finish", 9000)
        self.ev.write_text(first + line("step_start", 10_000) + line("step_finish", 12_000))
        self.assertAlmostEqual(loop.model_time_s(self.ev, len(first.encode())), 2)

    def test_an_open_step_stops_counting_at_an_endpoint_error(self):
        """After an error the model waits for opencode's retry: that wait is not model time, so neither the
        cap inside a segment nor a stopped segment's open_step_s counts it (Codex review, 2026-11-10). A step
        that finishes after an error counts as before."""
        self.ev.write_text(line("step_start", 0) + line("step_finish", 1000) + line("step_start", 2000)
                           + line("error", 3000, error={"message": "503"}))
        self.assertAlmostEqual(loop.model_time_s(self.ev, now=600), 1)
        self.assertAlmostEqual(loop.open_step_s(self.ev, now=600), 0)
        self.assertAlmostEqual(loop.model_used_s(self.ev, 0, [{"events_offset": 0, "open_step_s": 30}]), 1)
        with open(self.ev, "a") as f:
            f.write(line("step_start", 400_000))         # opencode's retry: counted from its own start
        self.assertAlmostEqual(loop.model_time_s(self.ev, now=410), 1 + 10)
        with open(self.ev, "a") as f:
            f.write(line("error", 401_000) + line("step_finish", 405_000))
        self.assertAlmostEqual(loop.model_time_s(self.ev, now=900), 1 + 5)

    def test_an_interrupted_step_counts_once(self):
        """A segment's open_step_s counts while its events end in that open step; a checkpointed segment
        whose step then finished (the run died after the checkpoint) is counted by its step_finish only."""
        seg1 = line("step_start", 0) + line("step_finish", 1000) + line("step_start", 2000)   # stopped mid-step
        seg2 = line("step_start", 50_000) + line("step_finish", 53_000)
        self.ev.write_text(seg1 + seg2)
        segs = [{"events_offset": 0, "open_step_s": 40}, {"events_offset": len(seg1.encode()), "open_step_s": 0}]
        self.assertAlmostEqual(loop.model_used_s(self.ev, 0, segs), 1 + 3 + 40)
        self.assertAlmostEqual(loop.model_used_s(self.ev, len(seg1.encode()), segs), 3)
        finished = seg1 + line("step_finish", 9000)
        self.ev.write_text(finished)
        self.assertAlmostEqual(loop.model_used_s(self.ev, 0, [{"events_offset": 0, "open_step_s": 5}]), 1 + 7)


class StepNotice(Tmp):
    NOTICE = "CRITICAL - " + loop.MAX_STEPS_MARK + "\n\nTools are disabled until next user input."

    def db(self, rows):
        p = self.root / "home" / ".local" / "share" / "opencode" / "opencode.db"
        p.parent.mkdir(parents=True)
        con = sqlite3.connect(p)
        con.execute("CREATE TABLE session_message (id TEXT, type TEXT, data TEXT)")
        for mid, typ, text in rows:
            con.execute("INSERT INTO session_message VALUES (?, ?, ?)", (mid, typ, json.dumps({"content": [{"type": "text", "text": text}]})))
        con.commit()
        con.close()
        return self.root / "home"

    def test_the_notice_is_replaced_and_the_note_says_tools_work(self):
        home = self.db([("m1", "user", "go, " + loop.MAX_STEPS_MARK), ("m2", "assistant", self.NOTICE)])
        self.ev.write_text(line("text", part={"text": self.NOTICE}))
        report = {}
        m = loop.with_step_reset("go on", self.ev, 30, home, report)
        self.assertEqual(report, {"replaced": 1, "gone": 0, "left": 0})
        self.assertTrue(m.endswith("go on") and "30 steps" in m and "CRITICAL" not in m)
        self.assertEqual(loop.clear_step_notice(home), {"replaced": 0, "gone": 1, "left": 0})

    def test_without_the_database_the_note_names_the_notice(self):
        self.ev.write_text(line("text", part={"text": self.NOTICE}))
        report = {}
        m = loop.with_step_reset("go on", self.ev, 30, self.root / "nothing", report)
        self.assertIn("CRITICAL", m)
        self.assertEqual(report, {"unread": True})

    def test_no_notice_no_note(self):
        self.ev.write_text(line("step_start"))
        self.assertEqual(loop.with_step_reset("go on", self.ev, 30), "go on")

    def test_stopped_on_step_limit(self):
        self.ev.write_text(line("step_start") + line("text", part={"text": "I reached the maximum steps."}))
        self.assertTrue(loop.stopped_on_step_limit(self.ev))
        self.ev.write_text(line("step_start") + line("text", part={"text": "Max steps.\nRESULT: done"}))
        self.assertFalse(loop.stopped_on_step_limit(self.ev))
        self.ev.write_text(line("step_start") + tool("ls") + line("text", part={"text": "maximum steps"}))
        self.assertFalse(loop.stopped_on_step_limit(self.ev))


class Errors(unittest.TestCase):
    ERR = '{"type":"error","error":{"message":"503 busy"}}'

    def seg(self, **kw):
        return {"suspend_interrupted": False, "timed_out": False, "exit": 1, **kw}

    def test_an_endpoint_error_is_resumed(self):
        self.assertTrue(loop.gateway_interrupted(self.seg(), self.ERR))

    def test_not_when_opencode_retried_itself(self):
        tail = self.ERR + "\n" + '{"type":"step_start"}'
        self.assertTrue(loop.recovered_after_error(tail))
        self.assertFalse(loop.gateway_interrupted(self.seg(), tail))
        self.assertTrue(loop.gateway_interrupted(self.seg(stalled_after_error=True), tail))

    def test_not_a_clean_exit_or_another_error(self):
        self.assertFalse(loop.gateway_interrupted(self.seg(exit=0), self.ERR))
        self.assertFalse(loop.gateway_interrupted(self.seg(), '{"type":"error","error":{"message":"bad tool"}}'))

    def test_stall_limits(self):
        self.assertEqual(loop.stall_limit("x"), loop.Timing().stall_after_error_s)
        self.assertEqual(loop.stall_limit(loop.PREEMPTED), loop.Timing().stall_after_preempt_s)


class Config(unittest.TestCase):
    def test_config(self):
        p = loop.Provider(id="gw", name="Gateway", key_path="/run/k")
        c = loop.opencode_config("https://api.example.org/v1", "/x/key", "m", 7, {"temperature": 1.0}, 1000, p)
        prov = c["provider"]["gw"]
        self.assertEqual((prov["options"]["baseURL"], prov["options"]["apiKey"]), ("https://api.example.org/v1", "{file:/run/k}"))
        self.assertEqual(prov["models"]["m"], {"name": "m", "limit": {"context": 1000, "output": 32768}, "options": {"temperature": 1.0}})
        self.assertEqual((c["model"], c["agent"]["build"]["steps"]), ("gw/m", 7))
        self.assertFalse(c["tools"]["webfetch"] or c["tools"]["websearch"])
        self.assertEqual(loop.opencode_config("u", None, "m", 1)["provider"]["endpoint"]["options"]["apiKey"], "dummy")

    def test_args(self):
        self.assertEqual(loop.opencode_args("m", None), ["run", "--standalone", "--auto", "--format", "json", "-m", "endpoint/m"])
        self.assertEqual(loop.opencode_args("m", SID)[-2:], ["--session", SID])

    def test_context_default_on_a_closed_port(self):
        ctx, src = loop.gateway_context("http://127.0.0.1:9/v1", None, "m", timeout=2)
        self.assertEqual(ctx, loop.DEFAULT_CONTEXT)
        self.assertIn("default", src)

    def test_resume_after_an_endpoint_error(self):
        p = loop.Prompts()
        self.assertIn(p.machine_stop, p.resume)
        self.assertIn(p.gateway_error, p.resume_after(True))
        self.assertEqual(p.resume_after(False), p.resume)


class Watchers(Tmp):
    def tree(self) -> Path:
        t = self.root / "tree"
        t.mkdir()
        (t / "A.lean").write_text("x\n")
        git(t, "init", "-q")
        git(t, "add", "-A")
        git(t, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
        return t

    def test_tree_changed(self):
        t = self.tree()
        from aisathome_harness.hostgit import TreeGit
        g = TreeGit(t, self.root / "state")
        self.assertTrue(loop.tree_changed(g, ["B.lean"]))      # no base yet: git fails, never nudge on a doubt
        g.init_base()
        self.assertFalse(loop.tree_changed(g, ["B.lean"]))
        (t / "B.lean").write_text("new\n")
        self.assertTrue(loop.tree_changed(g, ["B.lean"]))
        self.assertTrue(loop.tree_changed(g, []))
        self.assertFalse(loop.tree_changed(g, [], accept=lambda p: p.endswith(".py")))

    def test_first_edit_watch(self):
        changed = [False]
        w = loop.FirstEditWatch(self.ev, 2, lambda: changed[0], ["A.lean"], grace=2)
        self.ev.write_text((line("step_start") + line("step_finish")) * 1)
        self.assertFalse(w.poll())
        with open(self.ev, "a") as f:
            f.write(line("step_start") + line("step_finish"))
        self.assertTrue(w.poll())                    # between two steps, nothing changed
        changed[0] = True
        w2 = loop.FirstEditWatch(self.ev, 2, lambda: changed[0])
        self.assertFalse(w2.poll())
        self.assertTrue(w2.edited)

    def test_first_edit_watch_stops_mid_step_after_the_grace(self):
        w = loop.FirstEditWatch(self.ev, 1, lambda: False, grace=2)
        self.ev.write_text(line("step_start") + line("step_finish") + line("step_start"))
        self.assertFalse(w.poll())                   # a step is open
        with open(self.ev, "a") as f:
            f.write(line("step_finish") + line("step_start") + line("step_finish") + line("step_start"))
        self.assertTrue(w.poll())

    def test_loop_watch(self):
        w = loop.LoopWatch(self.ev, grace=2)
        body = (line("step_start") + tool("grep -rn foo .", "", 1) + line("step_finish")) * 5
        self.ev.write_text(body)
        self.assertTrue(w.poll())
        self.assertEqual(w.record(), {"fired": True, "signature": "grep foo", "count": 5, "at_step": 5})

    def test_an_edit_resets_the_loop_count(self):
        w = loop.LoopWatch(self.ev)
        rep = line("step_start") + tool("grep -rn foo .", "", 1) + line("step_finish")
        self.ev.write_text(rep * 4 + line("step_start") + tool("echo x > A.lean") + line("step_finish") + rep * 4)
        self.assertFalse(w.poll())

    def test_interruptions(self):
        self.assertEqual(loop.interruptions([{}, {}, {"first_edit_nudge": True}, {}]), 2)


class Checkpoint(Tmp):
    def test_the_diff_is_against_the_base_and_leaves_the_tree_alone(self):
        from aisathome_harness.hostgit import TreeGit
        t = self.root / "tree"
        t.mkdir()
        (t / "A.lean").write_text("a\n")
        git(t, "init", "-q")
        git(t, "add", "-A")
        git(t, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
        g = TreeGit(t, self.root / "state")
        g.init_base()
        (t / "A.lean").write_text("b\n")
        (t / "B.lean").write_text("new\n")
        git(t, "add", "A.lean")
        git(t, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "the model commits")
        before = subprocess.run(["git", "log", "--oneline"], cwd=t, capture_output=True, text=True).stdout
        n = loop.checkpoint_diff(self.root, g)
        self.assertGreater(n, 0)
        d = (self.root / "model.diff").read_text()
        self.assertIn("B.lean", d)
        self.assertIn("+b", d)            # a commit of the model's does not hide its edit
        self.assertEqual(subprocess.run(["git", "log", "--oneline"], cwd=t, capture_output=True, text=True).stdout, before)
        clean = self.root / "clean"
        subprocess.run(["git", "clone", "-q", str(t), str(clean)], check=True)
        git(clean, "checkout", "-q", "HEAD~1")
        git(clean, "apply", str(self.root / "model.diff"))
        self.assertEqual((clean / "A.lean").read_text(), "b\n")

    def test_no_base_no_diff(self):
        from aisathome_harness.hostgit import TreeGit
        (self.root / "t").mkdir()
        self.assertEqual(loop.checkpoint_diff(self.root, TreeGit(self.root / "t", self.root / "state")), -1)


class Segment(Tmp):
    def test_exit_and_events(self):
        script = f"import sys; sys.stdout.write({line('step_start')!r} + {line('step_finish', 1_002_000)!r})"
        s = loop.run_segment([sys.executable, "-c", script], self.root, 60, timing=loop.Timing(poll_s=0.05))
        self.assertEqual((s["exit"], s["timed_out"], s.get("gateway_interrupted")), (0, False, None))
        self.assertEqual(loop.event_stats(self.ev)["steps"], 1)

    def test_the_awake_cap_stops_it(self):
        s = loop.run_segment([sys.executable, "-c", "import time; time.sleep(30)"], self.root, 0.2, model_left_s=60,
                             timing=loop.Timing(poll_s=0.05))
        self.assertEqual((s["timed_out"], s["cap"]), (True, "awake ceiling"))
        self.assertLess(s["active_s"], 5)

    def test_a_project_stop_is_used(self):
        """The project's stop (a container's) ends the process; the loop does not terminate it itself."""
        pid = self.root / "pid"
        script = f"import os,time; open({str(pid)!r}, 'w').write(str(os.getpid())); time.sleep(30)"
        stopped = []

        def stop():
            stopped.append(1)
            os.kill(int(pid.read_text()), signal.SIGKILL)
        s = loop.run_segment([sys.executable, "-c", script], self.root, 0.3, timing=loop.Timing(poll_s=0.05), stop=stop)
        self.assertEqual((s["timed_out"], s["cap"], stopped), (True, "time", [1]))

    def test_a_nudge_stops_it_between_steps(self):
        script = ("import sys,time\n"
                  f"sys.stdout.write({(line('step_start') + line('step_finish')) * 3!r}); sys.stdout.flush(); time.sleep(30)\n")
        w = loop.FirstEditWatch(self.ev, 2, lambda: False)
        s = loop.run_segment([sys.executable, "-c", script], self.root, 60, nudge=w,
                             timing=loop.Timing(poll_s=0.05, first_edit_poll_s=0.05))
        self.assertTrue(s.get("first_edit_nudge"))


class Stats(Tmp):
    def test_event_stats_and_usage(self):
        self.ev.write_text(line("step_start") + tool("curl https://example.org") + tool("ls")
                           + line("step_finish", part={"tokens": {"input": 5, "output": 2, "reasoning": 1, "cache": {"read": 3}}})
                           + line("error", error={"message": "x"}))
        st = loop.event_stats(self.ev)
        self.assertEqual((st["steps"], st["tool_calls"], st["tokens_in"], st["tokens_cache_read"], len(st["errors"])), (1, 2, 5, 3, 1))
        self.assertEqual(st["network_like_commands"], ["curl https://example.org"])
        exp = self.root / "session.json"
        exp.write_text(json.dumps({"messages": [{"info": {"tokens": {"input": 10, "output": 4, "cache": {"read": 6}}}}, {"info": {}}]}))
        self.assertEqual(loop.session_usage(exp)["max_prompt"], 16)
        self.assertEqual(loop.session_usage(self.root / "nothing"), {})


if __name__ == "__main__":
    unittest.main()
