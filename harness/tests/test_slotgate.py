# SPDX-License-Identifier: Apache-2.0
"""slotgate: the slot count (leased and held model tasks per model slot), no slots online (one model task for the
demand), the idle release (start-up, calls in
flight, the idle window, the ceiling), the memory gate, the pick in the gateway's claim order (held for another
worker, skipped, not runnable here, tasks without the model while the slots are full), Picking's claim by id, and
spare_capacity. In process, against a fake task list and fake stats; the clock: 2026-11-14 20:00:00 UTC."""
from __future__ import annotations

import datetime as dt
import http.client
import tempfile
import unittest
from pathlib import Path

from aisathome_harness import slotgate as G

NOW = dt.datetime(2026, 11, 14, 20, 0, tzinfo=dt.UTC).timestamp()
KINDS = ["model-v1", "model-v2"]


def ago(s: float) -> str:
    return dt.datetime.fromtimestamp(NOW - s, dt.UTC).isoformat().replace("+00:00", "Z")


class Clock:
    def __init__(self, t: float = NOW):
        self.t = t

    def time(self):
        return self.t


class FakeList:
    gateway = "http://gw"

    def __init__(self, tasks, code=200):
        self.tasks, self.code = tasks, code

    def list(self):
        return self.code, {"tasks": self.tasks}


def uses_model(t: dict) -> bool:
    """The test project's: a `check` task (its own check, no model) and a `submit` kind take no slot."""
    return t.get("kind") != "submit" and t.get("role") != "check"


def runnable(t: dict) -> str | None:
    rel = (t.get("labels") or {}).get("release")
    return f"release {rel} is not this one" if rel and rel != "r1" else None


def task(tid, state="queued", role="work", order=1, seq=1, kind="model-v1", release="r1", **kw):
    return {"id": tid, "state": state, "role": role, "order": order, "seq": seq, "kind": kind,
            "labels": {"release": release}, **kw}


def leased(tid, live=None, since="2026-11-14T19:30:00Z", role="work"):
    t = {"id": tid, "state": "leased", "role": role, "kind": "model-v1", "lease": {"since": since}}
    if live is not None:
        t["live"] = live
    return t


def gate(tasks, slots=1, per_slot=1, code=200, **kw):
    kw.setdefault("uses_model", uses_model)
    kw.setdefault("log", lambda msg: None)
    return G.SlotGate(FakeList(tasks, code), per_slot, stats=lambda: {"nodes": {"slots": slots}}, clock=Clock(), **kw)


