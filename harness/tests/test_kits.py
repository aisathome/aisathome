# SPDX-License-Identifier: Apache-2.0
"""The kits: loop_guard, stepmetrics, behaviour, evidence, the sandbox (with Docker stubbed, and the egress
forwarder on localhost), and the Lean kit (package sets with a stand-in build, the sorry check, the axiom
scan and check command). Nothing here needs Docker, Lean or a network beyond localhost."""
from __future__ import annotations

import asyncio
import io
import json
import socket
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import behaviour, evidence, loop_guard, stepmetrics
from aisathome_harness.lean import axioms, pkgsets, sorry_text
from aisathome_harness.sandbox import egress_forward, sandbox


def ev(kind: str, ts: int, **kw) -> dict:
    return {"type": kind, "timestamp": ts, **kw}


def call(cmd: str, out: str = "", code: int = 0, tool: str = "bash") -> dict:
    return {"type": "tool_use", "part": {"tool": tool, "state": {"input": {"command": cmd}, "output": out,
                                                                 "metadata": {"exit": code}}}}


class LoopGuard(unittest.TestCase):
    def test_signature(self):
        self.assertEqual(loop_guard.signature("cd x && grep -rn 'class Foo' Mathlib | head -5"), "grep class Foo")
        self.assertEqual(loop_guard.signature("FOO=1 lake build A 2>&1 | tail"), "lake build A")
        self.assertIsNone(loop_guard.signature("cat > A.lean <<EOF\nx\nEOF"))

    def test_tracker(self):
        t = loop_guard.Tracker(3)
        self.assertIsNone(t.see(call("grep -rn foo .", "", 1)))
        self.assertIsNone(t.see(call("grep -rn foo src", "(no output)", 1)))
        self.assertIsNone(t.see(call("grep -rn foo . | head", "found", 0)))      # an answer does not count
        self.assertEqual(t.see(call("grep -rn foo lib", "", 1)), "grep foo")
        t.see(call("x", tool="edit"))
        self.assertIsNone(t.see(call("grep -rn foo .", "", 1)))

    def test_same_failure_text_only(self):
        t = loop_guard.Tracker(2)
        self.assertIsNone(t.see(call("lake build A", "error: one", 1)))
        self.assertIsNone(t.see(call("lake build A", "error: two", 1)))
        self.assertEqual(t.see(call("lake build A", "error: two", 1)), "lake build A")


class StepMetrics(unittest.TestCase):
    def test_rows(self):
        events = [(0, ev("step_start", 1_000_000)),
                  (1, ev("tool_use", 1_000_100, part={"tool": "bash", "state": {"input": {"command": "lake build A"},
                                                                                "time": {"start": 1_000_100, "end": 1_002_100}}})),
                  (2, ev("step_finish", 1_003_000, part={"reason": "tool-calls", "messageID": "m1",
                                                         "tokens": {"input": 10, "output": 5, "reasoning": 2, "cache": {"read": 4}}})),
                  (3, ev("error", 1_003_500, error={"message": stepmetrics.PREEMPTED})),
                  (4, ev("step_start", 1_004_000)), (5, ev("step_finish", 1_005_000, part={}))]
        r1, r2 = stepmetrics.step_rows(events, [{"offset": 0, "t0": 999.0, "resumed": False, "round": 1}], [(1000, 1001)])
        self.assertEqual((r1["model_s"], r1["tool_s"], r1["build_s"], r1["wait_s"], r1["prompt"], r1["completion"]),
                         (3.0, 2.0, 2.0, 1.0, 14, 7))
        self.assertEqual(r1["build_lock_wait_s"], 1.0)
        self.assertEqual((r2["errors"], r2["preempted"], r2["step"]), (1, True, 2))
        line = stepmetrics.to_line("t", r1)
        self.assertEqual((line["attempt"], line["end_utc"][-1], line["node"]), ("t", "Z", None))

    def test_append_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            raw = Path(d)
            (raw / "events.jsonl").write_text(json.dumps(ev("step_start", 1000)) + "\n" + json.dumps(ev("step_finish", 2000, part={})) + "\n")
            self.assertEqual(stepmetrics.append_new(raw, "t"), 1)
            self.assertEqual(stepmetrics.append_new(raw, "t"), 0)
            self.assertEqual(stepmetrics.aggregate(stepmetrics.read(raw))["steps"], 1)


