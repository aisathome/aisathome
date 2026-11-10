# SPDX-License-Identifier: Apache-2.0
"""The kits: loop_guard, stepmetrics, behaviour, evidence, the sandbox (with Docker stubbed, and the egress
forwarder on localhost), and the Lean kit (package sets with a stand-in build, the sorry check, the axiom
scan and check command). Nothing here needs Docker, Lean or a network beyond localhost."""
from __future__ import annotations

import asyncio
import io
import json
import shutil
import socket
import ssl
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


def free_port() -> int:
    probe = socket.socket()
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()
    return port


class Upstream:
    """A one-endpoint HTTP server (TLS with `cert`): records each request head and body, answers `ok`."""

    def __init__(self, cert: tuple[str, str] | None = None, gate: threading.Event | None = None):
        self.seen: list[bytes] = []
        self.gate = gate             # a stream: the first event, then the rest once the gate opens
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen()
        self.port = self.sock.getsockname()[1]
        self.ctx = None
        if cert:
            self.ctx = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
            self.ctx.load_cert_chain(*cert)
        threading.Thread(target=self.serve, daemon=True).start()

    def serve(self):
        while True:
            try:
                c, _ = self.sock.accept()
            except OSError:
                return
            try:
                if self.ctx:
                    c = self.ctx.wrap_socket(c, server_side=True)
                data = b""
                while b"\r\n\r\n" not in data:
                    data += c.recv(65536)
                head, body = data.split(b"\r\n\r\n", 1)
                n = next((int(l.split(b":")[1]) for l in head.split(b"\r\n") if l.lower().startswith(b"content-length:")), 0)
                while len(body) < n:
                    body += c.recv(65536)
                self.seen.append(head + b"\r\n\r\n" + body)
                if self.gate:
                    c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\ndata: 1\n\n")
                    self.gate.wait(5)
                    c.sendall(b"data: 2\n\n")
                    c.close()
                    continue
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\n\r\nok")
                c.close()
            except (OSError, ssl.SSLError):
                pass

    def close(self):
        self.sock.close()


