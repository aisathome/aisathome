# SPDX-License-Identifier: Apache-2.0
"""worker.Worker against an in-memory queue with the gateway's lease rules, and real child processes (a toy
project, fake_worker_child.py): claim, lease renewal, results, crashes and the pause after three, a lost or
expired lease, a stop that gives the task back with its checkpoint and its resumption (the same worker by id,
or another one), reports held while the gateway is unreachable, a watcher's notice, the busy marker; and
worker.child_main in process."""
from __future__ import annotations

import io
import json
import shutil
import signal
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stderr
from pathlib import Path

from aisathome_harness import worker as W

CHILD = [sys.executable, "-u", str(Path(__file__).resolve().parent / "fake_worker_child.py")]


class FakeQueue:
    """The gateway's lease rules, in memory: a claim takes the first queued task not held for another worker;
    a heartbeat, complete or fail needs the current lease; a give-back (fail with requeue) keeps the try and
    holds the task for its worker (`hold`: None holds it, 0 does not)."""

    base = "fake://queue/v1/projects/toy/tasks"

    def __init__(self):
        self.tasks: dict[str, dict] = {}
        self.calls: list[tuple] = []
        self.lock = threading.Lock()
        self.n = 0
        self.down = 0                    # the next `down` calls raise OSError (the gateway unreachable)

    def add(self, tid: str, spec: dict):
        self.tasks[tid] = {"id": tid, "spec": spec, "state": "queued", "lease": None, "worker": None, "tries": 0,
                           "interruptions": 0, "prefer": None, "result": None, "error": None, "continues": False}

    def _net(self):
        if self.down > 0:
            self.down -= 1
            raise ConnectionRefusedError("gateway down")

    def _lease(self, t, worker, hold):
        self.n += 1
        if not t["continues"]:
            t["tries"] += 1
        t.update(state="leased", lease=f"L{self.n}", worker=worker, prefer=None, continues=False, hold=hold)
        return 200, {"task": {"id": t["id"], "spec": t["spec"], "events": [{"note": f"try {t['tries']}"}]},
                     "lease": {"id": t["lease"]}}

    def claim(self, worker, lease_seconds=600, kinds=None, task=None, max_class=None, hold_seconds=None):
        with self.lock:
            self.calls.append(("claim", worker, task, kinds, hold_seconds))
            self._net()
            for t in self.tasks.values():
                if task and t["id"] != task:
                    continue
                if t["state"] == "queued" and t["prefer"] in (None, worker):
                    return self._lease(t, worker, hold_seconds)
            return (409, {"error": {"message": "not claimable"}}) if task else (204, {})

    def _own(self, tid, worker, lease):
        t = self.tasks.get(tid)
        if t is None:
            return None, (404, {"error": {"message": "no such task"}})
        if t["state"] != "leased" or t["lease"] != lease or t["worker"] != worker:
            return None, (409, {"error": {"message": f"lease lost ({t['state']})"}})
        return t, None

    def heartbeat(self, tid, worker, lease):
        with self.lock:
            self.calls.append(("heartbeat", tid, lease))
            self._net()
            t, err = self._own(tid, worker, lease)
            return err or (200, {"task": {"id": tid}})

    def complete(self, tid, worker, lease, result):
        with self.lock:
            self.calls.append(("complete", tid, lease))
            self._net()
            t, err = self._own(tid, worker, lease)
            if err:
                return err
            t.update(state="done", result=result)
            return 200, {"task": {"id": tid}}

    def fail(self, tid, worker, lease, reason="", requeue=False, fix=None):
        with self.lock:
            self.calls.append(("fail", tid, lease, requeue, reason))
            self._net()
            t, err = self._own(tid, worker, lease)
            if err:
                return err
            if requeue:
                t["interruptions"] += 1
                t.update(state="queued", lease=None, continues=True, prefer=None if t.get("hold") == 0 else worker)
            else:
                t.update(state="failed", error=reason)
            return 200, {"task": {"id": tid}}

    # what happens to a task outside the worker
    def expire(self, tid):
        with self.lock:
            t = self.tasks[tid]
            t.update(state="queued", lease=None, continues=True, prefer=t["worker"])

    def cancel(self, tid):
        with self.lock:
            self.tasks[tid].update(state="cancelled", lease=None)