class Behaviour(unittest.TestCase):
    def test_round(self):
        with tempfile.TemporaryDirectory() as d:
            e = Path(d) / "events.jsonl"
            calls = [call("grep -rn foo .lake/packages/mathlib"), call("lake build Sol"),
                     call("lake env lean Scratch.lean"), {"type": "tool_use", "part": {"tool": "write", "state": {"input": {"filePath": "/w/Sol.lean"}}}}]
            e.write_text("".join(json.dumps(ev("step_start", 0)) + "\n" + json.dumps(c) + "\n" for c in calls))
            b = behaviour.round_behaviour(e, target="Sol.lean")
            self.assertEqual((b["tool_calls"], b["library_search_calls"], b["builds"], b["lean_queries"], b["first_write_step"]),
                             (4, 1, 1, 1, 4))

    def test_session(self):
        exp = {"messages": [{"type": "assistant", "time": {"created": 0}, "tokens": {"output": 9}, "content": [
                   {"type": "tool", "name": "read", "state": {"input": {}}}]},
                            {"type": "assistant", "time": {"created": 120000}, "tokens": {"output": 3}, "content": [
                   {"type": "tool", "name": "edit", "state": {"input": {}}},
                   {"type": "tool", "name": "shell", "state": {"input": {"command": "lake build X"}}}]}]}
        b = behaviour.session_behaviour(exp)
        self.assertEqual((b["model_calls"], b["edits"], b["first_edit"], b["builds"], b["largest_generation"]), (2, 1, (2, 2.0), 1, 9))
        self.assertIsNone(behaviour.session_behaviour({"messages": []}))


class Evidence(unittest.TestCase):
    def test_layout(self):
        with tempfile.TemporaryDirectory() as d:
            raw = Path(d)
            self.assertIsNone(evidence.read_run(raw))
            self.assertFalse(evidence.run_finished({"partial": True, "finished_at": "x"}))
            self.assertEqual(evidence.round_of({"followup_round": 2}), 3)
            evidence.save_rounds(raw, {"rounds": [{"round": 1}]})
            self.assertEqual(evidence.load_rounds(raw)["rounds"], [{"round": 1}])
            (raw / "events.jsonl").write_text("x\n")
            (raw / "run.json").write_text("{}")
            dest = evidence.archive_try(raw, 1)
            self.assertEqual(sorted(p.name for p in raw.iterdir()), ["infra-try1", "rounds.json"])
            self.assertTrue((dest / "events.jsonl").exists())


class Forwarder(unittest.TestCase):
    def test_it_forwards_to_its_one_target(self):
        """An echo server as the target, the forwarder in a thread, a client through it."""
        echo = socket.socket()
        echo.bind(("127.0.0.1", 0))
        echo.listen()
        tport = echo.getsockname()[1]

        def serve():
            c, _ = echo.accept()
            c.sendall(c.recv(100).upper())
            c.close()
        threading.Thread(target=serve, daemon=True).start()
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        lport = probe.getsockname()[1]
        probe.close()
        loop_ = asyncio.new_event_loop()
        threading.Thread(target=lambda: loop_.run_until_complete(
            egress_forward.main_async(f"127.0.0.1:{lport}", f"127.0.0.1:{tport}")), daemon=True).start()
        with mock.patch("sys.stdout", io.StringIO()):
            for _ in range(100):
                try:
                    c = socket.create_connection(("127.0.0.1", lport), timeout=2)
                    break
                except OSError:
                    threading.Event().wait(0.05)
            c.sendall(b"hello")
            self.assertEqual(c.recv(100), b"HELLO")
            c.close()
        echo.close()


def fake_builder(tmp: Path, log, env=None):
    mo = tmp / pkgsets.MATHLIB_OLEAN
    mo.parent.mkdir(parents=True, exist_ok=True)
    mo.write_text("olean")
    (tmp / ".lake/packages/mathlib/Mathlib.lean").write_text("-- the same in every set\n")
    (tmp / ".lake/packages/mathlib/rev").write_text((tmp / "lake-manifest.json").read_text())


def manifest(rev: str) -> str:
    return json.dumps({"packages": [{"name": "mathlib", "rev": rev}, {"name": "batteries", "rev": "b1"}]})


class PackageSets(unittest.TestCase):
    def test_key(self):
        self.assertTrue(pkgsets.key(manifest("a" * 40)).startswith("a" * 12 + "-"))
        self.assertNotEqual(pkgsets.key(manifest("a" * 40)), pkgsets.key(manifest("b" * 40)))
        self.assertTrue(pkgsets.key(json.dumps({"packages": []})).startswith("nomathlib-"))

    def test_build_dedup_find_prune(self):
        with tempfile.TemporaryDirectory() as d:
            store, log = Path(d) / "store", io.StringIO()
            sets = []
            for rev in ("a" * 40, "b" * 40):
                m = manifest(rev)
                with pkgsets.store_lock(store):
                    sets.append(pkgsets.build_set(store, m, lambda tmp, m=m: (tmp / "lake-manifest.json").write_text(m),
                                                  fake_builder, log, meta={"commit": rev}))
                self.assertEqual(pkgsets.find(store, m), sets[-1])
            a, b = sets
            meta = json.loads((b / "set.json").read_text())
            self.assertEqual((meta["commit"], meta["files_linked"]), ("b" * 40, 2))      # Mathlib.lean and the olean
            same = "lake/packages/mathlib/Mathlib.lean"
            self.assertEqual((a / ("." + same)).stat().st_ino, (b / ("." + same)).stat().st_ino)
            self.assertEqual(pkgsets.sets(store), [a, b])
            tree = Path(d) / "tree"
            pkgsets.link_packages(b, tree)
            self.assertEqual((tree / ".lake" / "pkgset").read_text().strip(), b.name)
            self.assertEqual(pkgsets.prune(store, {b.name}, out=log), [a.name])
            self.assertEqual(pkgsets.sets(store), [b])
            self.assertTrue((tree / ".lake/packages/mathlib/rev").exists())          # a linked tree keeps its files
            (tree / "lake-manifest.json").write_text(manifest("b" * 40))
            self.assertIsNone(pkgsets.adopt(store, tree))       # the store has this set already

    def test_a_build_without_the_olean_fails(self):
        with tempfile.TemporaryDirectory() as d, self.assertRaises(RuntimeError):
            pkgsets.build_set(Path(d), manifest("c" * 40), lambda tmp: None, lambda tmp, log, env: None, io.StringIO())


