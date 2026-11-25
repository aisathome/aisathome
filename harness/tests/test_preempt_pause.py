# SPDX-License-Identifier: Apache-2.0
"""A preemption is a pause, not a failure (Timing.preempt_wait_cap_s): the session loop against an endpoint that
preempts the session's steps N times (fake_opencode), and the egress forwarder's hold after a preempted
response. Offline: no container, no model, no network."""
from __future__ import annotations

import asyncio
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import loop, stepmetrics
from aisathome_harness.sandbox import egress_forward as EF

from . import test_loop_characterisation as LC

PROMPTS = {"RESUME_PROMPT": "machine_stop: go on", "FIRST_EDIT_NUDGE": "edit", "LOOP_NUDGE": "loop {signature} {count}"}
# what an aisathome gateway's preempted stream ends with, as opencode writes it
PREEMPT = {"type": "provider.internal", "status": 200,
           "message": "preempted for a person: chat and agent requests go first and the nodes are busy with them; "
                      "try again shortly",
           "response": {"body": json.dumps({"error": {"code": 503, "reason": "preempted", "type": "server_error",
                                                      "message": "preempted for a person: try again shortly"}})}}


# What an aisathome gateway sends for a preemption, byte for byte (gateway/preempt.go, Go's encoding/json: keys
# sorted, no spaces): the 503 body of a request that had not started streaming (preemptedError, json.Encoder's
# newline included), and the error event that ends a stream that had (relay.preempted through sseError/writeSSE).
GW_MSG = "preempted for a person: chat and agent requests go first and the nodes are busy with them; try again shortly"
GW_503_BODY = ('{"error":{"code":"preempted","message":"' + GW_MSG + '","type":"server_error"}}\n').encode()
GW_SSE_EVENT = ('data: {"error":{"code":503,"message":"' + GW_MSG + '","reason":"preempted","type":"server_error"}}'
                '\n\n').encode()
# opencode v2's error event for an endpoint's HTTP 503 (its shape as in test_rounds.ERR_503) carrying the gateway's
# pre-stream body; the message is a generic one here, so only the body names the preemption
PREEMPT_503 = {"type": "provider.internal", "message": "Service Unavailable", "status": 503,
               "response": {"body": GW_503_BODY.decode()}}


def preempted_run(n: int) -> list[dict]:
    """n invocations that each do one step, are preempted, retry and exit inside the retried step (as opencode
    did on 2026-11-25, 01:00-04:12 UTC), then one that finishes."""
    inv = [{"actions": [LC.step("ls"), {"error": PREEMPT, "after_ms": 40000}, {"start": {}}], "exit": 1}
           for _ in range(n)]
    return inv + [{"actions": [LC.step("ls"), LC.talk("RESULT: done")], "exit": 0}]


def play(n: int, timing: dict | None = None) -> dict:
    return LC.play("preempt", PROMPTS, ({}, 10, 30, ["fresh"], preempted_run(n)), timing=timing)