class Clock:
    """A clock that moves 1 s per reading."""

    def __init__(self):
        self.t = 1000.0

    def monotonic(self):
        self.t += 1
        return self.t

    def time(self):
        return 1_700_000_000.0


def wait_for(cond, timeout=20.0):
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout:
        if cond():
            return True
        time.sleep(0.02)
    return False


class Base(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="wtest-"))
        self.addCleanup(shutil.rmtree, self.root, True)
        self.q = FakeQueue()
        self.log = self.root / "events.log"
        old = (signal.getsignal(signal.SIGTERM), signal.getsignal(signal.SIGINT))
        self.addCleanup(lambda: (signal.signal(signal.SIGTERM, old[0]), signal.signal(signal.SIGINT, old[1])))

    def spec(self, **kw) -> dict:
        return {"log": str(self.log), **kw}

    def worker(self, name="w1", state=None, **kw) -> W.Worker:
        logs: list[str] = []
        w = W.Worker(self.q, name, state or self.root / name, CHILD, poll_s=0.05, report_wait_s=0, stop_wait_s=5,
                     log=logs.append, **kw)
        w.logs = logs
        return w

    def events(self) -> list[str]:
        return self.log.read_text().splitlines() if self.log.exists() else []


class Lifecycle(Base):
    def test_a_task_is_claimed_run_reported_and_its_folders_go(self):
        busy = self.root / "busy" / "w1"
        self.q.add("t1", self.spec(busy=str(busy)))
        w = self.worker(kinds=["other"], hold_s=0, busy_file=busy)
        w.run_forever(once=True)
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["result"]["outcome"]), ("done", "ok"))
        self.assertEqual(t["result"]["busy"], "t1")                 # the marker while the child ran
        self.assertEqual(t["result"]["worker"], "w1")               # the child's spec names the worker
        self.assertFalse(busy.exists())                             # and gone after
        self.assertEqual(self.q.calls[0], ("claim", "w1", None, ["other"], 0))
        self.assertEqual(self.events(), ["cleanup t1"])
        self.assertEqual(w.held(), [])
        self.assertTrue(any("[t1] claimed (try 1)" in l for l in w.logs), w.logs)
        self.assertTrue(any("[t1] done: ok" in l for l in w.logs), w.logs)

    def test_the_lease_is_renewed_while_the_child_runs(self):
        self.q.add("t1", self.spec(sleep=1.5))
        w = self.worker(heartbeat_s=0.2)
        w.run_forever(once=True)
        beats = [c for c in self.q.calls if c[0] == "heartbeat"]
        self.assertGreaterEqual(len(beats), 4, self.q.calls)
        self.assertTrue(all(c[2] == "L1" for c in beats))
        self.assertEqual(self.q.tasks["t1"]["state"], "done")

    def test_an_expired_lease_is_taken_back_and_a_lost_one_stops_the_task(self):
        self.q.add("t1", self.spec(sleep=1.5, running=str(self.root / "t1.running")))
        w = self.worker(heartbeat_s=0.1)
        th = threading.Thread(target=w.run_forever, kwargs={"once": True})
        th.start()
        self.assertTrue(wait_for((self.root / "t1.running").exists))
        self.q.expire("t1")                      # the gateway was unreachable too long: the lease expired
        th.join(30)
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["tries"]), ("done", 1))
        self.assertTrue(any("lease had expired; taken back" in l for l in w.logs), w.logs)
        self.assertIn(("complete", "t1", "L2"), self.q.calls)     # reported with the new lease
        # cancelled while it runs: the child stops, nothing is reported, its folders go
        self.q.add("t2", self.spec(sleep=30, running=str(self.root / "t2.running")))
        th = threading.Thread(target=w.run_forever, kwargs={"once": True})
        t0 = time.monotonic()
        th.start()
        self.assertTrue(wait_for((self.root / "t2.running").exists))
        self.q.cancel("t2")
        th.join(30)
        self.assertLess(time.monotonic() - t0, 20)
        self.assertTrue(any("[t2] lease lost" in l for l in w.logs), w.logs)
        self.assertFalse([c for c in self.q.calls if c[0] in ("complete", "fail") and c[1] == "t2"])
        self.assertIn("cleanup t2", self.events())
        self.assertEqual(w.held(), [])

    def test_a_crash_fails_the_task_and_three_in_a_row_pause_the_worker(self):
        for i in range(4):
            self.q.add(f"t{i}", self.spec(mode="crash"))
        w = self.worker()
        with redirect_stderr(io.StringIO()):
            w.run_forever(once=True)
        self.assertEqual([self.q.tasks[f"t{i}"]["state"] for i in range(4)], ["failed", "failed", "failed", "queued"])
        self.assertIn("crashed: RuntimeError: boom in the task", self.q.tasks["t0"]["error"])
        self.assertIn("3 tasks crashed in a row, the last t2", (w.state / "paused").read_text())
        self.assertIn("paused after 3 crashes", w.paused())
        self.assertEqual(self.events(), ["cleanup t0", "cleanup t1", "cleanup t2"])
        (w.state / "paused").unlink()
        self.q.tasks["t3"]["spec"] = self.spec()
        w.run_forever(once=True)
        self.assertEqual(self.q.tasks["t3"]["state"], "done")

    def test_a_task_that_cannot_start_here_is_given_back_not_failed(self):
        self.q.add("t1", self.spec(mode="give_back"))
        w = self.worker(hold_s=0, checkpoints=True)
        w.run_forever(once=True)
        t = self.q.tasks["t1"]
        # taken again at once (held for nobody), and after three in a row the worker pauses instead of using up
        # the task's interruptions
        self.assertEqual((t["state"], t["tries"], t["interruptions"]), ("queued", 1, 3))
        self.assertIn("its checkpoint could not be restored", (w.state / "paused").read_text())
        self.assertEqual(sum("cannot start here: its checkpoint is unreadable here" in l for l in w.logs), 3, w.logs)
        self.assertNotIn("checkpoint t1", " ".join(self.events()))   # nothing of this host's goes up
        self.assertEqual(w.held(), [])

    def test_a_refused_spec_is_failed_at_once(self):
        self.q.add("t1", {"v": 99})

        def on_claim(task):
            raise W.Refuse("spec version 99 unknown to this worker")
        w = self.worker(on_claim=on_claim)
        w.run_forever(once=True)
        self.assertEqual(self.q.tasks["t1"]["state"], "failed")
        self.assertIn("unknown to this worker", self.q.tasks["t1"]["error"])

    def test_a_result_the_gateway_did_not_take_stays_held_and_goes_later(self):
        self.q.add("t1", self.spec(progress=str(self.root / "t1.progress")))
        w = self.worker(report_tries=2)
        real = self.q.complete

        def down_complete(*a, **k):
            if not getattr(self, "_once", False):
                self._once = True
                self.q.down = 2          # both tries of the report
            return real(*a, **k)
        self.q.complete = down_complete
        w.step()
        self.assertEqual(w.held()[0]["phase"], "unreported")
        self.assertEqual(self.q.tasks["t1"]["state"], "leased")
        w.run_forever(once=True)         # the next step: the child finds it finished, the report goes
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["result"]["resumed"]), ("done", True))
        self.assertEqual(w.held(), [])