class SorryText(unittest.TestCase):
    def test_prose_and_strings_are_not_code(self):
        text = '/-- spaces that admit a frame -/\ntheorem a : True := by\n  trivial -- no sorry here\ndef s := "sorry"\n'
        diff = "diff --git a/A.lean b/A.lean\n+++ b/A.lean\n@@ -0,0 +1,4 @@\n" + "".join("+" + l + "\n" for l in text.splitlines())
        self.assertEqual(sorry_text.hits(diff, lambda p: text), [])

    def test_code_is_found(self):
        text = "theorem a : True := by\n  sorry\n"
        diff = "diff --git a/A.lean b/A.lean\n+++ b/A.lean\n@@ -0,0 +1,2 @@\n+theorem a : True := by\n+  sorry\n"
        self.assertEqual([(h["line"], h["word"], h["how"]) for h in sorry_text.hits(diff, lambda p: text)], [(2, "sorry", "code")])
        self.assertEqual(sorry_text.hits(diff, lambda p: None)[0]["how"], "raw line")
        self.assertEqual(sorry_text.hits(diff.replace("A.lean", "A.md"), lambda p: text), [])


class Axioms(unittest.TestCase):
    def test_scan(self):
        self.assertEqual(axioms.scan("theorem a : True := trivial\n-- sorry in a comment\n"), [])
        self.assertEqual(axioms.scan("axiom x : False\ntheorem a : True := by native_decide\n"),
                         ["native_decide", "declares an axiom"])
        self.assertEqual(axioms.scan_lines("a\n  sorry\n"), {"sorry": [2]})
        extra = axioms.FORBIDDEN + [(r"^\s*import\s+Secret\.", "imports a forbidden module")]
        self.assertEqual(axioms.scan("import Secret.X\n", extra), ["imports a forbidden module"])

    def test_check_command_and_its_output(self):
        src = axioms.check_command("chk", "MARK", "solution", ("propext",))
        self.assertIn('elab "#chk " tgt:ident', src)
        self.assertIn("a == ``propext", src)
        self.assertNotIn("Quot.sound", src)
        self.assertEqual(axioms.parse_check("Chk.lean:9:0: info: MARK_OK same_expr=true axioms=[propext, Quot.sound]\n", "MARK"),
                         {"ok": True, "same_expr": True, "axioms": ["propext", "Quot.sound"]})
        self.assertEqual(axioms.parse_check("Chk.lean:9:0: error: MARK_FAIL axioms: [sorryAx]\n", "MARK", rc=1),
                         {"ok": False, "reason": "axioms", "detail": "[sorryAx]"})
        self.assertEqual(axioms.parse_check("error: unknown identifier\n", "MARK", rc=1)["reason"], None)

    def test_a_forged_success_line_does_not_count(self):
        mark = axioms.new_mark()
        self.assertNotEqual(mark, axioms.new_mark())
        good = f"Chk.lean:9:0: info: {mark}_OK same_expr=true axioms=[propext]\n"
        self.assertTrue(axioms.parse_check(good, mark)["ok"])
        # the model's text inside another diagnostic (a universe named after the success line)
        inside = (f"Chk.lean:9:0: error: universe parameters [«{mark}_OK same_expr=true axioms=[]»] vs [u]\n"
                  f"{mark}_OK same_expr=true axioms=[]\n")
        self.assertFalse(axioms.parse_check(inside, mark, rc=1)["ok"])
        self.assertFalse(axioms.parse_check(inside, mark, rc=0)["ok"])          # an error line, whatever the exit
        self.assertFalse(axioms.parse_check(good, mark, rc=1)["ok"])            # a failed run
        self.assertFalse(axioms.parse_check(good.replace(mark, "CHECK_0000000000000000"), mark)["ok"])   # wrong nonce
        self.assertFalse(axioms.parse_check(f"Chk.lean:9:0: info: x {mark}_OK same_expr=true axioms=[]\n", mark)["ok"])
        self.assertFalse(axioms.parse_check(good + good, mark)["ok"])          # one success line, not two
        self.assertFalse(axioms.parse_check(good + f"Chk.lean:9:0: error: {mark}_FAIL axioms: [x]\n", mark)["ok"])


if __name__ == "__main__":
    unittest.main()