class Pause(unittest.TestCase):
    def test_without_the_setting_the_session_ends_at_the_first_preemption(self):
        r = play(5)
        self.assertEqual(len(r["calls"]), 1)                     # opencode "recovered" (a step started): no resume
        self.assertNotIn("preempt_wait_s", r["runs"][0])

    def test_preempted_five_times_then_finished(self):
        r = play(5, {"preempt_wait_cap_s": 7200})
        run = r["runs"][0]
        self.assertEqual(len(r["calls"]), 6)                     # five resumes, not max_resumes (3)
        self.assertEqual((run["preempted"], run["preempt_wait_exhausted"]), (5, False))
        self.assertGreaterEqual(run["preempt_wait_s"], 5 * 40 + 15 + 30 + 60 + 60 + 60)   # gaps + backoffs
        self.assertEqual(run["steps"], 7)
        self.assertIn("RESULT: done", json.dumps(r["events"]))

    def test_the_wait_cap_ends_the_session_for_a_give_back(self):
        r = play(5, {"preempt_wait_cap_s": 200})
        run = r["runs"][0]
        self.assertTrue(run["preempt_wait_exhausted"])
        self.assertLess(len(r["calls"]), 6)
        self.assertLessEqual(run["preempt_wait_s"], 200 + 40)

    def test_the_wait_is_not_model_time(self):
        r = play(2, {"preempt_wait_cap_s": 7200})
        self.assertLess(r["runs"][0]["model_s"], 10)             # 4 one-second steps; the 80 s of waits not counted

    def test_preempt_stats_and_the_end_inside_a_preempted_step(self):
        ev = (b'{"type":"step_start","timestamp":1000}\n'
              b'{"type":"error","timestamp":2000,"error":{"message":"preempted for a person"}}\n'
              b'{"type":"step_start","timestamp":32000}\n'
              b'{"type":"error","timestamp":40000,"error":{"message":"503 busy"}}\n'
              b'{"type":"error","timestamp":41000,"error":{"message":"preempted for a person"}}\n')
        self.assertEqual(loop.preempt_stats(ev, 51.0), {"preempted": 2, "preempt_wait_s": 40.0})
        tail = ev.decode()
        self.assertTrue(loop.ended_in_preempted_step(tail))
        self.assertFalse(loop.ended_in_preempted_step(tail + '{"type":"step_finish","timestamp":60000}\n'))
        self.assertFalse(loop.ended_in_preempted_step(tail + '{"type":"error","timestamp":1,"error":"503 busy"}\n'))
        seg = {"suspend_interrupted": False, "timed_out": False, "exit": 1}
        self.assertTrue(loop.gateway_interrupted(seg, tail + '{"type":"step_start","timestamp":1}\n', preempt_pause=True))
        self.assertFalse(loop.gateway_interrupted(seg, tail + '{"type":"step_start","timestamp":1}\n'))

    def test_both_gateway_formats_are_preemptions_to_the_loop(self):
        """The gateway's message is in its pre-stream 503 body and in its stream event alike: the loop and the
        step metrics recognise either as opencode writes it."""
        for err in (PREEMPT, PREEMPT_503):
            with self.subTest(err["type"] + str(err.get("status"))):
                ev_line = json.dumps({"type": "error", "timestamp": 2000, "error": err}, separators=(",", ":"))
                ev = '{"type":"step_start","timestamp":1000}\n' + ev_line + '\n{"type":"step_start","timestamp":32000}\n'
                self.assertEqual(loop.preempt_stats(ev.encode()), {"preempted": 1, "preempt_wait_s": 30.0})
                self.assertTrue(loop.ended_in_preempted_step(ev))
                self.assertEqual(loop.stall_limit(ev_line), loop.DEFAULT_TIMING.stall_after_preempt_s)
                rows = stepmetrics.step_rows([(0, json.loads(x)) for x in
                                              (ev + '{"type":"step_finish","timestamp":33000}\n').splitlines()])
                self.assertTrue(rows[0]["preempted"])


E = 1_000_000          # the fake clock's epoch seconds (far behind the files' real mtimes: no stall is seen)


class LiveAwakeTimer(unittest.TestCase):
    """run_segment's own awake timer, on a fake clock that moves 60 s per poll once opencode has written its error."""

    def segment(self, err: dict, cap_s: float) -> dict:
        events = "".join(json.dumps(e, separators=(",", ":")) + "\n" for e in (       # compact, as opencode writes
            {"type": "step_start", "timestamp": E * 1000}, {"type": "step_finish", "timestamp": (E + 1) * 1000},
            {"type": "error", "timestamp": (E + 2) * 1000, "error": err}))
        script = f"import sys, time; sys.stdout.write({events!r}); sys.stdout.flush(); time.sleep(1.5); sys.exit(1)"
        t = [float(E)]
        with tempfile.TemporaryDirectory() as d:
            raw = Path(d)

            def tick(_):                      # opencode waits for its retry: 60 s pass per poll
                if b'"type":"error"' in (raw / "events.jsonl").read_bytes():
                    t[0] += 60
            with mock.patch.object(loop.time, "monotonic", lambda: t[0]), \
                    mock.patch.object(loop.time, "time", lambda: t[0]):
                seg = loop.run_segment([sys.executable, "-c", script], raw, 100, on_tick=tick,
                                       timing=loop.Timing(poll_s=0.05, stall_after_error_s=1e9,
                                                          stall_after_preempt_s=1e9, preempt_wait_cap_s=cap_s))
        return seg, t[0] - E

    def test_the_wait_after_a_preemption_is_not_awake_time(self):
        seg, elapsed = self.segment(PREEMPT, 7200)
        self.assertGreater(elapsed, 300)      # the fake clock went well past the 100 s awake budget
        self.assertFalse(seg["timed_out"])    # opencode is left to its retry, not killed as out of time
        self.assertEqual(seg["exit"], 1)
        self.assertTrue(seg["gateway_interrupted"])
        self.assertGreaterEqual(seg["preempt_wait_s"], 300)

    def test_other_waits_and_the_setting_off_still_count(self):
        for err, cap_s in (({"message": "503 busy"}, 7200), (PREEMPT, 0)):
            with self.subTest(err=err["message"][:9], cap_s=cap_s):
                seg, elapsed = self.segment(err, cap_s)
                self.assertEqual((seg["timed_out"], seg["cap"], elapsed), (True, "time", 120))