class Gate(Base):
    def test_the_gate_holds_new_claims_but_not_a_held_task(self):
        self.q.add("t1", self.spec())
        why = ["1 of 1 model slots busy"]
        w = self.worker(gate=lambda: why[0], claim_lock=self.root / "claim.lock")
        self.assertFalse(w.step())
        self.assertEqual([c for c in self.q.calls if c[0] == "claim"], [])
        self.assertEqual(w.gated, "1 of 1 model slots busy")
        # a task this worker holds (claimed before a stop) resumes whatever the gate says
        w.hold({"task": "t1", "lease": "L0", "spec": {**self.spec(), "task": "t1"}, "phase": "interrupted", "since": 0})
        self.q.tasks["t1"]["worker"] = "w1"
        self.q.expire("t1")                      # held for w1, which takes it back by id
        self.assertTrue(w.step())
        self.assertEqual(self.q.tasks["t1"]["state"], "done")
        # the gate opens: claims again
        self.q.add("t2", self.spec())
        why[0] = None
        self.assertTrue(w.step())
        self.assertEqual(self.q.tasks["t2"]["state"], "done")

    def test_a_broken_gate_claims_as_without_one(self):
        self.q.add("t1", self.spec())

        def gate():
            raise ValueError("stats unreadable")
        w = self.worker(gate=gate)
        self.assertTrue(w.step())
        self.assertEqual(self.q.tasks["t1"]["state"], "done")
        self.assertTrue(any("gate: ValueError" in l for l in w.logs), w.logs)

    def test_the_claim_lock_serialises_gate_and_claim_on_a_host(self):
        import fcntl
        self.q.add("t1", self.spec())
        lock = self.root / "claim.lock"
        seen = []

        def gate():
            with open(lock, "a") as fh:          # another worker of this host cannot take the lock now
                try:
                    fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    seen.append("free")
                except BlockingIOError:
                    seen.append("held")
            return None
        self.worker(gate=gate, claim_lock=lock).step()
        self.assertEqual(seen, ["held"])

    def test_a_gateway_without_hold_seconds_is_said_once(self):
        self.q.add("t1", self.spec())
        self.q.add("t2", self.spec())
        self.q.hold_refused = True
        w = self.worker(hold_s=0)
        w.step()
        w.step()
        self.assertEqual(sum("no hold_seconds" in l for l in w.logs), 1, w.logs)


