# SPDX-License-Identifier: Apache-2.0
"""A preemption is a pause, not a failure (Timing.preempt_wait_cap_s): the session loop against an endpoint that
preempts the session's steps N times (fake_opencode), run_segment's clocks (fake: awake, waiting after a
preemption, suspended), a round's wait kept across a stop of the worker (stand-in segments), and the egress
forwarder's hold after a preempted response. Offline: no container, no model, no network."""
from __future__ import annotations

import asyncio
import json
import sys
import tempfile
import time
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

    def test_a_later_endpoint_error_after_a_recovered_preemption_is_an_ordinary_interruption(self):
        """opencode recovers from a preemption (its retry finishes a step), then exits on a 502: that is an
        endpoint error, resumed at most max_resumes times, not a preemption's pause."""
        err_502 = {"type": "provider.internal", "status": 502, "message": "Bad Gateway",
                   "response": {"body": "bad gateway"}}
        inv = [{"actions": [LC.step("ls"), {"error": PREEMPT, "after_ms": 1000}, LC.step("ls"), {"error": err_502}],
                "exit": 1} for _ in range(5)] + [{"actions": [LC.step("ls"), LC.talk("RESULT: done")], "exit": 0}]
        r = LC.play("preempt", PROMPTS, ({}, 10, 30, ["fresh"], inv), timing={"preempt_wait_cap_s": 7200})
        run = r["runs"][0]
        self.assertEqual(len(r["calls"]), 1 + loop.Timing.max_resumes)
        self.assertEqual((run["preempted"], run["resumed"]), (4, 3))
        self.assertFalse(any("preempt_pause_s" in sg or sg["preempt_ended"] for sg in run["segments"]))

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

    def test_another_error_ends_a_preemption_s_wait(self):
        """opencode's retry after a preemption fails with a 502 before any step starts: from there it is the
        endpoint's ordinary trouble (resumed under max_resumes), not a preemption's wait."""
        ev = (b'{"type":"step_start","timestamp":1000}\n'
              b'{"type":"error","timestamp":2000,"error":{"message":"preempted for a person"}}\n'
              b'{"type":"error","timestamp":10000,"error":{"type":"provider.internal","status":502,'
              b'"message":"Bad Gateway"}}\n')
        self.assertEqual(loop.preempt_stats(ev, 51.0), {"preempted": 1, "preempt_wait_s": 8.0})
        self.assertFalse(loop.ended_in_preempted_step(ev.decode()))

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

    def segment(self, err: dict, cap_s: float, active_left_s: float = 100, suspend_at: int | None = None,
                **kw) -> tuple[dict, float]:
        """`suspend_at`: the machine sleeps 30 min after that many polls of waiting (the wall clock jumps, the
        monotonic one does not)."""
        events = "".join(json.dumps(e, separators=(",", ":")) + "\n" for e in (       # compact, as opencode writes
            {"type": "step_start", "timestamp": E * 1000}, {"type": "step_finish", "timestamp": (E + 1) * 1000},
            {"type": "error", "timestamp": (E + 2) * 1000, "error": err}))
        script = f"import sys, time; sys.stdout.write({events!r}); sys.stdout.flush(); time.sleep(1.5); sys.exit(1)"
        mono, wall, polls = [float(E)], [float(E)], [0]
        with tempfile.TemporaryDirectory() as d:
            raw = Path(d)

            def tick(_):                      # opencode waits for its retry: 60 s pass per poll
                if b'"type":"error"' in (raw / "events.jsonl").read_bytes():
                    mono[0] += 60
                    wall[0] += 60
                    polls[0] += 1
                    if polls[0] == suspend_at:
                        wall[0] += 1800
            with mock.patch.object(loop.time, "monotonic", lambda: mono[0]), \
                    mock.patch.object(loop.time, "time", lambda: wall[0]):
                seg = loop.run_segment([sys.executable, "-c", script], raw, active_left_s, on_tick=tick,
                                       timing=loop.Timing(poll_s=0.05, stall_after_error_s=1e9,
                                                          stall_after_preempt_s=1e9, preempt_wait_cap_s=cap_s), **kw)
        return seg, mono[0] - E

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

    def test_a_segment_still_waiting_when_the_round_s_wait_cap_is_spent_is_stopped(self):
        """opencode retrying a preempted step on its own can wait past the cap inside one segment: the live
        check stops it there, and the session ends exhausted (not after the segment, if ever)."""
        for err in (PREEMPT, PREEMPT_503):
            with self.subTest(status=err["status"]):
                seg, elapsed = self.segment(err, 7200, active_left_s=1e6, preempt_wait_left_s=150)
                self.assertTrue(seg.get("preempt_wait_exhausted"))
                self.assertEqual((seg["timed_out"], elapsed), (False, 180))     # waited 58, 118, 178 s: stopped
                self.assertTrue(seg["preempt_ended"])
        # another error's silence is not a preemption's wait, also when the round's wait is spent already
        seg, _ = self.segment({"message": "503 busy"}, 7200, active_left_s=1e6, preempt_wait_left_s=-1)
        self.assertNotIn("preempt_wait_exhausted", seg)

    def test_a_suspend_during_the_wait_is_not_waiting(self):
        """The event times are the wall clock; the wait is counted on the awake (monotonic) clock as the awake
        time is, so the suspend neither shortens the awake budget nor spends the wait cap."""
        seg, elapsed = self.segment(PREEMPT, 7200, active_left_s=1e6, suspend_at=3)
        self.assertTrue(seg["suspend_interrupted"])
        self.assertEqual((seg["suspended_s"], seg["active_s"], elapsed), (1800, 180, 180))
        self.assertEqual(seg["preempt_wait_s"], 178.0)       # from the preemption (E+2) on, awake