class Hold(unittest.TestCase):
    def test_after_a_preemption_the_next_request_waits_doubling_to_the_cap(self):
        t = [0.0]
        slept = []

        async def sleep(d):
            slept.append(d)
            t[0] += d
        h = EF.Hold(base_s=5, max_s=60, clock=lambda: t[0], sleep=sleep)
        asyncio.run(h.wait())
        self.assertEqual(slept, [])                              # nothing preempted: no wait
        for _ in range(6):
            h.preempted()
            asyncio.run(h.wait())
        self.assertEqual(slept, [5, 10, 20, 40, 60, 60])
        h.preempted()
        h.ok()
        asyncio.run(h.wait())
        self.assertEqual(len(slept), 6)                          # a clean response ends the pause
        self.assertEqual(h.counters["held"], 6)

    def test_the_marker_is_found_across_chunks(self):
        w = EF.Watch(b'"reason":"preempted"')
        for chunk in (b'data: {"error":{"code":503,"rea', b'son":"preempted","type":"server_error"}}\n\n'):
            w.feed(chunk)
        self.assertTrue(w.seen)
        w2 = EF.Watch(b'"reason":"preempted"')
        w2.feed(b'data: {"choices":[{"delta":{"content":"preempted"}}]}\n\n')
        self.assertFalse(w2.seen)

    def test_both_gateway_formats_are_seen_also_split_and_quoted_text_is_not(self):
        for body in (GW_503_BODY, GW_SSE_EVENT):
            for cut in range(1, len(body)):
                w = EF.Watch(*EF.PREEMPT_MARKS)
                w.feed(body[:cut])
                w.feed(body[cut:])
                self.assertTrue(w.seen, (body, cut))
        w = EF.Watch(*EF.PREEMPT_MARKS)       # the model writing the marks: JSON strings, quotes escaped
        w.feed(b'data: {"choices":[{"delta":{"content":"\\"code\\":\\"preempted\\" \\"reason\\":\\"preempted\\""}}]}\n\n')
        self.assertFalse(w.seen)


class ForwarderHold(unittest.TestCase):
    """The forwarder between a client and a stand-in gateway that answers with the gateway's exact bytes."""

    RESPONSES = {
        "503": b"HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nRetry-After: 10\r\n"
               b"Content-Length: " + str(len(GW_503_BODY)).encode() + b"\r\n\r\n" + GW_503_BODY,
        "sse": b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n\r\n"
               b'data: {"choices":[{"delta":{"content":"hi"}}]}\n\n' + GW_SSE_EVENT,
        "ok": b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"
              b'data: {"choices":[{"delta":{"content":"preempted"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n',
    }

    def test_each_gateway_preemption_starts_the_hold_and_a_clean_answer_ends_it(self):
        slept = []

        async def sleep(d):
            slept.append(d)

        async def main():
            answers = []

            async def gateway(r, w):
                await r.readuntil(b"\r\n\r\n")
                await r.readexactly(2)
                w.write(self.RESPONSES[answers.pop(0)])
                await w.drain()
                w.close()
            gw = await asyncio.start_server(gateway, "127.0.0.1", 0)
            hold = EF.Hold(sleep=sleep)
            fw = EF.Forwarder("127.0.0.1", gw.sockets[0].getsockname()[1], "http", "/v1", None, hold=hold)
            srv = await asyncio.start_server(fw.handle, "127.0.0.1", 0)
            port = srv.sockets[0].getsockname()[1]

            async def ask(kind: str) -> bytes:
                answers.append(kind)
                r, w = await asyncio.open_connection("127.0.0.1", port)
                w.write(b"POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}")
                await w.drain()
                got = await asyncio.wait_for(r.read(), 10)
                w.close()
                await asyncio.sleep(0.05)     # the forwarder's bookkeeping after both pipes end
                return got
            seen = []
            for kind in ("503", "sse", "ok", "sse"):
                got = await ask(kind)
                self.assertIn(self.RESPONSES[kind].split(b"\r\n\r\n", 1)[1], got)   # passed through as it came
                seen.append(hold.counters["preempted"])
            self.assertEqual(seen, [1, 2, 2, 3])
            self.assertEqual(hold.n, 1)               # the clean answer ended the run of preemptions
            self.assertEqual([round(x) for x in slept], [5, 10])    # the two requests after a preemption were held
            srv.close()
            gw.close()
        asyncio.run(asyncio.wait_for(main(), 30))


if __name__ == "__main__":
    unittest.main()