class Stops(Base):
    def run_until_stopped(self, w: W.Worker, running: Path, how="stop"):
        th = threading.Thread(target=w.run_forever)
        th.start()
        self.assertTrue(wait_for(running.exists))
        time.sleep(0.2)
        (w.stop if how == "stop" else (lambda: w.notice("instance-action: terminate")))()
        th.join(30)
        self.assertFalse(th.is_alive())

    def test_a_stop_gives_the_task_back_with_its_checkpoint_and_the_next_start_resumes_it(self):
        self.q.add("t1", self.spec(sleep=60, running=str(self.root / "t1.running"),
                                   progress=str(self.root / "t1.progress")))
        w = self.worker(checkpoints=True)
        self.run_until_stopped(w, self.root / "t1.running")
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["tries"], t["interruptions"], t["prefer"]), ("queued", 1, 1, "w1"))
        gave = [c for c in self.q.calls if c[0] == "fail"]
        self.assertEqual(len(gave), 1)
        self.assertTrue(gave[0][3])                                    # requeue: the try is kept
        self.assertEqual(gave[0][4], W.GIVE_BACK_REASON)
        self.assertIn("checkpoint t1 give-back by w1", self.events())  # before the give-back
        self.assertEqual(w.held()[0]["phase"], "interrupted")
        self.assertTrue(any("[t1] given back (HTTP 200)" in l for l in w.logs), w.logs)
        # the next start: the same worker takes it back by id and resumes it
        w2 = self.worker()
        w2.run_forever(once=True)
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["result"]["resumed"], t["tries"]), ("done", True, 1))
        self.assertIn(("claim", "w1", "t1", None, None), self.q.calls)
        self.assertTrue(any("[t1] taken back" in l for l in w2.logs), w2.logs)
        self.assertEqual(w2.held(), [])

    def test_with_hold_zero_another_worker_resumes_the_given_back_task(self):
        self.q.add("t1", self.spec(sleep=60, running=str(self.root / "t1.running"),
                                   progress=str(self.root / "t1.progress")))
        self.run_until_stopped(self.worker(hold_s=0, checkpoints=True), self.root / "t1.running")
        self.assertIsNone(self.q.tasks["t1"]["prefer"])
        other = self.worker("w2", hold_s=0)
        other.run_forever(once=True)
        t = self.q.tasks["t1"]
        self.assertEqual((t["state"], t["result"]["resumed"], t["result"]["worker"], t["tries"]), ("done", True, "w2", 1))

    def test_a_watchers_notice_takes_the_stop_path(self):
        class Watch:
            def __init__(self, on):
                self.on, self.stopped = on, False

            def start(self):
                pass

            def stop(self):
                self.stopped = True
        made = []
        self.q.add("t1", self.spec(sleep=60, running=str(self.root / "t1.running")))
        w = self.worker(watchers=(lambda on: made.append(Watch(on)) or made[-1],), clock=Clock())
        th = threading.Thread(target=w.run_forever)
        th.start()
        self.assertTrue(wait_for((self.root / "t1.running").exists))
        made[0].on("instance-action: terminate")
        th.join(30)
        self.assertEqual(self.q.tasks["t1"]["state"], "queued")
        self.assertTrue(made[0].stopped)
        self.assertTrue(any("given back (HTTP 200, 1.0 s after the spot notice)" in l for l in w.logs), w.logs)