class Slots(unittest.TestCase):
    def why(self, tasks, slots, per_slot=1, code=200):
        return gate(tasks, slots, per_slot, code)()

    def test_one_task_per_slot(self):
        busy = {"id": "a", "state": "leased"}
        self.assertIsNone(self.why([], 1))
        self.assertEqual(self.why([busy], 1), "1 task(s) running for 1 model slot(s) online (1 per slot)")
        self.assertIsNone(self.why([busy], 2))
        self.assertIsNone(self.why([busy], 1, per_slot=2))
        self.assertEqual(self.why([], 0), "no model slots online (1 per slot)")
        self.assertIsNone(self.why([{"id": "d", "state": "done"}, {"id": "q", "state": "queued"}], 1))

    def test_a_task_held_for_its_worker_counts_until_its_hold_ends(self):
        held = lambda until: {"id": "b", "state": "queued", "prefer_worker": "r-1", "prefer_until": until}
        self.assertIsNotNone(self.why([held("2026-11-14T20:05:00.123456789Z")], 1))
        self.assertIsNotNone(self.why([held("2026-11-14T21:05:00+01:00")], 1))
        self.assertIsNone(self.why([held("2026-11-14T19:59:59Z")], 1))
        self.assertIsNone(self.why([held("2026-11-14T20:30:00+01:00")], 1))

    def test_tasks_without_the_model_take_no_slot_and_are_picked_while_the_slots_are_full(self):
        busy = [task("a", "leased", seq=1), task("b", "leased", seq=2)]
        chk = task("c-check", role="check", order=1, seq=9)
        g = gate(busy + [task("d", seq=3), chk], slots=2)
        self.assertEqual((g(), g.pick), (None, "c-check"))
        g = gate(busy + [task("d", seq=3)], slots=2)
        self.assertEqual((g(), g.pick), ("2 task(s) running for 2 model slot(s) online (1 per slot)", None))
        # a leased check or submit task does not fill a model slot
        g = gate([task("a", "leased"), task("x-check", "leased", role="check"),
                  task("x-submit", "leased", kind="submit"), task("d", seq=3)], slots=2)
        self.assertEqual((g(), g.pick), (None, "d"))
        # without uses_model every task takes a slot
        g = gate([task("a", "leased"), task("x-check", "leased", role="check"), task("d", seq=3)], slots=2,
                 uses_model=None)
        self.assertEqual(g(), "2 task(s) running for 2 model slot(s) online (1 per slot)")

    def test_an_unreadable_queue_raises_for_the_worker_to_log_and_per_slot_is_checked(self):
        with self.assertRaises(OSError):
            gate([], code=503)()
        with self.assertRaises(ValueError):
            G.SlotGate(FakeList([]), 0)

    def test_a_stats_outage_lifts_the_slot_limit_not_the_memory_gate(self):
        def down():
            raise OSError("no route")
        said = []
        g = G.SlotGate(FakeList([task("a", "leased"), task("b", seq=2)]), 1, stats=down, clock=Clock(), worker="r-1",
                       kinds=KINDS, memory=lambda: "memory: full", log=said.append)
        self.assertEqual((g(), g.pick), ("memory: full", None))
        g.memory = None
        self.assertEqual((g(), g.pick), (None, "b"))
        self.assertEqual(said, ["gate: /v1/stats unreadable (OSError); no slot limit this time"] * 2)

    def test_a_truncated_stats_reply_is_an_outage(self):
        def cut():
            raise http.client.IncompleteRead(b'{"nodes": {"slo')
        said = []
        g = G.SlotGate(FakeList([task("a", "leased"), task("b", seq=2)]), 1, stats=cut, clock=Clock(),
                       memory=lambda: "memory: full", log=said.append)
        self.assertEqual((g(), g.pick), ("memory: full", None))
        g.memory = None
        self.assertEqual((g(), g.pick), (None, "b"))
        self.assertEqual(said, ["gate: /v1/stats unreadable (IncompleteRead); no slot limit this time"] * 2)
        self.assertIsNone(G.spare_capacity(FakeList([]), cut))

    def test_no_slot_limit_still_picks_and_an_empty_queue_says_nothing(self):
        g = gate([], per_slot=None)
        self.assertEqual((g(), g.pick), (None, None))
        g = gate([task("a", release="r9", seq=1), task("b", seq=2)], per_slot=None, slots=0, runnable=runnable)
        self.assertEqual((g(), g.pick), (None, "b"))

    def test_the_default_stats_read_the_gateways_public_stats(self):
        seen = []
        orig = G.get_json
        G.get_json = lambda url, **kw: seen.append(url) or {"nodes": {"slots": 1}}
        self.addCleanup(setattr, G, "get_json", orig)
        self.assertIsNone(G.SlotGate(FakeList([]), 1, clock=Clock())())
        self.assertEqual(seen, ["http://gw/v1/stats"])