class Forwarder(unittest.TestCase):
    """The forwarder on localhost between a client (the model's side) and a recording endpoint."""

    log: list = []

    @classmethod
    def setUpClass(cls):
        # the forwarders' threads outlive each test: their log goes to a list for the whole class
        cls.quiet = mock.patch.object(egress_forward, "print", create=True,
                                      new=lambda *a, **k: Forwarder.log.append(" ".join(map(str, a))))
        cls.quiet.start()

    @classmethod
    def tearDownClass(cls):
        cls.quiet.stop()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.key = Path(self.tmp.name) / "key"
        self.key.write_text("sk-secret-key-0123456789\n")

    def tearDown(self):
        self.tmp.cleanup()

    def start(self, up: Upstream, scheme: str = "http", prefix: str = "/v1", key: bool = True,
              cafile: str | None = None, host: str = "127.0.0.1") -> int:
        lport = free_port()
        loop_ = asyncio.new_event_loop()
        threading.Thread(target=lambda: loop_.run_until_complete(egress_forward.main_async(
            f"127.0.0.1:{lport}", f"{host}:{up.port}", scheme, prefix, str(self.key) if key else None, cafile)),
            daemon=True).start()
        for _ in range(100):
            try:
                socket.create_connection(("127.0.0.1", lport), timeout=2).close()
                return lport
            except OSError:
                threading.Event().wait(0.05)
        self.fail("forwarder did not start")

    def ask(self, lport: int, request: bytes) -> bytes:
        c = socket.create_connection(("127.0.0.1", lport), timeout=5)
        c.sendall(request)
        out = b""
        while True:
            d = c.recv(65536)
            if not d:
                break
            out += d
        c.close()
        return out

    REQ = (b"POST /v1/chat/completions HTTP/1.1\r\nHost: api.example.org\r\nAuthorization: Bearer dummy\r\n"
           b"Proxy-Authorization: x\r\nConnection: keep-alive, X-Drop\r\nX-Drop: 1\r\nExpect: 100-continue\r\n"
           b"Content-Type: application/json\r\n"
           b"Content-Length: 11\r\n\r\n{\"a\": \"b\"}\n")

    def test_it_adds_the_key_and_pins_the_target(self):
        up = Upstream()
        lport = self.start(up)
        answer = self.ask(lport, self.REQ)
        self.assertTrue(answer.endswith(b"\r\n\r\nok"))
        self.assertEqual(answer.split(b"\r\n\r\n")[0].split(b"\r\n"),         # the client never reuses the connection
                         [b"HTTP/1.1 200 OK", b"Content-Length: 2", b"Connection: close"])
        head, body = up.seen[0].split(b"\r\n\r\n", 1)
        lines = head.split(b"\r\n")
        self.assertEqual(lines[0], b"POST /v1/chat/completions HTTP/1.1")
        self.assertIn(f"Host: 127.0.0.1:{up.port}".encode(), lines)
        self.assertIn(b"Authorization: Bearer sk-secret-key-0123456789", lines)
        self.assertEqual([l for l in lines if l.lower().startswith(b"authorization")], [b"Authorization: Bearer sk-secret-key-0123456789"])
        self.assertIn(b"Connection: close", lines)
        for gone in (b"proxy-authorization", b"x-drop", b"connection: keep-alive", b"host: api.example.org", b"expect"):
            self.assertNotIn(gone, head.lower())
        self.assertEqual(body, b'{"a": "b"}\n')
        threading.Event().wait(0.2)
        self.assertTrue(any("adds the key" in x for x in self.log))
        self.assertFalse([x for x in self.log if "sk-secret" in x])     # never in the forwarder's log
        up.close()

    def test_a_stream_is_passed_on_as_it_comes(self):
        """Server-sent events reach the client one by one (the model's time counts from the first chunk)."""
        gate = threading.Event()
        up = Upstream(gate=gate)
        lport = self.start(up)
        c = socket.create_connection(("127.0.0.1", lport), timeout=5)
        c.sendall(self.REQ)
        got = b""
        while b"data: 1" not in got:
            got += c.recv(65536)
        self.assertNotIn(b"data: 2", got)
        gate.set()
        while b"data: 2" not in got:
            got += c.recv(65536)
        c.close()
        up.close()

    def test_without_a_key_the_client_s_authorization_is_still_dropped(self):
        up = Upstream()
        lport = self.start(up, key=False)
        self.ask(lport, self.REQ)
        self.assertNotIn(b"authorization", up.seen[0].lower())
        up.close()

    def test_requests_off_the_endpoint_s_api_are_refused(self):
        """Absolute-form (another host), CONNECT, paths outside the base path or with dot segments: refused
        before the endpoint is contacted, so the key goes nowhere."""
        up = Upstream()
        lport = self.start(up)
        for line, status in ((b"GET http://evil.example/v1/models HTTP/1.1", b"400"),
                             (b"CONNECT evil.example:443 HTTP/1.1", b"400"),
                             (b"GET /admin HTTP/1.1", b"403"), (b"GET /v1/../admin HTTP/1.1", b"403"),
                             (b"GET /v1/%2e%2e/admin HTTP/1.1", b"403"), (b"GET /v10/models HTTP/1.1", b"403"),
                             (b"GET /v1/models HTTP/1.1\r\n folded: header", b"400")):
            out = self.ask(lport, line + b"\r\nHost: x\r\n\r\n")
            self.assertTrue(out.startswith(b"HTTP/1.1 " + status), (line, out[:40]))
        self.assertEqual(up.seen, [])
        self.assertTrue(self.ask(lport, b"GET /v1/models HTTP/1.1\r\nHost: x\r\n\r\n").endswith(b"ok"))
        self.assertEqual(len(up.seen), 1)
        up.close()

    def test_control_bytes_cannot_split_off_a_second_request(self):
        """A target that passes the base-path check but holds LF, TAB, CR, NUL or DEL could be split into two
        requests by a lenient endpoint, the second to another path with the key (Codex review, 2026-11-10):
        refused before the endpoint is contacted, as are bare CR/LF elsewhere in the head and control bytes
        in a method or header value."""
        up = Upstream()
        lport = self.start(up)
        smuggled = b"\nGET\t/admin\tHTTP/1.1\nX:"
        for line in (b"GET /v1/models" + smuggled + b" HTTP/1.1",            # bare LF in the target
                     b"GET /v1/models\t/admin HTTP/1.1",                       # TAB
                     b"GET /v1/models\r/admin HTTP/1.1",                       # bare CR
                     b"GET /v1/models\x00/admin HTTP/1.1",                     # NUL
                     b"GET /v1/models\x7f/admin HTTP/1.1",                     # DEL
                     b"GET /v1/models\x85 HTTP/1.1",                           # beyond ASCII
                     b"GE\x00T /v1/models HTTP/1.1",                           # in the method
                     b"GET /v1/models HTTP/1.1\nGET /admin HTTP/1.1",          # bare LF ends the request line
                     b"GET /v1/models HTTP/1.1\r\nX-A: 1\nGET /admin HTTP/1.1",  # ... or a header line
                     b"GET /v1/models HTTP/1.1\r\nX-A: 1\x00"):                # control byte in a value
            out = self.ask(lport, line + b"\r\nHost: x\r\n\r\n")
            self.assertTrue(out.startswith(b"HTTP/1.1 400"), (line, out[:40]))
            with self.assertRaises(egress_forward.Refused, msg=repr(line)):
                egress_forward.rewrite(line + b"\r\nHost: x", "api", "/v1", "k")
        self.assertEqual(up.seen, [])
        ok = egress_forward.rewrite(b"GET /v1/models?q=a%20b HTTP/1.1\r\nX-A: tab\there \x85", "api", "/v1", "k")
        self.assertTrue(ok.startswith(b"GET /v1/models?q=a%20b HTTP/1.1\r\nHost: api\r\nX-A: tab\there \x85\r\n"))
        up.close()

    def test_a_key_that_could_break_a_header_is_refused(self):
        for bad in ("", "two words", "line\r\nX-Evil: 1"):
            self.key.write_text(bad)
            with self.assertRaises(SystemExit, msg=repr(bad)):
                egress_forward.read_key(str(self.key))

    def test_an_unreachable_endpoint_is_a_502(self):
        dead = free_port()
        up = mock.Mock(port=dead)
        lport = self.start(up)
        self.assertTrue(self.ask(lport, self.REQ).startswith(b"HTTP/1.1 502"))

    @unittest.skipUnless(shutil.which("openssl"), "needs openssl to make a test certificate")
    def test_tls_to_the_endpoint_with_its_certificate_checked(self):
        """The endpoint speaks TLS with a certificate for `localhost`: the forwarder checks it against the
        given CA and the name, and the request arrives with the key; with the wrong name it is refused."""
        d = Path(self.tmp.name)
        cert, keyf = d / "cert.pem", d / "key.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=localhost",
                        "-addext", "subjectAltName=DNS:localhost", "-keyout", str(keyf), "-out", str(cert)],
                       check=True, capture_output=True)
        up = Upstream((str(cert), str(keyf)))
        lport = self.start(up, scheme="https", cafile=str(cert), host="localhost")
        self.assertTrue(self.ask(lport, self.REQ).endswith(b"ok"))
        self.assertIn(b"Authorization: Bearer sk-secret-key-0123456789", up.seen[0])
        self.assertIn(f"Host: localhost:{up.port}".encode(), up.seen[0])
        # the same certificate under another name: the TLS check fails, the client gets a 502, no key sent
        lport2 = self.start(up, scheme="https", cafile=str(cert), host="127.0.0.1")
        self.assertTrue(self.ask(lport2, self.REQ).startswith(b"HTTP/1.1 502"))
        self.assertEqual(len(up.seen), 1)
        up.close()


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
