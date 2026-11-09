# SPDX-License-Identifier: Apache-2.0
"""What the model controls cannot reach the host: symlinks in its HOME, its tree's .git (config, hooks,
attributes). Each case is an attack, set up the way a model in the sandbox could set it up."""
from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import loop, safefs
from aisathome_harness.hostgit import TreeGit

HERE = Path(__file__).resolve().parent


def git(tree: Path, *args: str):
    subprocess.run(["git", *args], cwd=tree, check=True, capture_output=True)


class Tmp(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.root = Path(self.d.name)
        self.host_file = self.root / "host-secret"          # a file outside the sandbox
        self.host_file.write_text("SECRET\n")
        self.home = self.root / "home"
        self.home.mkdir()

    def tearDown(self):
        self.d.cleanup()


class SafeFs(Tmp):
    def test_a_symlinked_file_is_replaced_not_followed(self):
        os.symlink(self.host_file, self.home / "f")
        safefs.write_text(self.home, "f", "x")
        self.assertEqual(self.host_file.read_text(), "SECRET\n")
        self.assertFalse((self.home / "f").is_symlink())
        self.assertEqual((self.home / "f").read_text(), "x")

    def test_a_symlinked_directory_is_refused(self):
        os.symlink(self.root, self.home / ".config")
        with self.assertRaises(safefs.UnsafePath):
            safefs.write_text(self.home, ".config/host-secret", "x")
        self.assertEqual(self.host_file.read_text(), "SECRET\n")
        with self.assertRaises(safefs.UnsafePath):
            safefs.read_bytes(self.home, ".config/host-secret")

    def test_reads_do_not_follow(self):
        os.symlink(self.host_file, self.home / "PR_BODY.md")
        with self.assertRaises(safefs.UnsafePath):
            safefs.read_bytes(self.home, "PR_BODY.md")
        self.assertFalse(safefs.copy_out(self.home, "PR_BODY.md", self.root / "out"))
        self.assertFalse((self.root / "out").exists())
        os.mkfifo(self.home / "fifo")
        with self.assertRaises(safefs.UnsafePath):
            safefs.read_bytes(self.home, "fifo")

    def test_paths_stay_inside(self):
        for bad in ("../x", "/etc/passwd", "", "a/../../x"):
            with self.assertRaises(ValueError, msg=bad):
                safefs.write_text(self.home, bad, "x")

    def test_unlink_removes_the_link_only(self):
        os.symlink(self.host_file, self.home / "l")
        safefs.unlink(self.home, "l")
        self.assertTrue(self.host_file.exists())
        self.assertFalse(os.path.lexists(self.home / "l"))


class StepNotice(Tmp):
    NOTICE = "CRITICAL - " + loop.MAX_STEPS_MARK + "\n\nTools are disabled."

    def make_db(self, path: Path, wal: bool = False):
        path.parent.mkdir(parents=True, exist_ok=True)
        con = sqlite3.connect(path)
        if wal:
            con.execute("PRAGMA journal_mode=WAL")
        con.execute("CREATE TABLE session_message (id TEXT, type TEXT, data TEXT)")
        con.execute("INSERT INTO session_message VALUES ('m1', 'assistant', ?)",
                    (json.dumps({"content": [{"type": "text", "text": self.NOTICE}]}),))
        con.commit()
        return con

    def test_a_symlinked_database_is_not_opened(self):
        victim = self.root / "victim.db"
        self.make_db(victim).close()
        before = victim.read_bytes()
        os.makedirs(self.home / ".local/share/opencode")
        os.symlink(victim, self.home / loop.DB)
        self.assertIsNone(loop.clear_step_notice(self.home, work=self.root / "work"))
        self.assertEqual(victim.read_bytes(), before)

    def test_a_symlinked_journal_is_not_written(self):
        con = self.make_db(self.home / loop.DB)
        con.close()
        os.symlink(self.host_file, self.home / (loop.DB + "-wal"))
        self.assertIsNone(loop.clear_step_notice(self.home, work=self.root / "work"))
        self.assertEqual(self.host_file.read_text(), "SECRET\n")

    def test_sqlite_never_touches_the_home(self):
        """A rollback journal name in the HOME that is a symlink to a host file: SQLite, run on the HOME's
        database, would treat it as a hot journal and truncate or delete through it; ours runs on a copy."""
        self.make_db(self.home / loop.DB).close()
        os.symlink(self.host_file, self.home / (loop.DB + "-journal"))
        self.assertEqual(loop.clear_step_notice(self.home, work=self.root / "work"), {"replaced": 1, "gone": 0, "left": 0})
        self.assertEqual(self.host_file.read_text(), "SECRET\n")

    def test_wal_mode_changes_land_in_the_database(self):
        con = self.make_db(self.home / loop.DB, wal=True)     # left open: the rows are in the -wal file
        r = loop.clear_step_notice(self.home, work=self.root / "work")
        con.close()
        self.assertEqual(r, {"replaced": 1, "gone": 0, "left": 0})
        self.assertFalse((self.home / (loop.DB + "-wal")).exists())
        row = sqlite3.connect(self.home / loop.DB).execute("SELECT data FROM session_message").fetchone()[0]
        self.assertIn(loop.Prompts().step_notice_gone, row)


class Session(Tmp):
    """A fresh session whose HOME the model has already filled with traps (a resume would find them)."""

    def make(self, script: str, **kw):
        tree = self.root / "tree"
        tree.mkdir(exist_ok=True)
        (tree / "A.lean").write_text("a\n")
        if not (tree / ".git").exists():
            git(tree, "init", "-q")
            git(tree, "add", "-A")
            git(tree, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base")
        raw = self.root / "raw"
        raw.mkdir(exist_ok=True)
        kw.setdefault("timing", loop.Timing(poll_s=0.05))
        return loop.Session(task_id="t", tree=tree, raw=raw, home=self.home, state=self.root / "state",
                            command=lambda m, sid: [sys.executable, "-c", script], prompt=lambda s, m: "go",
                            context=lambda *a: (1000, "test"), **kw)

    EV = ('import sys, time; sys.stdout.write(\'{"type":"step_start","timestamp":%d,"sessionID":"ses_x"}\\n'
          '{"type":"step_finish","timestamp":%d,"sessionID":"ses_x","part":{}}\\n\' % (time.time()*1000, time.time()*1000))')

    def test_opencode_config_through_a_symlink(self):
        os.makedirs(self.home / ".config/opencode")
        os.symlink(self.host_file, self.home / loop.CONFIG)
        loop.run_session(self.make(self.EV), "https://api.example.org/v1", None, "m", 10, 5)
        self.assertEqual(self.host_file.read_text(), "SECRET\n")
        self.assertIn("api.example.org", (self.home / loop.CONFIG).read_text())

    def test_config_directory_symlinked_to_the_host(self):
        os.symlink(self.root, self.home / ".config")
        with self.assertRaises(safefs.UnsafePath):
            loop.run_session(self.make(self.EV), "https://api.example.org/v1", None, "m", 10, 5)
        self.assertEqual(self.host_file.read_text(), "SECRET\n")

    def test_collected_files_and_the_lock_log_are_not_followed(self):
        os.symlink(self.host_file, self.home / "NOTES.md")
        os.symlink(self.host_file, self.home / loop.LOCKWAIT)
        (self.home / "REPORT.md").write_text("mine")
        m = loop.run_session(self.make(self.EV, collect=("NOTES.md", "REPORT.md")), "https://api.example.org/v1", None, "m", 10, 5)
        self.assertFalse((self.root / "raw" / "NOTES.md").exists())
        self.assertEqual((self.root / "raw" / "REPORT.md").read_text(), "mine")
        self.assertEqual(m["lock_wait_s"], 0)

    def test_the_model_cannot_run_commands_through_its_git_config(self):
        """A filter, fsmonitor, textconv and a hook in the tree's own .git: the host's checkpoint and
        first-edit check run git, and none of them may run."""
        marker = self.root / "PWNED"
        tree = self.root / "tree"
        evil = (f'import pathlib, subprocess; t = pathlib.Path({str(tree)!r}); '
                f'subprocess.run(["git", "config", "filter.x.clean", "touch {marker}; cat"], cwd=t); '
                f'subprocess.run(["git", "config", "core.fsmonitor", "touch {marker}; false"], cwd=t); '
                f'subprocess.run(["git", "config", "diff.x.textconv", "touch {marker}; cat"], cwd=t); '
                f'(t / ".gitattributes").write_text("* filter=x diff=x\\n"); '
                f'h = t / ".git/hooks/post-index-change"; h.write_text("#!/bin/sh\\ntouch {marker}\\n"); h.chmod(0o755); '
                f'(t / "A.lean").write_text("b\\n"); ' + self.EV.replace("import sys, time; ", "import sys, time\n"))
        s = self.make(evil, first_edit_step=1, first_edit_targets=["A.lean"])
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        self.assertIn("+b", (self.root / "raw" / "model.diff").read_text())
        self.assertFalse(marker.exists(), "a command from the tree's git config ran on the host")
        subprocess.run(["git", "status"], cwd=tree, capture_output=True)     # the tree's own git does run them
        self.assertTrue(marker.exists())

    def test_the_final_tree_is_checkpointed(self):
        tree = self.root / "tree"
        script = f'import pathlib; pathlib.Path({str(tree / "B.lean")!r}).write_text("late\\n"); ' + self.EV.replace("import sys, time; ", "import sys, time\n")
        loop.run_session(self.make(script, timing=loop.Timing(poll_s=0.05, checkpoint_s=3600)),
                         "https://api.example.org/v1", None, "m", 10, 5)
        self.assertIn("B.lean", (self.root / "raw" / "model.diff").read_text())

    def test_model_diff_of_a_crlf_file_applies_to_the_base(self):
        """CRLF line ends survive into model.diff byte for byte, so the patch applies to the base."""
        tree = self.root / "tree"
        tree.mkdir()
        (tree / "W.lean").write_bytes(b"one\r\ntwo\r\nthree\r\n")
        script = (f'import pathlib; pathlib.Path({str(tree / "W.lean")!r}).write_bytes(b"one\\r\\nTWO\\r\\nthree\\r\\n"); '
                  + self.EV.replace("import sys, time; ", "import sys, time\n"))
        s = self.make(script)
        loop.snapshot_base(s.tree, s.state)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        patch = (self.root / "raw" / "model.diff").read_bytes()
        self.assertIn(b"+TWO\r\n", patch)
        base = self.root / "base"
        base.mkdir()
        (base / "W.lean").write_bytes(b"one\r\ntwo\r\nthree\r\n")
        p = subprocess.run(["git", "apply", "-"], cwd=base, input=patch, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual((base / "W.lean").read_bytes(), b"one\r\nTWO\r\nthree\r\n")

    def test_with_a_sandbox_that_adds_the_key_the_config_holds_no_key_path(self):
        """The bundled sandbox's forwarder adds the key: opencode's config gets the forwarder's plain-HTTP
        URL and a placeholder key, never the key file's path (nor the key)."""
        key = self.root / "endpoint.key"
        key.write_text("sk-endpoint-secret-42\n")
        seen = []

        class Sb:
            def model_url(self, base_url):
                return "http://api.example.org/v1"

            def command(self_, m, sid):
                seen.append((self.home / loop.CONFIG).read_text())
                return [sys.executable, "-c", self.EV]
            before = stop = close = lambda self_: None
            export = lambda self_, sid: None
        s = self.make(self.EV, sandbox=Sb())
        s.command = None
        s.__post_init__()
        loop.run_session(s, "https://api.example.org/v1", str(key), "m", 10, 5)
        cfg = json.loads(seen[0])["provider"]["endpoint"]["options"]
        self.assertEqual(cfg, {"baseURL": "http://api.example.org/v1", "apiKey": "dummy"})
        self.assertNotIn("sk-endpoint-secret", seen[0])

    def test_the_key_is_masked_in_every_evidence_file(self):
        """Should the key reach the model's output anyway (an endpoint that echoes it, a model that finds
        it), no evidence file keeps it: masked at the same length, so event offsets stay valid."""
        key = self.root / "endpoint.key"
        key.write_text("sk-endpoint-secret-42\n")
        leak = self.EV.replace("\\n\' % (", '\\n{\\"type\\":\\"text\\",\\"text\\":\\"sk-endpoint-secret-42\\"}\\n\' % (')
        s = self.make(leak)
        said = []
        s.log = said.append
        loop.run_session(s, "https://api.example.org/v1", str(key), "m", 10, 5)
        ev = (s.raw / "events.jsonl").read_bytes()
        self.assertIn(b"[REDACTED]", ev)
        for f in s.raw.rglob("*"):
            if f.is_file():
                self.assertNotIn(b"sk-endpoint-secret-42", f.read_bytes(), f.name)
        self.assertTrue(any("SECURITY" in m for m in said))
        self.assertEqual(ev.count(b"\n"), 3)

    def test_redact_reaches_nested_and_gzipped_files_and_keeps_sizes(self):
        import gzip
        from aisathome_harness import evidence
        raw = self.root / "raw2"
        (raw / "infra-try1").mkdir(parents=True)
        (raw / "events.jsonl").write_bytes(b'{"x":"sk-abcdefgh-123"}\n')
        (raw / "infra-try1" / "events.jsonl.gz").write_bytes(gzip.compress(b"key sk-abcdefgh-123 here"))
        (raw / "clean.txt").write_text("nothing")
        os.symlink(self.host_file, raw / "link")
        self.host_file.write_text("sk-abcdefgh-123")          # outside: a symlink is not followed
        self.assertEqual(evidence.redact(raw, "sk-abcdefgh-123"), 2)
        self.assertEqual((raw / "events.jsonl").read_bytes(), b'{"x":"[REDACTED]*****"}\n')
        self.assertEqual(gzip.decompress((raw / "infra-try1" / "events.jsonl.gz").read_bytes()), b"key [REDACTED]***** here")
        self.assertEqual(self.host_file.read_text(), "sk-abcdefgh-123")
        self.assertEqual(evidence.redact(raw, "short"), 0)       # placeholders are left alone

    def test_a_dirty_tree_is_not_started(self):
        s = self.make(self.EV)
        loop.snapshot_base(s.tree, s.state)
        (s.tree / "A.lean").write_text("touched\n")
        with self.assertRaises(SystemExit):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)

    def test_one_session_per_evidence_directory(self):
        """Another thread (or process) holds the task: a second session refuses; the holder itself may
        re-enter (run_task holds the lock and runs sessions inside it)."""
        import threading
        s = self.make(self.EV)
        held, release = threading.Event(), threading.Event()

        def hold():
            with loop.task_lock(s.raw):
                held.set()
                release.wait(10)
        t = threading.Thread(target=hold)
        t.start()
        held.wait(10)
        try:
            with self.assertRaises(RuntimeError):
                loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        finally:
            release.set()
            t.join()
        with loop.task_lock(s.raw):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        self.assertEqual(self.lock_from_another_process(s.raw), 0, "the lock is released after the session")

    def lock_from_another_process(self, raw: Path) -> int:
        """0 when another process can take the task's lock now, 1 when it is held."""
        code = ("import sys; sys.path.insert(0, sys.argv[2]); from aisathome_harness import loop\n"
                "try:\n    with loop.task_lock(__import__('pathlib').Path(sys.argv[1])): pass\n"
                "except RuntimeError: sys.exit(1)\n")
        return subprocess.run([sys.executable, "-c", code, str(raw), str(HERE.parent)]).returncode

    def test_an_archive_cannot_move_the_lock_away(self):
        """A round-1 retry archives the evidence directory's files while the task's lock is held: another
        process still cannot take the task."""
        from aisathome_harness import evidence
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        with loop.task_lock(s.raw):
            evidence.archive_try(s.raw, 1)
            self.assertEqual(sorted(p.name for p in s.raw.iterdir()), ["infra-try1"])
            self.assertEqual(self.lock_from_another_process(s.raw), 1)
        self.assertEqual(self.lock_from_another_process(s.raw), 0)

    def test_a_resumed_followup_without_tool_calls_is_flagged(self):
        """The follow-up is stopped before it starts, then resumed (no `followup` argument) and makes no
        tool call: the flag comes from the recorded round."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        real = s.command
        s.command = lambda m, sid: (_ for _ in ()).throw(KeyboardInterrupt())
        with self.assertRaises(KeyboardInterrupt):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="fix")
        s.command = real              # EV: a step, no tool call
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, resume=True)
        self.assertEqual((m["followup_round"], m["round_tool_calls"], m["followup_no_tool_calls"]), (1, 0, True))

    def test_a_followup_with_tool_calls_before_a_stop_is_not_flagged(self):
        tool = ('import sys, time\nt = int(time.time() * 1000)\n'
                'sys.stdout.write(\'{"type":"step_start","timestamp":%d,"sessionID":"ses_x"}\\n\' % t)\n'
                'sys.stdout.write(\'{"type":"tool_use","timestamp":%d,"sessionID":"ses_x","part":{"tool":"bash",'
                '"state":{"input":{"command":"ls"}}}}\\n\' % t)\n'
                'sys.stdout.write(\'{"type":"step_finish","timestamp":%d,"sessionID":"ses_x","part":{}}\\n\' % t)\n')
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        s.command = lambda m, sid: [sys.executable, "-c", tool]
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="fix")
        rm = json.loads((s.raw / "run.json").read_text())
        rm.update(partial=True, finished_at=None)             # as if stopped after its tool call
        (s.raw / "run.json").write_text(json.dumps(rm))
        s.command = lambda m, sid: [sys.executable, "-c", self.EV]
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, resume=True)
        self.assertEqual((m["round_tool_calls"], m["followup_no_tool_calls"], m["new_tool_calls"]), (1, False, 0))

    def test_a_record_without_a_needed_field_is_refused(self):
        """No silent defaults: a resume or follow-up whose run.json lacks a field it reads (an older
        harness's record) stops with a clear error, for every such field."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        s.command = lambda m, sid: (_ for _ in ()).throw(KeyboardInterrupt())
        with self.assertRaises(KeyboardInterrupt):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="fix")
        pending = json.loads((s.raw / "run.json").read_text())
        needed = ("session", "segments", "step_cap", "wall_cap_min", "events_start",     # spelled out: the test
                  "followup_round", "base_steps", "round_tools")                          # must not follow the code
        for field in needed:
            rm = {k: v for k, v in pending.items() if k != field}
            (s.raw / "run.json").write_text(json.dumps(rm))
            with self.assertRaises(SystemExit, msg=field) as e:
                loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, resume=True)
            self.assertIn("older harness", str(e.exception))
            self.assertIn(field, str(e.exception))
        finished = {**pending, "partial": False, "finished_at": "x", "steps": 1, "tool_calls": 0}
        finished.pop("pending_message")
        for field in needed[:5] + ("tool_calls",):
            (s.raw / "run.json").write_text(json.dumps({k: v for k, v in finished.items() if k != field}))
            with self.assertRaises(SystemExit, msg=field):
                loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="again")

    def test_a_resume_keeps_the_round_s_recorded_caps(self):
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 99, 99, resume=True)
        self.assertEqual((m["step_cap"], m["wall_cap_min"]), (5, 10))

    def test_followup_numbering_survives_a_stop(self):
        """A stop after run.round1.json is written and before the round's first checkpoint: the next
        follow-up is round 1 again, not 2."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        real = loop._atomic_write
        calls = []

        def stop_after_round_file(path, text):
            real(path, text)
            calls.append(path.name)
            if path.name.startswith("run.round"):
                raise KeyboardInterrupt
        with mock.patch.object(loop, "_atomic_write", stop_after_round_file), self.assertRaises(KeyboardInterrupt):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="fix")
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="fix")
        self.assertEqual(m["followup_round"], 1)
        self.assertEqual(sorted(p.name for p in s.raw.glob("run.round*.json")), ["run.round1.json"])
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, followup="again")
        self.assertEqual(m["followup_round"], 2)

    def test_a_stop_before_the_followup_starts_resumes_that_round(self):
        """The follow-up's round is on record (partial run.json) before anything runs: a stop right there
        resumes round 1 of the follow-ups, with its caps, and sends its own message, not the resume text."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        sent = []
        real = s.command

        def stop_at_start(m, sid):
            raise KeyboardInterrupt
        s.command = stop_at_start
        with self.assertRaises(KeyboardInterrupt):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 7, 3, followup="REVIEW: fix it")
        rm = json.loads((s.raw / "run.json").read_text())
        self.assertEqual((rm["partial"], rm["followup_round"], rm["step_cap"]), (True, 1, 3))
        s.command = lambda m, sid: sent.append(m) or real(m, sid)
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, resume=True)
        self.assertEqual((m["followup_round"], m["step_cap"], m["wall_cap_min"]), (1, 3, 7))
        self.assertEqual(sent, ["REVIEW: fix it"])
        self.assertEqual(m["new_tool_calls"], 0)

    def test_the_step_cap_is_never_exceeded(self):
        """A run that used all its steps gets no further step on resume, however often it is resumed."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 1)
        calls = []
        s.command = lambda m, sid: calls.append(m) or [sys.executable, "-c", self.EV]
        for _ in range(2):
            m = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 1, resume=True)
        self.assertEqual(calls, [])
        self.assertEqual(m["steps"], 1)

    def test_a_retry_gets_its_own_budget(self):
        """A round whose awake time is spent: a resume runs nothing, a retry runs with fresh caps once."""
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 1, 5)
        rm = json.loads((s.raw / "run.json").read_text())
        rm["segments"][0]["active_s"] = 10_000          # the awake ceiling of a 1-minute round is spent
        (s.raw / "run.json").write_text(json.dumps(rm))
        calls = []
        real = s.command
        s.command = lambda m, sid: calls.append(m) or real(m, sid)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 1, 5, resume=True)
        self.assertEqual(calls, [])
        m = loop.run_session(s, "https://api.example.org/v1", None, "m", 1, 5, resume=True, retry=True)
        self.assertEqual(len(calls), 1)
        self.assertEqual(m["retry_from"]["segment"], 1)
        self.assertEqual(m["steps"], 2)

    def test_a_retried_followup_is_scored_on_its_own_attempt(self):
        """A follow-up round consumed by endpoint errors after 3 steps, and its retry the same: the retry
        counts its own 3 steps (not 6), so it is infrastructure again, not the model's."""
        from aisathome_harness import rounds
        s = self.make(self.EV, timing=loop.Timing(poll_s=0.05, max_resumes=0, gateway_backoff_s=0))
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 20)
        err = ('{"type":"error","timestamp":1,"sessionID":"ses_x","error":{"type":"provider.internal",'
               '"message":"no node serving m","status":503}}')
        three = ('import sys, time\nt = int(time.time()*1000)\n'
                 'sys.stdout.write(\'{"type":"step_start","timestamp":%d,"sessionID":"ses_x"}\\n'
                 '{"type":"step_finish","timestamp":%d,"sessionID":"ses_x","part":{}}\\n\' % (t, t) * 3)\n'
                 f'sys.stdout.write({err!r} + "\\n")\n')
        s.command = lambda m, sid: [sys.executable, "-c", three]
        first = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 20, followup="fix it")
        self.assertEqual(first["round_steps"], 3)
        self.assertEqual(rounds.classify_round(s.raw, first, "no_solution")["unscored"], "infra")
        again = loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 20, resume=True, retry=True)
        self.assertEqual((again["round_steps"], again["round_steps_all"]), (3, 6))
        c = rounds.classify_round(s.raw, again, "no_solution")
        self.assertEqual((c["unscored"], c["gateway_errors"], c["round_steps"]), ("infra", 1, 3))

    def test_run_json_is_written_atomically(self):
        s = self.make(self.EV)
        loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5)
        writes = []
        real = loop._atomic_write
        with mock.patch.object(loop, "_atomic_write", lambda p, t: (writes.append(p.name), real(p, t))):
            loop.run_session(s, "https://api.example.org/v1", None, "m", 10, 5, resume=True)
        self.assertIn("run.json", writes)
        self.assertEqual([p.name for p in s.raw.iterdir() if p.name.endswith(".tmp")], [])


if __name__ == "__main__":
    unittest.main()