class NoSlots(unittest.TestCase):
    """No model slots online: one model task may be claimed, so the gateway sees a worker and asks for a pilot."""
    NONE = "no model slots online (1 per slot)"

    def test_one_model_task_is_claimed_then_the_gate_waits(self):
        said = []
        g = gate([task("a", seq=1), task("b", seq=2)], slots=0, log=said.append)
        self.assertEqual((g(), g.pick), (None, "a"))
        self.assertEqual(said, ["gate: no model slots online; claiming a model task (a) so the gateway sees the demand "
                                "for one"])
        g = gate([task("a", "leased", seq=1), task("b", seq=2)], slots=0)
        self.assertEqual((g(), g.pick), (self.NONE, None))
        # with idle release alike, an idle lease included
        g = gate([task("b", seq=2)], slots=0, idle_s=20)
        self.assertEqual((g(), g.pick), (None, "b"))
        g = gate([leased("a", {"in_flight": 0, "updated_at": ago(600)}), task("b", seq=2)], slots=0, idle_s=20)
        self.assertEqual(g(), self.NONE + "; 1 more leased, idle")

    def test_a_task_held_for_its_worker_counts(self):
        held = task("h", seq=1, prefer_worker="r-2", prefer_until=ago(-300))
        g = gate([held, task("b", seq=2)], slots=0, worker="r-1")
        self.assertEqual((g(), g.pick), (self.NONE, None))
        g = gate([held, task("b", seq=2)], slots=0, worker="r-2")      # r-2 resumes its own
        self.assertEqual((g(), g.pick), (self.NONE, None))

    def test_bootstrap_sets_how_many_and_0_is_none(self):
        g = gate([task("a")], slots=0, bootstrap=0)
        self.assertEqual((g(), g.pick), (self.NONE, None))
        g = gate([task("x", "leased"), task("a", seq=2)], slots=0, bootstrap=2)
        self.assertEqual((g(), g.pick), (None, "a"))
        for bad in (-1, 1.5, None):
            with self.assertRaises(ValueError):
                G.SlotGate(FakeList([]), 1, bootstrap=bad)

    def test_the_memory_gate_and_the_pick_still_apply(self):
        no = lambda: "memory: full"
        g = gate([task("a")], slots=0, memory=no)
        self.assertEqual((g(), g.pick), ("memory: full", None))
        g = gate([task("a", seq=1), task("a-check", role="check", seq=2)], slots=0, memory=no)
        self.assertEqual((g(), g.pick), (None, "a-check"))
        g = gate([task("a", release="r2")], slots=0, runnable=runnable)
        self.assertEqual((g(), g.pick), (self.NONE, None))
        g = gate([], slots=0)                                          # nothing queued: the reason as before
        self.assertEqual((g(), g.pick), (self.NONE, None))

    def test_slots_online_are_not_bootstrapped(self):
        g = gate([task("x", "leased"), task("a", seq=2)], slots=1, bootstrap=5)
        self.assertEqual((g(), g.pick), ("1 task(s) running for 1 model slot(s) online (1 per slot)", None))
        idle = lambda tid: leased(tid, {"in_flight": 0, "updated_at": ago(600)})
        g = gate([idle("x"), idle("y"), task("a", seq=2)], slots=1, idle_s=20, bootstrap=5)
        self.assertEqual(g(), "2 model task(s) leased (2 idle) for 1 model slot(s) online (at most 2 per slot)")

    def test_unreadable_stats_are_not_no_slots(self):
        def down():
            raise OSError("no route")
        g = G.SlotGate(FakeList([task("x", "leased"), task("a", seq=2)]), 1, stats=down, clock=Clock(), log=[].append)
        self.assertEqual((g(), g.pick), (None, "a"))                   # no slot limit, as before


class Pick(unittest.TestCase):
    def test_a_task_this_release_cannot_run_is_passed_over(self):
        newer = task("x", order=0, seq=1, release="r2", requeued=True)
        g = gate([newer, task("y", order=10001, seq=2)], slots=2, runnable=runnable, kinds=KINDS)
        self.assertEqual((g(), g.pick), (None, "y"))
        g = gate([newer], slots=2, runnable=runnable, kinds=KINDS)
        why = "1 queued task(s), none this release can run (e.g. x: release r2 is not this one)"
        self.assertEqual((g(), g.pick), (why, None))

    def test_skipped_tasks_are_left_alone_for_a_while(self):
        g = gate([task("a", seq=1), task("b", seq=2)], slots=2, worker="r-1", kinds=KINDS)
        g.skip["a"] = NOW + 10
        self.assertEqual((g(), g.pick), (None, "b"))
        g.clock.t = NOW + 11
        self.assertEqual((g(), g.pick), (None, "a"))
        self.assertEqual(g.skip, {})
        g = gate([task("a", seq=1)], slots=2)
        g.skip["a"] = NOW + 10
        self.assertEqual((g(), g.pick), ("1 queued task(s), none this release can run (e.g. a: skipped here)", None))

    def test_the_gateways_claim_order_and_holds(self):
        later = ago(-300)
        q = [task("a", order=10001, seq=5), task("b", order=1, seq=9),
             task("c", order=10001, seq=1, requeued=True, prefer_worker="r-2", prefer_until=later)]
        g = gate(q, slots=2, worker="r-1", kinds=KINDS)  # c is held for r-2: for r-1, b (order 1) is first
        self.assertEqual((g(), g.pick), (None, "b"))
        g = gate(q, slots=2, worker="r-2", kinds=KINDS)  # r-2 gets its own requeued task first
        self.assertEqual((g(), g.pick), (None, "c"))
        g = gate([q[0], task("z", order=10001, seq=7, kind="other-project")], slots=2, kinds=KINDS)
        self.assertEqual((g(), g.pick), (None, "a"))

    def test_the_memory_gate_holds_model_tasks_only(self):
        no = lambda: "memory: full"
        g = gate([task("a")], slots=2, memory=no)
        self.assertEqual((g(), g.pick), ("memory: full", None))
        g = gate([task("a", seq=1), task("a-check", role="check", order=10000, seq=2)], slots=2, memory=no)
        self.assertEqual((g(), g.pick), (None, "a-check"))
        g = gate([task("a")], slots=2, memory=lambda: None)
        self.assertEqual((g(), g.pick), (None, "a"))

    def test_picking_claims_the_pick_by_id_once(self):
        calls = []

        class C(FakeList):
            def claim(self, worker, lease_seconds=600, kinds=None, task=None, **kw):
                calls.append((task, kinds, kw))
                return 204, None
        g = G.SlotGate(C([]), 1, stats=lambda: {"nodes": {"slots": 1}}, clock=Clock())
        c = G.Picking(C([]), g)
        g.pick = "x-check"
        c.claim("r-1", 600, kinds=["model-v1"], hold_seconds=0)
        c.claim("r-1", 600, kinds=["model-v1"])
        self.assertEqual(calls, [("x-check", ["model-v1"], {"hold_seconds": 0}), (None, ["model-v1"], {})])
        self.assertEqual(c.gateway, "http://gw")