class Rig:
    """run_session with stand-in segments (Session.segment): each scripted segment appends its events
    (`waited` s after a preemption, then the end in that step, or `err` instead) and returns its record;
    the loop's sleeps are recorded, not slept. `stop_in` (a segment's index): the worker stops there, after
    the segment's first checkpoint (as when the host stops in the middle of it)."""

    def __init__(self, d: Path, timing: dict):
        self.root, self.timing, self.kw, self.slept, self.left = d, timing, [], [], []
        self.tree, self.raw = d / "tree", d / "raw"
        self.tree.mkdir()
        (self.tree / "f").write_text("base\n")
        self.raw.mkdir()
        (d / "home").mkdir()

    def segment(self, spec: dict):
        def seg(cmd, raw, left, **kw):
            self.kw.append(kw)
            self.left.append(left)                         # the awake time the loop allowed it
            now = time.time()
            t0 = now - spec.get("waited", 0) - 2
            evs = [{"type": "step_start", "timestamp": int(t0 * 1000), "sessionID": "ses_r"},
                   {"type": "step_finish", "timestamp": int((t0 + 1) * 1000), "sessionID": "ses_r"}]
            if spec.get("err"):
                evs.append({"type": "error", "timestamp": int((t0 + 2) * 1000), "error": spec["err"]})
            elif "waited" in spec:
                evs += [{"type": "error", "timestamp": int((t0 + 2) * 1000), "error": PREEMPT},
                        {"type": "step_start", "timestamp": int(now * 1000)}]     # opencode exits in the retry
            with open(raw / "events.jsonl", "a") as fh:
                fh.write("".join(json.dumps(e, separators=(",", ":")) + "\n" for e in evs))
            if spec.get("stop"):
                kw["on_tick"](spec["waited"] + 2)          # the segment's first checkpoint
                raise KeyboardInterrupt("the worker stopped")
            out = {"exit": 0 if spec.get("done") else 1, "timed_out": False, "suspend_interrupted": False,
                   "active_s": spec.get("active_s", spec.get("waited", 0) + 2), "wall_s": spec.get("waited", 0) + 2, "suspended_s": 0,
                   "model_s": 1, "open_step_s": 0}
            if not spec.get("done"):
                out["gateway_interrupted"] = True
            if "waited" in spec:
                out.update(preempted=1, preempt_wait_s=float(spec["waited"]), preempt_ended=True)
            if spec.get("exhausted"):                  # run_segment stopped it on the wait cap
                out["preempt_wait_exhausted"] = True
            return out
        return seg

    def run(self, specs: list[dict], resume: bool = False) -> dict:
        segs = iter(specs)
        s = loop.Session(task_id="t", tree=self.tree, raw=self.raw, home=self.root / "home",
                         state=self.root / "state", command=lambda m, sid: ["true"], prompt=lambda st, m: "go",
                         timing=loop.Timing(checkpoint_s=0, **self.timing), loop_guard=False, log=lambda m: None,
                         segment=lambda cmd, raw, left, **kw: self.segment(next(segs))(cmd, raw, left, **kw))
        with mock.patch.object(loop.time, "sleep", self.slept.append):
            loop.run_session(s, "http://127.0.0.1:9/v1", None, "m", 60, 100, resume=resume)
        return json.loads((self.raw / "run.json").read_text())