class ChildMain(unittest.TestCase):
    def setUp(self):
        self.d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.d, True)
        (self.d / "spec.json").write_text(json.dumps({"task": "t1"}))
        self.args = ["run-task", str(self.d / "spec.json"), str(self.d / "out.json"), str(self.d / "err.json")]

    def err(self) -> dict:
        return json.loads((self.d / "err.json").read_text())

    def test_results_crashes_and_give_backs(self):
        self.assertEqual(W.child_main(self.args, run=lambda s: {"outcome": "ok", "task": s["task"]}), 0)
        self.assertEqual(json.loads((self.d / "out.json").read_text()), {"outcome": "ok", "task": "t1"})

        def boom(s):
            raise PermissionError(13, "Permission denied", "/w/x")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(W.child_main(self.args, run=boom), W.CRASH)
        self.assertEqual(self.err(), {"error": "PermissionError: [Errno 13] Permission denied: '/w/x'"})

        def ex(s):
            raise SystemExit("no env")
        with redirect_stderr(io.StringIO()):
            self.assertEqual(W.child_main(self.args, run=ex), W.CRASH)
        self.assertEqual(self.err(), {"error": "exit: no env"})

        def gb(s):
            raise W.GiveBack("unreadable")
        self.assertEqual(W.child_main(self.args, run=gb), W.GIVE_BACK)
        self.assertEqual(self.err(), {"error": "unreadable", "give_back": True})

    def test_the_tasks_lock(self):
        class Lock:
            def __init__(self):
                self.entered = 0

            def __enter__(self):
                self.entered += 1

            def __exit__(self, *a):
                return False
        lk = Lock()
        self.assertEqual(W.child_main(self.args, run=lambda s: {}, lock=lambda tid, wait_s: lk), 0)
        self.assertEqual(lk.entered, 1)
        with redirect_stderr(io.StringIO()):
            self.assertEqual(W.child_main(["cleanup-task", self.args[1]], run=lambda s: {}, cleanup=lambda s: 1 / 0,
                                          lock=lambda tid, wait_s: None), 0)          # held elsewhere: nothing removed
            self.assertEqual(W.child_main(self.args, run=lambda s: {}, lock=lambda tid, wait_s: None), W.CRASH)
            self.assertEqual(W.child_main(["nonsense"], run=lambda s: {}), 2)
        self.assertIn("lock", self.err()["error"])


if __name__ == "__main__":
    unittest.main()