QUEUED = {"id": "h1", "state": "queued", "role": "work", "kind": "model-v1", "seq": 2}


class IdleRelease(unittest.TestCase):
    QUEUED = QUEUED

    def idle_gate(self, tasks, slots=1, per_slot=1, idle_s=20, max_per_slot=2):
        g = gate(tasks, slots, per_slot, idle_s=idle_s, max_per_slot=max_per_slot)
        return g(), g.pick

    def test_a_leased_task_holds_its_slot_only_while_it_uses_the_model(self):
        live = lambda s, n=0: {"requests": 3, "in_flight": n, "updated_at": ago(s)}
        holds = lambda t, idle_s=20, known=True: G.holds_slot(t, NOW, idle_s, known, 300)
        self.assertTrue(holds(leased("start-up", since="2026-11-14T19:58:00Z")))        # no request ended yet
        self.assertFalse(holds(leased("start-up", since="2026-11-14T19:50:00Z")))       # 10 min, no request: idle
        self.assertTrue(holds(leased("start-up", since="2026-11-14T19:50:00Z"), known=False))   # old gateway
        self.assertFalse(holds(leased("no request")))                                   # leased 30 min: past start-up
        self.assertTrue(holds({"id": "no since", "state": "leased"}))                    # no lease time: starting
        self.assertTrue(holds(leased("generating", live(600, n=1))))                     # a long generation in flight
        self.assertTrue(holds(leased("first in flight", {"requests": 0, "in_flight": 1})))
        self.assertTrue(holds(leased("between steps", live(19))))
        self.assertFalse(holds(leased("its own checks", live(21))))
        self.assertFalse(holds(leased("its own checks", live(600))))
        self.assertTrue(holds(leased("old gateway", {k: v for k, v in live(600).items() if k != "in_flight"})))
        self.assertTrue(holds(leased("odd", dict(live(600), in_flight="x"))))
        self.assertTrue(holds(leased("no end yet", {"requests": 0, "in_flight": 0}, since="2026-11-14T19:58:00Z")))
        # A continued try: its last request was in the lease before; this lease is starting up, for startup_s at most.
        self.assertTrue(holds(leased("resumed", live(600), since="2026-11-14T19:58:00Z")))
        self.assertFalse(holds(leased("resumed", live(1500), since="2026-11-14T19:52:00Z")))
        self.assertTrue(holds(leased("resumed", dict(live(1500), in_flight=1), since="2026-11-14T19:52:00Z")))
        self.assertTrue(G.holds_slot(leased("resumed", live(1500), since="2026-11-14T19:52:00Z"), NOW, 20, True, 600))
        self.assertTrue(holds(leased("idle", live(600)), idle_s=None))                   # off: always
        self.assertTrue(holds(leased("future", live(-5))))                               # the gateway's clock ahead

    def test_inflight_known(self):
        self.assertTrue(G.inflight_known([leased("a"), leased("b", {"requests": 1, "in_flight": 0})]))
        self.assertFalse(G.inflight_known([leased("a"), leased("b", {"requests": 1})]))
        self.assertFalse(G.inflight_known([{"id": "d", "state": "done", "live": {"in_flight": 0}}]))

    def test_claims_beside_a_resumed_task_past_its_startup(self):
        resumed = leased("o1", {"requests": 49, "in_flight": 0, "updated_at": ago(1200)}, since="2026-11-14T19:51:00Z")
        self.assertEqual(self.idle_gate([resumed, self.QUEUED]), (None, "h1"))
        g = gate([resumed, self.QUEUED], idle_s=20, startup_s=900)
        self.assertEqual(g(), "1 task(s) running for 1 model slot(s) online (1 per slot)")

    def test_claims_beside_a_task_with_no_model_request_for_a_while(self):
        quiet = leased("o1", {"requests": 40, "in_flight": 0, "updated_at": ago(600)})
        self.assertEqual(self.idle_gate([quiet, self.QUEUED]), (None, "h1"))
        # Off: the leased task holds the slot whatever it does.
        self.assertEqual(self.idle_gate([quiet, self.QUEUED], idle_s=None)[0],
                         "1 task(s) running for 1 model slot(s) online (1 per slot)")
        # A gateway without live.in_flight: every leased task counts.
        old = leased("o1", {"requests": 40, "updated_at": ago(600)})
        self.assertEqual(self.idle_gate([old, self.QUEUED])[0], "1 task(s) running for 1 model slot(s) online (1 per slot)")
        # Busy beside an idle one: the slot is taken, and the message says the idle one is there.
        busy = leased("h0", {"requests": 2, "in_flight": 1, "updated_at": ago(600)})
        self.assertEqual(self.idle_gate([quiet, busy, self.QUEUED])[0],
                         "1 task(s) running for 1 model slot(s) online (1 per slot); 1 more leased, idle")

    def test_a_ceiling_of_leased_tasks_per_slot(self):
        idle = lambda tid: leased(tid, {"requests": 40, "in_flight": 0, "updated_at": ago(600)})
        self.assertEqual(self.idle_gate([idle("a"), idle("b"), self.QUEUED])[0],
                         "2 model task(s) leased (2 idle) for 1 model slot(s) online (at most 2 per slot)")
        self.assertEqual(self.idle_gate([idle("a"), idle("b"), self.QUEUED], max_per_slot=3), (None, "h1"))
        self.assertEqual(self.idle_gate([idle("a"), idle("b"), self.QUEUED], slots=2), (None, "h1"))
        # A task held for its worker counts toward the ceiling (it resumes without asking).
        held = {"id": "c", "state": "queued", "role": "work", "kind": "model-v1", "prefer_worker": "r-1",
                "prefer_until": "2026-11-14T20:05:00Z"}
        self.assertEqual(self.idle_gate([idle("a"), held, self.QUEUED], per_slot=2)[0],
                         "2 model task(s) leased (1 idle) for 1 model slot(s) online (at most 2 per slot)")
        # The ceiling never sits below the workers per slot.
        self.assertEqual(self.idle_gate([idle("a"), idle("b"), self.QUEUED], per_slot=3), (None, "h1"))
        # Tasks without the model still neither count nor wait.
        chk = {"id": "chk", "state": "queued", "role": "check", "kind": "model-v1", "seq": 3}
        self.assertEqual(self.idle_gate([idle("a"), idle("b"), self.QUEUED, chk]), (None, "chk"))

    def test_settings_are_checked(self):
        for kw in ({"idle_s": -1}, {"idle_s": float("nan")}, {"idle_s": 20, "max_per_slot": 0}, {"startup_s": -1}):
            with self.assertRaises(ValueError):
                G.SlotGate(FakeList([]), 1, **kw)
        g = G.SlotGate(FakeList([]), 1, max_per_slot=0)         # the ceiling is not used without idle release
        self.assertEqual((g.idle_s, g.max_per_slot, g.startup_s), (None, 0, 300.0))


