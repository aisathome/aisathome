# SPDX-License-Identifier: Apache-2.0
"""A preemption is a pause, not a failure (Timing.preempt_wait_cap_s): the session loop against an endpoint that
preempts the session's steps N times (fake_opencode), and the egress forwarder's hold after a preempted
response. Offline: no container, no model, no network."""
from __future__ import annotations

import asyncio
import json
import unittest

from aisathome_harness import loop
from aisathome_harness.sandbox import egress_forward as EF

from . import test_loop_characterisation as LC

PROMPTS = {"RESUME_PROMPT": "machine_stop: go on", "FIRST_EDIT_NUDGE": "edit", "LOOP_NUDGE": "loop {signature} {count}"}
# what an aisathome gateway's preempted stream ends with, as opencode writes it
PREEMPT = {"type": "provider.internal", "status": 200,
           "message": "preempted for a person: chat and agent requests go first and the nodes are busy with them; "
                      "try again shortly",
           "response": {"body": json.dumps({"error": {"code": 503, "reason": "preempted", "type": "server_error",
                                                      "message": "preempted for a person: try again shortly"}})}}


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


if __name__ == "__main__":
    unittest.main()
