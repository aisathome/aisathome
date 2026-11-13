# SPDX-License-Identifier: Apache-2.0
"""The fix-round engine with stand-in sessions and checks: proved, fixed, out of rounds, not fixable,
infrastructure retried once, other unscored kinds, a follow-up without tool calls, and resuming the
engine itself after a stop."""
from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from aisathome_harness import evidence, rounds

# opencode v2's shape for an endpoint's 503 (from a recorded session; the message is an aisathome
# gateway's when no server of the model is connected)
ERR_503 = ('{"type":"error","timestamp":1,"sessionID":"ses_1","error":{"type":"provider.internal","message":'
           '"no node serving \\"m\\" is online right now","status":503,"response":{"body":"{}"}}}\n')
REFUSED = '{"type":"error","timestamp":1,"error":{"message":"request refused by policy"}}\n'


class Rig:
    """A task's evidence directory and a scripted project: each session appends `steps` step events and
    writes a finished run.json; each check pops the next outcome."""

    def __init__(self, raw: Path, outcomes: list, sessions: list | None = None):
        self.raw, self.outcomes, self.calls = raw, list(outcomes), []
        self.sessions = list(sessions or [])
        self.round = 0

    def _session(self, kind: str, msg: str | None = None, followup: bool = False):
        self.calls.append((kind, msg))
        spec = self.sessions.pop(0) if self.sessions else {}
        ev = self.raw / "events.jsonl"
        start = ev.stat().st_size if ev.exists() else 0
        with open(ev, "a") as f:
            f.write('{"type":"step_start","timestamp":1}\n{"type":"step_finish","timestamp":2}\n' * spec.get("steps", 6))
            f.write(spec.get("tail", ""))
        prev = evidence.read_run(self.raw) or {}
        fu = (prev.get("followup_round") or 0) + 1 if followup else prev.get("followup_round")
        rm = {"session": "ses_1", "finished_at": f"t{len(self.calls)}", "events_start": start if followup or not prev else prev.get("events_start", 0),
              "steps": spec.get("steps", 6), "round_steps": spec.get("steps", 6), **({"followup_round": fu} if fu else {})}
        if spec.get("partial"):
            rm = {"session": "ses_1", "partial": True}
        (self.raw / "run.json").write_text(json.dumps(rm))

    def check(self, rm, n):
        out = self.outcomes.pop(0)
        if isinstance(out, dict):
            return {"round": n, **out}
        rec = {"round": n, "outcome": out}
        rec.update(rounds.classify_round(self.raw, rm, out, refusal="refused by policy", refused_kind="refused"))
        return rec

    def make(self, **kw) -> rounds.Rounds:
        return rounds.Rounds(raw=self.raw, fix_rounds=kw.pop("fix_rounds", 2),
                             start=lambda: self._session("start"), resume=lambda: self._session("resume"),
                             followup=lambda m: self._session("followup", m, True), check=self.check,
                             feedback=kw.pop("feedback", lambda rec, n, total: None if rec["outcome"] == "axioms" else f"fix {rec['outcome']} {n}/{total}"),
                             wait=lambda probe: True, label="t", **kw)