class Memory(unittest.TestCase):
    def info(self, **gb):
        return lambda: {k: float(v) for k, v in gb.items()}

    def test_private_memory_not_memavailable(self):
        # 7.6 GB host, 1.0 GB private: 7.6 - 1.0 - 6.0 = 0.6 left
        m = {"MemTotal": 7.6, "MemAvailable": 6.4, "AnonPages": 0.8, "Shmem": 0.05, "SUnreclaim": 0.1, "PageTables": 0.03,
             "KernelStack": 0.02}
        self.assertIsNone(G.MemoryGate(0.5, 6.0, self.info(**m))())
        why = G.MemoryGate(0.7, 6.0, self.info(**m))()
        self.assertEqual(why, "memory: 1.0 GB private of 7.6 leaves 0.6 GB beside the page cache's 6.0, a session needs 0.7")
        self.assertIsNone(G.MemoryGate(0, 6.0, self.info(**m))())           # off
        self.assertIsNone(G.MemoryGate(0.7, 6.0, self.info())())            # unreadable: no gate

    def test_meminfo_parses_kb_to_gb(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "meminfo"
            f.write_text("MemTotal:        8000000 kB\nAnonPages:       1048576 kB\nHugePages_Total:       0\n")
            m = G.meminfo(str(f))
            self.assertAlmostEqual(m["AnonPages"], 1.0)
            self.assertAlmostEqual(G.private_gb(m), 1.0)
            self.assertEqual(G.meminfo(str(Path(d) / "none")), {})


class SpareCapacity(unittest.TestCase):
    def test_the_capacity_from_the_stats_and_the_queue(self):
        tasks = [{"id": "me", "state": "leased", "role": "work", "kind": "model-v1"},
                 {"id": "a", "state": "leased", "role": "work", "kind": "model-v1"},
                 {"id": "b", "state": "queued", "role": "work", "kind": "model-v1"},
                 {"id": "c", "state": "queued", "role": "check", "kind": "model-v1"},      # no model
                 {"id": "d", "state": "queued", "role": "submit", "kind": "submit"}]
        cap = lambda st, **kw: G.spare_capacity(FakeList(tasks), st, uses_model=uses_model, exclude="me", **kw)
        self.assertEqual(cap(lambda: {"nodes": {"slots": 3, "busy": 1}}), {"free": 2, "queued": 1, "slots": 3})
        self.assertEqual(cap(lambda: {"nodes": {"slots": 3, "busy": 3}}), {"free": 0, "queued": 1, "slots": 3})

        def down():
            raise OSError("no")
        self.assertIsNone(cap(down))
        self.assertIsNone(G.spare_capacity(FakeList(tasks, 503), lambda: {"nodes": {"slots": 3}}))

    def test_the_capacity_with_idle_release(self):
        """A leased task with no request in flight and its last one 10 minutes ago frees its slot; the ceiling
        (max_per_slot x slots, less every leased model task) bounds free; a gateway without live.in_flight counts every
        leased task, as without idle release."""
        quiet = {"requests": 40, "in_flight": 0, "updated_at": "2026-11-26T14:50:00Z"}
        tasks = [{"id": "me", "state": "leased", "role": "work", "kind": "model-v1"},
                 {"id": "a", "state": "leased", "role": "work", "kind": "model-v1", "live": quiet,
                  "lease": {"since": "2026-11-26T14:00:00Z"}},
                 {"id": "b", "state": "leased", "role": "work", "kind": "model-v1",
                  "live": dict(quiet, in_flight=1), "lease": {"since": "2026-11-26T14:00:00Z"}},
                 {"id": "c", "state": "queued", "role": "work", "kind": "model-v1"}]
        st = lambda: {"nodes": {"slots": 2, "busy": 1}}
        clock = Clock(G.epoch("2026-11-26T15:00:00Z"))
        cap = lambda ts=tasks, **kw: G.spare_capacity(FakeList(ts), st, exclude="me", clock=clock, **kw)
        self.assertEqual(cap(), {"free": 0, "queued": 1, "slots": 2})                     # a and b both count
        self.assertEqual(cap(idle_s=20), {"free": 1, "queued": 1, "slots": 2})            # a is idle
        old = [dict(t, live={k: v for k, v in t["live"].items() if k != "in_flight"}) if "live" in t else t for t in tasks]
        self.assertEqual(cap(old, idle_s=20), {"free": 0, "queued": 1, "slots": 2})       # no in_flight: as without
        # 2 slots x 2 per slot less the 1 busy leaves 3, but the ceiling (2 x max(1, 2) = 4) less the 2 leased: 2
        self.assertEqual(cap(idle_s=20, per_slot=2, max_per_slot=1), {"free": 2, "queued": 1, "slots": 2})


class Epoch(unittest.TestCase):
    def test_gateway_times(self):
        self.assertEqual(G.epoch("2026-11-14T20:00:00Z"), NOW)
        self.assertAlmostEqual(G.epoch("2026-11-14T21:00:00.25+01:00"), NOW + 0.25)
        for none in (None, "", "yesterday", "2026-11-14 20:00:00"):
            self.assertEqual(G.epoch(none), 0.0)


if __name__ == "__main__":
    unittest.main()
