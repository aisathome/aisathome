# SPDX-License-Identifier: Apache-2.0
"""queue.QueueClient against a fake gateway on 127.0.0.1: the requests it sends (paths, bodies, the key only in
Authorization and never on a redirect, no key for the public queue), the older gateway's compatibility path,
typed errors and retries."""
from __future__ import annotations

import http.server
import json
import tempfile
import threading
import unittest
from pathlib import Path

from aisathome_harness import queue as Q


class FakeGateway:
    """Records each request (method, path, auth header, body) and answers from `answers` (path -> list of
    (status, body)), else 200 {}."""

    def __init__(self):
        self.seen: list[dict] = []
        self.answers: dict[str, list[tuple[int, dict]]] = {}
        me = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _do(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n)) if n else None
                me.seen.append({"method": self.command, "path": self.path, "auth": self.headers.get("Authorization"),
                                "ua": self.headers.get("User-Agent"), "body": body})
                q = me.answers.get(self.path) or []
                status, out = q.pop(0) if q else (200, {})
                self.send_response(status)
                if status in (301, 302, 307):
                    self.send_header("Location", out.get("location", "/elsewhere"))
                data = b"" if status == 204 else json.dumps(out).encode()
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = _do

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class Client(unittest.TestCase):
    def setUp(self):
        self.gw = FakeGateway()
        self.addCleanup(self.gw.close)
        d = Path(tempfile.mkdtemp())
        (d / "k").write_text("test-key-0001\n")
        self.c = Q.QueueClient(self.gw.url, d / "k", "proj")

    def test_each_call_its_path_and_body_with_the_key_in_authorization(self):
        c = self.c
        c.create({"id": "t1", "kind": "other", "spec": {}, "created_by": "me"})
        c.list(state="queued", kind="other")
        c.get("t1/a")
        c.claim("w1", 600, kinds=["other"], hold_seconds=0)
        c.claim("w1", 300, task="t1", kinds=["other"])
        c.heartbeat("t1", "w1", "L1")
        c.complete("t1", "w1", "L1", {"outcome": "ok"})
        c.fail("t1", "w1", "L1", "boom\nsk-abcdef123456")
        c.give_back("t1", "w1", "L1")
        c.cancel("t1", "not needed")
        c.exported("t1", "2026-11-14T00:00:00Z")
        got = [(s["method"], s["path"], s["body"]) for s in self.gw.seen]
        base = "/v1/projects/proj/tasks"
        self.assertEqual(got[:4], [
            ("POST", base, {"id": "t1", "kind": "other", "spec": {}, "created_by": "me"}),
            ("GET", base + "?state=queued&kind=other", None),
            ("GET", base + "/t1/a", None),
            ("POST", base + "/claim", {"worker": "w1", "lease_seconds": 600, "kinds": ["other"], "hold_seconds": 0})])
        self.assertEqual(got[4][2], {"worker": "w1", "lease_seconds": 300, "task": "t1"})     # a task: no kinds
        self.assertEqual(got[5], ("POST", base + "/t1/heartbeat", {"worker": "w1", "lease": "L1"}))
        self.assertEqual(got[6][2]["result"], {"outcome": "ok"})
        self.assertEqual(got[7][2]["requeue"], False)
        self.assertNotIn("\n", got[7][2]["reason"])
        self.assertIsNone(Q.SECRET_LIKE.search(got[7][2]["reason"]))
        self.assertEqual((got[8][1], got[8][2]["requeue"]), (base + "/t1/fail", True))
        self.assertEqual(got[9], ("POST", base + "/t1/cancel", {"reason": "not needed"}))
        self.assertEqual(got[10], ("POST", base + "/t1/exported", {"seen": "2026-11-14T00:00:00Z"}))
        self.assertTrue(all(s["auth"] == "Bearer test-key-0001" for s in self.gw.seen))
        self.assertEqual(self.gw.seen[0]["ua"], "aisathome-harness-queue")

    def test_the_key_can_be_given_and_the_project_is_required(self):
        c = Q.QueueClient(self.gw.url, None, "p2", key="k2")
        c.list()
        self.assertEqual((self.gw.seen[-1]["path"], self.gw.seen[-1]["auth"]), ("/v1/projects/p2/tasks", "Bearer k2"))
        with self.assertRaises(ValueError):
            Q.QueueClient(self.gw.url, None, "p2")
        with self.assertRaises(ValueError):
            Q.QueueClient(self.gw.url, None, "", key="k")

    def test_the_public_queue_goes_without_the_key(self):
        self.gw.answers["/v1/projects/proj/queue"] = [(200, {"project": "proj", "tasks": []})]
        code, body = self.c.public_queue()
        self.assertEqual((code, body["project"]), (200, "proj"))
        self.assertIsNone(self.gw.seen[-1]["auth"])

    def test_the_key_never_follows_a_redirect(self):
        self.gw.answers["/v1/projects/proj/tasks"] = [(302, {"location": "/stolen"})]
        code, _ = self.c.list()
        self.assertEqual(code, 302)
        self.assertEqual([s["path"] for s in self.gw.seen], ["/v1/projects/proj/tasks"])     # /stolen never asked

    def test_an_older_gateway_gets_the_result_without_cached_tokens(self):
        p = "/v1/projects/proj/tasks/t1/complete"
        self.gw.answers[p] = [(400, {"error": {"message": 'not a valid request: json: unknown field "cached_tokens"'}}),
                              (200, {"task": {}})]
        r = self.c.complete("t1", "w", "L", {"outcome": "proved", "prompt_tokens": 1, "cached_tokens": 9})
        self.assertEqual(r.status, 200)
        sent = [s["body"]["result"] for s in self.gw.seen]
        self.assertEqual(sent, [{"outcome": "proved", "prompt_tokens": 1, "cached_tokens": 9},
                                {"outcome": "proved", "prompt_tokens": 1}])
        # another 400 is not retried
        self.gw.seen.clear()
        self.gw.answers[p] = [(400, {"error": {"message": "outcome: too long"}})]
        self.assertEqual(self.c.complete("t1", "w", "L", {"outcome": "x", "cached_tokens": 1}).status, 400)
        self.assertEqual(len(self.gw.seen), 1)

    def test_an_older_gateway_gets_the_claim_without_hold_seconds_from_then_on(self):
        p = "/v1/projects/proj/tasks/claim"
        self.gw.answers[p] = [(400, {"error": {"message": 'not a valid request: json: unknown field "hold_seconds"'}}),
                              (204, {})]
        self.assertEqual(self.c.claim("w", 600, kinds=["other"], hold_seconds=0).status, 204)
        self.assertTrue(self.c.hold_refused)
        self.c.claim("w", 600, task="t1", hold_seconds=0)
        sent = [s["body"] for s in self.gw.seen]
        self.assertEqual(sent, [{"worker": "w", "lease_seconds": 600, "kinds": ["other"], "hold_seconds": 0},
                                {"worker": "w", "lease_seconds": 600, "kinds": ["other"]},
                                {"worker": "w", "lease_seconds": 600, "task": "t1"}])
        # another 400 (a value out of range) is the gateway's answer, not retried
        c = Q.QueueClient(self.gw.url, None, "proj", key="k")
        self.gw.seen.clear()
        self.gw.answers[p] = [(400, {"error": {"message": "hold_seconds: 0 to 600"}})]
        self.assertEqual(c.claim("w", hold_seconds=601).status, 400)
        self.assertEqual((len(self.gw.seen), c.hold_refused), (1, False))

    def test_typed_errors_from_check(self):
        self.gw.answers["/v1/projects/proj/tasks/t1/heartbeat"] = [(409, {"error": {"message": "lease lost"}})]
        r = self.c.heartbeat("t1", "w", "L")
        self.assertEqual((r.status, r.error, r.ok), (409, "lease lost", False))
        with self.assertRaises(Q.Conflict) as e:
            r.check()
        self.assertEqual((e.exception.status, e.exception.message), (409, "lease lost"))
        self.gw.answers["/v1/projects/proj/tasks/claim"] = [(204, {})]
        r = self.c.claim("w")
        self.assertEqual(r.check(200, 204), {})
        for status, cls in ((400, Q.BadRequest), (401, Q.Unauthorized), (403, Q.Forbidden), (404, Q.NotFound),
                            (503, Q.Unavailable), (500, Q.QueueError)):
            with self.assertRaises(cls):
                Q.Reply(status, {}).check()

    def test_retries_a_network_failure_with_backoff_only_when_asked(self):
        waits = []
        dead = "http://127.0.0.1:9"                       # nothing listens there
        c = Q.QueueClient(dead, None, "p", key="k", retries=2, retry_wait_s=0.5, sleep=waits.append, timeout=2)
        with self.assertRaises(OSError):
            c.list()
        self.assertEqual(waits, [0.5, 1.0])
        waits.clear()
        with self.assertRaises(OSError):
            Q.QueueClient(dead, None, "p", key="k", sleep=waits.append, timeout=2).list()
        self.assertEqual(waits, [])

    def test_helpers(self):
        self.assertEqual(Q.task_headers("t5-F01"), {"X-Aisathome-Task": "t5-F01"})
        self.assertEqual(Q.last_note({"events": [{"note": "a"}, {"note": "b"}]}), "b")
        self.assertEqual(Q.last_note({}), "")
        self.assertEqual(Q.task_class("fix_round", "prereview"), Q.CLASS_FIX_PREREVIEW)
        self.assertEqual(Q.task_class("followup"), Q.CLASS_ATTEMPT)
        s = Q.safe_text("/srv/runner/work/prove/runs/p2m-t4-O02/state/overlay/0/work/work", 500)
        self.assertEqual(s, "…/work")
        self.assertEqual(Q.error_text({"error": {"message": "m"}}), "m")
        self.assertEqual(Q.error_text([]), "")


if __name__ == "__main__":
    unittest.main()