class Engine(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.raw = Path(self.d.name)

    def tearDown(self):
        self.d.cleanup()

    def test_proved_in_round_one(self):
        rig = Rig(self.raw, ["proved"])
        st = rounds.run_rounds(rig.make())
        self.assertEqual(st["final"]["outcome"], "proved")
        self.assertEqual((st["final"]["scored"], st["final"]["proved_in_round"]), (True, 1))
        self.assertEqual([c[0] for c in rig.calls], ["start"])

    def test_fixed_in_round_two(self):
        rig = Rig(self.raw, ["compile_error", "proved"])
        st = rounds.run_rounds(rig.make())
        self.assertEqual(st["final"]["proved_in_round"], 2)
        self.assertEqual(rig.calls, [("start", None), ("followup", "fix compile_error 1/3")])
        self.assertEqual([r["round"] for r in st["rounds"]], [1, 2])

    def test_out_of_rounds(self):
        rig = Rig(self.raw, ["compile_error"] * 3)
        st = rounds.run_rounds(rig.make())
        self.assertEqual(st["final"], {**st["final"], "outcome": "compile_error", "scored": True, "rounds": 3, "proved": False})
        self.assertEqual([c[0] for c in rig.calls], ["start", "followup", "followup"])

    def test_not_fixable_stops(self):
        rig = Rig(self.raw, ["axioms"])
        st = rounds.run_rounds(rig.make())
        self.assertEqual((st["final"]["outcome"], st["final"]["rounds"]), ("axioms", 1))
        self.assertEqual(len(rig.calls), 1)

    def test_a_followup_without_tool_calls_twice_stops_not_scored(self):
        none = {"outcome": "compile_error", "unscored": None, "no_tool_calls": True}
        rig = Rig(self.raw, ["compile_error", none, none])
        r = rig.make()
        r.retry = lambda: rig._session("retry")
        st = rounds.run_rounds(r)
        self.assertEqual((st["final"]["outcome"], st["final"]["rounds"], st["final"]["scored"], st["final"]["unscored"]),
                         ("compile_error", 2, False, "no_tool_calls"))
        self.assertEqual([c[0] for c in rig.calls], ["start", "followup", "retry"])

    def test_a_followup_without_tool_calls_runs_again_and_is_never_judged(self):
        """Trial 4's C12: round 2's reply was cut after 98 characters with no tool call. That round runs again
        once with its own caps; the feedback (a project's no-progress rule) never sees it."""
        none = {"outcome": "refused", "unscored": None, "no_tool_calls": True}
        seen = []
        rig = Rig(self.raw, ["refused", none, "refused", "proved"])
        r = rig.make(feedback=lambda rec, n, total: seen.append((n, rec.get("no_tool_calls"))) or f"fix {n}")
        r.retry = lambda: rig._session("retry")
        st = rounds.run_rounds(r)
        self.assertEqual([c[0] for c in rig.calls], ["start", "followup", "retry", "followup"])
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"], st["final"]["proved_in_round"]), ("proved", True, 3))
        self.assertEqual(seen, [(1, None), (2, None)])
        self.assertTrue(st["rounds"][1]["retried"])

    def test_a_followup_without_tool_calls_is_not_retried_without_infra_retry(self):
        rig = Rig(self.raw, ["compile_error", {"outcome": "compile_error", "unscored": None, "no_tool_calls": True}])
        st = rounds.run_rounds(rig.make(infra_retry=False))
        self.assertEqual((st["final"]["scored"], st["final"]["unscored"]), (False, "no_tool_calls"))
        self.assertEqual(len(rig.calls), 2)

    def test_infra_in_round_one_is_retried_once_from_scratch(self):
        rig = Rig(self.raw, ["compile_error", "proved"], sessions=[{"steps": 2, "tail": ERR_503}, {"steps": 6}])
        st = rounds.run_rounds(rig.make())
        self.assertEqual(st["final"]["outcome"], "proved")
        self.assertEqual([c[0] for c in rig.calls], ["start", "start"])
        self.assertEqual(st["try"], 2)
        self.assertTrue((self.raw / "infra-try1" / "events.jsonl").exists())
        first = st["rounds"][0]
        self.assertEqual((first["unscored"], first["retried"], first["gateway_errors"]), ("infra", True, 1))

    def test_infra_twice_ends_not_scored(self):
        bad = {"steps": 2, "tail": ERR_503}
        rig = Rig(self.raw, ["compile_error", "compile_error"], sessions=[bad, bad])
        st = rounds.run_rounds(rig.make())
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("infra", False))
        self.assertEqual(len(rig.calls), 2)

    def test_infra_without_retry_ends_at_once(self):
        rig = Rig(self.raw, ["compile_error"], sessions=[{"steps": 2, "tail": ERR_503}])
        st = rounds.run_rounds(rig.make(infra_retry=False))
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("infra", False))

    def test_a_round_with_steps_is_scored_despite_errors(self):
        rig = Rig(self.raw, ["compile_error"], sessions=[{"steps": rounds.INFRA_MAX_STEPS, "tail": ERR_503}])
        st = rounds.run_rounds(rig.make(fix_rounds=0))
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("compile_error", True))

    def test_infra_in_a_later_round_is_retried_with_its_own_caps(self):
        rig = Rig(self.raw, ["compile_error", "compile_error", "proved"],
                  sessions=[{"steps": 6}, {"steps": 1, "tail": ERR_503}, {"steps": 6}])
        r = rig.make()
        r.retry = lambda: rig._session("retry")
        st = rounds.run_rounds(r)
        self.assertEqual([c[0] for c in rig.calls], ["start", "followup", "retry"])
        self.assertEqual(st["final"]["outcome"], "proved")

    def test_an_endpoint_that_stays_down_ends_infra(self):
        rig = Rig(self.raw, ["compile_error"], sessions=[{"steps": 1, "tail": ERR_503}])
        r = rig.make(probe=lambda: False)
        r.wait = lambda probe: False
        st = rounds.run_rounds(r)
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("infra", False))

    def test_other_unscored_kinds_end_without_retry(self):
        rig = Rig(self.raw, ["compile_error"], sessions=[{"steps": 9, "tail": REFUSED}])
        st = rounds.run_rounds(rig.make())
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("refused", False))
        self.assertEqual(st["rounds"][0]["refusals"], 1)

    def test_proved_is_scored_whatever_the_errors(self):
        rig = Rig(self.raw, ["proved"], sessions=[{"steps": 1, "tail": ERR_503 + REFUSED}])
        st = rounds.run_rounds(rig.make())
        self.assertEqual((st["final"]["outcome"], st["final"]["scored"]), ("proved", True))

    def test_a_finished_task_is_left_alone(self):
        rig = Rig(self.raw, ["proved"])
        rounds.run_rounds(rig.make())
        rig2 = Rig(self.raw, [])
        st = rounds.run_rounds(rig2.make())
        self.assertEqual(rig2.calls, [])
        self.assertEqual(st["final"]["outcome"], "proved")

    def test_an_interrupted_session_is_resumed_then_checked_once(self):
        rig = Rig(self.raw, ["proved"], sessions=[{"partial": True}, {"steps": 6}])
        st = rounds.run_rounds(rig.make())
        self.assertEqual([c[0] for c in rig.calls], ["start", "resume"])
        self.assertEqual(len(st["rounds"]), 1)

    def test_a_record_is_reused_after_a_stop(self):
        """A stop after the check was recorded: the next call does not check the same run again."""
        rig = Rig(self.raw, ["compile_error", "proved"])
        r = rig.make()
        r.followup = lambda m: (_ for _ in ()).throw(KeyboardInterrupt())    # the stop, before round 2 runs
        with self.assertRaises(KeyboardInterrupt):
            rounds.run_rounds(r)
        self.assertEqual(len(evidence.load_rounds(self.raw)["rounds"]), 1)
        rig.calls.clear()
        st = rounds.run_rounds(rig.make())
        self.assertEqual(rig.calls, [("followup", "fix compile_error 1/3")])
        self.assertEqual([x["outcome"] for x in st["rounds"]], ["compile_error", "proved"])