class RoundTime(unittest.TestCase):
    """A round's time on the record: each segment's wait after preemptions and the backoff after it, kept
    across a stop of the worker, and what counts against max_resumes."""

    T = {"preempt_wait_cap_s": 300, "preempt_backoff_s": 15, "preempt_backoff_max_s": 60}

    def test_a_stopped_worker_s_resume_goes_on_counting_the_round_s_wait(self):
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            with self.assertRaises(KeyboardInterrupt):
                rig.run([{"waited": 40}, {"waited": 40}, {"waited": 50, "stop": True}])
            self.assertEqual(rig.slept, [15, 30])
            partial = json.loads((rig.raw / "run.json").read_text())
            self.assertTrue(partial["partial"])
            self.assertAlmostEqual(partial["segments"][-1]["preempt_wait_s"], 50, delta=1)   # the stopped one's wait
            run = rig.run([{"waited": 40}, {"waited": 40}], resume=True)
            # 40+15, 40+30, 50 before the stop: 175 s of the 300 left 125 when the resumed segment began
            self.assertAlmostEqual(rig.kw[3]["preempt_wait_left_s"], 125, delta=1)
            self.assertEqual(rig.slept, [15, 30, 60])          # the backoff goes on doubling
            self.assertTrue(run["preempt_wait_exhausted"])     # 175 + 40 + 60 + 40 = 315 > 300
            self.assertAlmostEqual(run["preempt_wait_s"], 315, delta=1)
            self.assertEqual(run["preempted"], 5)

    def test_preemption_pauses_before_a_restart_do_not_count_against_max_resumes(self):
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), {**self.T, "preempt_wait_cap_s": 7200})
            with self.assertRaises(KeyboardInterrupt):
                rig.run([{"waited": 10}, {"waited": 10}, {"waited": 10}, {"waited": 10, "stop": True}])
            self.assertEqual(rig.slept, [15, 30, 60])
            run = rig.run([{"err": {"type": "provider.internal", "status": 502, "message": "Bad Gateway"}},
                           {"done": True}], resume=True)
            self.assertEqual(rig.slept[3:], [2 * loop.Timing.gateway_backoff_s])   # the 502 resumed (2 of 3:
                                                                                   # the stop was the first)
            self.assertEqual((run["resumed"], run["exit"]), (2, 0))    # the stop and the 502

    def test_a_resume_that_finds_the_round_s_budget_spent_ends_on_it_not_on_the_old_pause(self):
        """The round spent its wait cap and, in the same segments, its awake budget (3 x 60 min): the resume
        after the give-back runs no segment, and its run.json says the budget ended the round, not a pause
        again (which would have the task given back and resumed for ever)."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            run = rig.run([{"waited": 40}, {"waited": 40, "exhausted": True, "active_s": 11000}])
            self.assertTrue(run["preempt_wait_exhausted"])
            self.assertEqual(run["timed_out"], False)
            run = rig.run([], resume=True)
            self.assertEqual(len(rig.kw), 2)                       # no segment ran
            self.assertEqual((run["preempt_wait_exhausted"], run["timed_out"], run["cap"]),
                             (False, True, "awake ceiling"))
            self.assertTrue(run["segments"][-1]["preempt_wait_exhausted"])     # the history is kept

    def test_a_resume_after_a_give_back_starts_a_fresh_wait(self):
        """The round spent its wait cap and was given back: the resume's wait budget and backoff start again,
        the waits before the give-back stay on the record but no longer count against the cap."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            run = rig.run([{"waited": 40}, {"waited": 40, "exhausted": True}])
            self.assertTrue(run["preempt_wait_exhausted"])
            self.assertEqual(rig.slept, [15])
            run = rig.run([{"waited": 40}, {"done": True}], resume=True)
            self.assertAlmostEqual(rig.kw[2]["preempt_wait_left_s"], 300, delta=1)     # not 300 - (40+15+40)
            self.assertAlmostEqual(rig.kw[3]["preempt_wait_left_s"], 245, delta=1)     # 300 - (40+15)
            self.assertEqual(rig.slept, [15, 15])                  # the backoff starts again too
            self.assertEqual((run["preempt_wait_exhausted"], run["exit"]), (False, 0))
            self.assertAlmostEqual(run["preempt_wait_s"], 150, delta=1)     # all of the round's waits, on the record

    def test_a_preemption_s_wait_does_not_count_toward_the_awake_ceiling(self):
        """A segment awake 11000 s, 10500 of them waiting after a preemption: 500 s of the ceiling's 10800
        (3 x 60 min) are used, so the session goes on instead of ending on the awake ceiling."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), {**self.T, "preempt_wait_cap_s": 20000})
            run = rig.run([{"waited": 10500, "active_s": 11000}, {"done": True}])
            self.assertEqual(len(rig.kw), 2)                       # the second segment ran
            self.assertEqual(rig.left, [10800, 10300])             # 10800 - (11000 - 10500)
            self.assertEqual((run["timed_out"], run["cap"], run["exit"]), (False, None, 0))

    BAD = {"err": {"type": "provider.internal", "status": 502, "message": "Bad Gateway"}}

    def test_the_segment_that_spent_the_wait_does_not_count_against_max_resumes(self):
        """The give-back after the spent wait is a pause, not an interruption: the resume after it still has
        all max_resumes (3) for endpoint errors."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            rig.run([{"waited": 40, "exhausted": True}])
            run = rig.run([self.BAD, self.BAD, self.BAD, {"done": True}], resume=True)
            self.assertEqual((len(rig.kw), run["exit"]), (5, 0))   # all three errors resumed
            self.assertEqual(run["resumed"], 3)                     # the three errors, not the give-back

    def test_the_resume_after_a_worker_stop_counts_against_max_resumes(self):
        """A resume after a stop of the worker counts the resume it makes itself (run.json's `resumed` counts
        it too): two endpoint errors and a stop have used max_resumes (3), so the next error ends the session
        instead of being resumed a fourth time."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            rig.run([{"waited": 40, "exhausted": True}])          # the give-back: free
            with self.assertRaises(KeyboardInterrupt):
                rig.run([self.BAD, self.BAD, {"waited": 5, "stop": True}], resume=True)
            run = rig.run([self.BAD, {"done": True}], resume=True)
            self.assertEqual((len(rig.kw), run["exit"]), (5, 1))   # the error after the stop is not resumed
            self.assertEqual(run["resumed"], 3)                     # 2 errors and the stop

    def test_a_run_json_from_before_the_preemption_pause_resumes(self):
        """A partial run.json that a harness without the pause wrote (no preemption fields in it or its
        segments) resumes with the pause on: the round's wait cap is whole, its stop counts as an interruption."""
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            t0 = int((time.time() - 700) * 1000)
            (rig.raw / "events.jsonl").write_text(
                json.dumps({"type": "step_start", "timestamp": t0, "sessionID": "ses_r"}, separators=(",", ":")) + "\n"
                + json.dumps({"type": "step_finish", "timestamp": t0 + 1000, "sessionID": "ses_r"}, separators=(",", ":")) + "\n")
            (rig.raw / "run.json").write_text(json.dumps({
                "task": "t", "model": "m", "base_url": "http://127.0.0.1:9/v1", "session": "ses_r",
                "wall_cap_min": 60, "step_cap": 100, "events_start": 0, "first_edit_nudge": None,
                "segments": [{"exit": None, "timed_out": False, "suspend_interrupted": True, "active_s": 600,
                              "wall_s": 600, "suspended_s": 0, "in_progress": True, "open_step_s": 0,
                              "events_offset": 0, "started_at": t0 / 1000}],
                "partial": True}))
            run = rig.run([{"waited": 40}, {"done": True}], resume=True)
            self.assertAlmostEqual(rig.kw[0]["preempt_wait_left_s"], 300, delta=1)
            self.assertEqual(rig.slept, [15])
            self.assertEqual((run["preempt_wait_exhausted"], run["preempted"], run["resumed"], run["exit"]),
                             (False, 1, 1, 0))
            self.assertEqual(len(run["segments"]), 3)

    def test_a_segment_stopped_on_the_wait_cap_ends_the_session(self):
        with tempfile.TemporaryDirectory() as d:
            rig = Rig(Path(d), self.T)
            run = rig.run([{"waited": 40}, {"waited": 40, "exhausted": True}, {"done": True}])
            self.assertTrue(run["preempt_wait_exhausted"])
            self.assertEqual((len(rig.kw), rig.slept), (2, [15]))


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

    def test_the_hold_starts_as_the_preemption_passes_not_when_the_connection_ends(self):
        """The client has the preemption event and sends its retry while the preempted stream's connection is
        still open: the retry is held."""
        slept = []

        async def sleep(d):
            slept.append(d)

        async def main():
            release = asyncio.Event()
            first = [True]

            async def gateway(r, w):
                await r.readuntil(b"\r\n\r\n")
                await r.readexactly(2)
                if first[0]:
                    first[0] = False
                    w.write(self.RESPONSES["sse"])       # the error event, and the connection stays open
                    await w.drain()
                    await release.wait()
                else:
                    w.write(self.RESPONSES["ok"])
                    await w.drain()
                w.close()
            gw = await asyncio.start_server(gateway, "127.0.0.1", 0)
            hold = EF.Hold(sleep=sleep)
            fw = EF.Forwarder("127.0.0.1", gw.sockets[0].getsockname()[1], "http", "/v1", None, hold=hold)
            srv = await asyncio.start_server(fw.handle, "127.0.0.1", 0)
            port = srv.sockets[0].getsockname()[1]
            req = b"POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}"
            r1, w1 = await asyncio.open_connection("127.0.0.1", port)
            w1.write(req)
            await w1.drain()
            await asyncio.wait_for(r1.readuntil(GW_SSE_EVENT), 10)
            r2, w2 = await asyncio.open_connection("127.0.0.1", port)      # the retry, the first still open
            w2.write(req)
            await w2.drain()
            self.assertIn(b"[DONE]", await asyncio.wait_for(r2.read(), 10))
            self.assertEqual(([round(x) for x in slept], hold.counters["preempted"]), ([5], 1))
            release.set()
            await asyncio.wait_for(r1.read(), 10)
            for w in (w1, w2):
                w.close()
            await asyncio.sleep(0.05)
            self.assertEqual(hold.counters["preempted"], 1)      # counted once
            srv.close()
            gw.close()
        asyncio.run(asyncio.wait_for(main(), 30))


if __name__ == "__main__":
    unittest.main()