class InfraShapes(unittest.TestCase):
    """The HTTP status of an endpoint error, in both shapes opencode writes."""

    def classify(self, err: dict) -> str | None:
        with tempfile.TemporaryDirectory() as d:
            raw = Path(d)
            (raw / "events.jsonl").write_text(json.dumps({"type": "error", "timestamp": 1, "error": err}) + "\n")
            return rounds.classify_round(raw, {"round_steps": 1}, "no_solution")["unscored"]

    def test_status_and_status_code(self):
        self.assertEqual(self.classify({"name": "APIError", "message": "busy", "status": 503}), "infra")
        self.assertEqual(self.classify({"name": "APIError", "data": {"message": "busy", "statusCode": 502}}), "infra")
        self.assertEqual(self.classify({"name": "APIError", "data": {"message": "Bad Gateway", "statusCode": 504}}), "infra")

    def test_other_errors_are_the_model_s(self):
        self.assertIsNone(self.classify({"name": "APIError", "data": {"message": "bad request", "statusCode": 400}}))
        self.assertIsNone(self.classify({"name": "ToolError", "data": {"message": "line 503: unknown"}}))
        self.assertIsNone(self.classify({"name": "APIError", "data": {"message": "x", "statusCode": 5031}}))


class Probe(unittest.TestCase):
    def test_wait_for_gateway(self):
        seq = iter([False, False, True])
        slept = []
        self.assertTrue(rounds.wait_for_gateway(lambda: next(seq), poll_s=10, max_wait_s=100, sleep=slept.append))
        self.assertEqual(slept, [10, 10])
        self.assertFalse(rounds.wait_for_gateway(lambda: False, poll_s=10, max_wait_s=5, sleep=slept.append))

    def test_a_closed_port_is_down(self):
        self.assertFalse(rounds.gateway_probe("http://127.0.0.1:9/v1", None, "m", timeout=2))


if __name__ == "__main__":
    unittest.main()
