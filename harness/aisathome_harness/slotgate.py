# SPDX-License-Identifier: Apache-2.0
"""A queue worker's slot gate: whether it may claim another task that uses the model, and which task.

The model slots are the gateway's: `nodes.slots` of its public /v1/stats is what a runner may size its work by.
The gate is the runners' own accounting against them, not a reservation on a node. It is a `worker.Worker`
`gate` (a reason to wait, else None) that also picks the task to claim (`pick`, claimed by id through
`Picking`):

    gate = SlotGate(client, per_slot=1, worker="host-1", kinds=["my-kind"], uses_model=..., runnable=...)
    w = Worker(Picking(client, gate), "host-1", state, child, gate=gate, claim_lock=state / "claim.lock")

Model slots (`per_slot`): no new model task while the project has as many leased model tasks as `per_slot` times
the model slots online (`per_slot` None: no limit). A task given back and still held for its worker counts as
running (that worker resumes it without asking). Tasks that run no model (`uses_model(task)` false: a project's
own check or submission, say) neither count nor wait: while the slots are full, or the memory gate says no, the
gate picks the first queued task of that sort instead. /v1/stats unreadable (or cut short): no slot limit this
time (logged), as a worker without the gate; the memory gate still applies. The task list unreadable: OSError, which
the worker logs before claiming as without a gate.

No model slots online (`bootstrap`, off by default): the gateway asks a connector for a pilot (a GPU node made on
demand) only for the project's workers it has seen claim or holding a lease, so a gate that waits for a slot before
it claims would wait for good where pilots are the only GPUs. With `bootstrap` N >= 1 and `nodes.slots` 0 the gate
still lets a model task be claimed while the project's leased (and held) model tasks are fewer than N. That lease
is the demand; the task's first model request waits for the pilot as long as the gateway queues it (the project's
queue timeout). The default, 0, waits for a slot. The memory gate still applies. /v1/stats unreadable is not 0
slots.

Idle release (`idle_s`): a leased model task counts against the slots only while it uses the model
(`holds_slot`), from the task list's `live` record of its model calls:
  (a) it has no live record of this lease yet (start-up: no `live`, or a `live.updated_at` before the lease's
      `since`, a continued try), for at most `startup_s` of the lease, so a try resumed after its model work does
      not hold the slot to its end;
  (b) a model request of it is in flight (`live.in_flight` > 0); or
  (c) its last request ended less than `idle_s` seconds ago (`live.updated_at`).
So a task in a long stretch with no model request (its own checks, say) frees its slot for the next task. A
gateway that does not report `live.in_flight` leaves every leased task counted: a task is never taken for idle
without that field. A hard ceiling stays: no new model task while the leased (and held) model tasks reach
`max_per_slot` (or `per_slot`, if larger) times the slots. The runner's clock is compared with the gateway's
`updated_at`. `idle_s` None: every leased model task counts, and no ceiling beyond `per_slot`.

Memory (`memory`, e.g. `MemoryGate`): a callable giving a reason to claim no new model task, or None.

The pick: the first queued task, in the gateway's claim order (requeued first, then `order`, then creation,
`seq`), of the worker's `kinds`, not held for another worker, not skipped by this worker (`skip`: task id to
the time until which it is left alone, e.g. after the worker gave it back as one it cannot run), and that this
release can run (`runnable(task)`: None, or why not). Always claimed by id: a claim left to the gateway can land
on a task this release cannot run when another host took the first one meanwhile. A claim by id that another
worker won is answered 409, and the worker asks again at its next poll.

`spare_capacity` counts alike for a project that sizes its next tasks by the room left.

The clock (`clock.time`), the stats (`stats`) and the log are parameters, for tests. Standard library only.
"""
from __future__ import annotations

import http.client
import json
import re
import sys
import time
import urllib.request
from collections.abc import Callable
from datetime import datetime
from pathlib import Path

from .queue import DEFAULT_LEASE_S

STARTUP_S = 300.0              # idle release: a leased task with no model request ended in its lease holds its slot so long
MAX_PER_SLOT = 2               # idle release: the ceiling of leased model tasks per model slot


def epoch(ts) -> float:
    """A gateway time (RFC 3339, any fraction, Z or an offset) in seconds since the epoch; 0 for none."""
    if not ts:
        return 0.0
    m = re.match(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)$", str(ts))
    if not m:
        return 0.0
    frac = float(m.group(2) or 0)
    return datetime.fromisoformat(m.group(1) + ("+00:00" if m.group(3) == "Z" else m.group(3))).timestamp() + frac


def get_json(url: str, timeout: float = 15, user_agent: str = "aisathome-harness") -> dict:
    """A public GET (no key), e.g. the gateway's /v1/stats."""
    with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": user_agent}), timeout=timeout) as r:
        return json.loads(r.read())


def holds_slot(t: dict, now: float, idle_s: float | None, inflight_known: bool = False,
               startup_s: float = STARTUP_S) -> bool:
    """Whether a leased model task (a list entry) holds a model slot at `now` (idle release, see the top). idle_s None:
    always. Else only with a model request in flight, within idle_s of its last one's end, or while it starts up: no
    request of this lease has ended yet (no `live`, or a `live.updated_at` before the lease's `since`, a continued try)
    and the lease is younger than startup_s. A gateway that does not report `live.in_flight`: always. A task with no
    `live` at all counts as starting up only when `inflight_known` (some leased task's `live` has `in_flight`, so the
    gateway counts calls); otherwise always, as an old gateway's."""
    if idle_s is None:
        return True
    since = epoch((t.get("lease") or {}).get("since"))

    def starting() -> bool:
        return not since or now - since < startup_s

    live = t.get("live")
    if not isinstance(live, dict):
        return starting() if inflight_known else True   # no request ended yet; an old gateway: always
    if "in_flight" not in live:
        return True                     # a gateway without the in-flight count
    try:
        if int(live.get("in_flight") or 0) > 0:
            return True
    except (TypeError, ValueError):
        return True
    last = epoch(live.get("updated_at"))
    if not last or last < since:
        return starting()               # none ended in this lease yet (a continued try's last one was in the lease before)
    return now - last < idle_s


def inflight_known(tasks: list[dict]) -> bool:
    """Whether the gateway counts model calls in flight: some leased task's `live` has `in_flight`."""
    return any(isinstance(t.get("live"), dict) and "in_flight" in t["live"] for t in tasks if t.get("state") == "leased")


def held_for_other(t: dict, worker: str, now: float) -> bool:
    """The gateway's heldFor: a task that came back waits for its last worker until prefer_until."""
    return bool(t.get("prefer_worker")) and t.get("prefer_worker") != worker and epoch(t.get("prefer_until")) > now


def every_task(t: dict) -> bool:
    """The default `uses_model`: every task takes a model slot."""
    return True


def any_task(t: dict) -> str | None:
    """The default `runnable`: this release runs every task."""
    return None


def _stderr(msg: str):
    print(msg, file=sys.stderr, flush=True)


# ----------------------------------------------------------------------------- memory

def meminfo(path: str = "/proc/meminfo") -> dict:
    """/proc/meminfo in GB (kB / 2**20)."""
    out = {}
    try:
        for line in Path(path).read_text().splitlines():
            k, _, v = line.partition(":")
            if v.split():
                out[k.strip()] = int(v.split()[0]) / 2**20
    except (OSError, ValueError):
        pass
    return out


def private_gb(m: dict) -> float:
    """The machine's memory that the page cache cannot have: processes' anonymous pages, shared memory (tmpfs,
    containers' /dev/shm), the kernel's own unreclaimable slab, page tables and stacks."""
    return sum(m.get(k, 0.0) for k in ("AnonPages", "Shmem", "SUnreclaim", "PageTables", "KernelStack"))


class MemoryGate:
    """A reason to claim no new model task while MemTotal - private memory - `reserve` leaves less than `session` GB
    (one more session); None otherwise, or when it is off (session 0) or /proc/meminfo is unreadable. `reserve` is
    the page cache a host's tasks need kept warm (a toolchain's working set, say): MemAvailable is no measure of
    this, since it counts those cached pages as free."""

    def __init__(self, session_gb: float, reserve_gb: float = 0.0, read=meminfo):
        self.session, self.reserve, self.read = float(session_gb), float(reserve_gb), read

    def __call__(self) -> str | None:
        if self.session <= 0:
            return None
        m = self.read()
        if not m.get("MemTotal"):
            return None
        left = m["MemTotal"] - private_gb(m) - self.reserve
        if left >= self.session:
            return None
        return (f"memory: {private_gb(m):.1f} GB private of {m['MemTotal']:.1f} leaves {left:.1f} GB beside the page "
                f"cache's {self.reserve:.1f}, a session needs {self.session:.1f}")


# ----------------------------------------------------------------------------- the gate

class SlotGate:
    """The worker's gate (worker.Worker `gate`, see the top): a reason to wait, else None, and the task to claim in
    `pick`. `client` needs `list()` (and `gateway` for the default `stats`)."""

    def __init__(self, client, per_slot: int | None, stats: Callable[[], dict] | None = None, clock=time,
                 worker: str = "", kinds: list[str] | None = None, runnable: Callable[[dict], str | None] | None = None,
                 memory: Callable[[], str | None] | None = None, idle_s: float | None = None,
                 max_per_slot: int = MAX_PER_SLOT, startup_s: float = STARTUP_S,
                 uses_model: Callable[[dict], bool] | None = None, log: Callable[[str], None] = _stderr,
                 bootstrap: int = 0):
        if per_slot is not None and per_slot < 1:
            raise ValueError("per_slot: a whole number >= 1")
        if not isinstance(bootstrap, int) or bootstrap < 0:
            raise ValueError("bootstrap: a whole number >= 0")
        if idle_s is not None and not idle_s >= 0:
            raise ValueError("idle_s: seconds >= 0")
        if idle_s is not None and max_per_slot < 1:
            raise ValueError("max_per_slot: a whole number >= 1")
        if not startup_s >= 0:
            raise ValueError("startup_s: seconds >= 0")
        self.c, self.per_slot, self.clock = client, per_slot, clock
        self.idle_s, self.max_per_slot, self.startup_s = idle_s, max_per_slot, startup_s
        self.stats = stats or (lambda: get_json(client.gateway + "/v1/stats"))
        self.worker, self.kinds = worker, kinds
        self.runnable = runnable or any_task
        self.uses_model = uses_model or every_task
        self.memory, self.log, self.bootstrap = memory, log, bootstrap
        self.pick: str | None = None
        self.skip: dict[str, float] = {}

    def __call__(self) -> str | None:
        self.pick = None
        code, body = self.c.list()
        if code != 200:
            raise OSError(f"list: HTTP {code}")
        now = self.clock.time()
        tasks = body.get("tasks") or []
        uses_model = self.uses_model
        why, boot = None, False
        try:
            st = self.stats() if self.per_slot else None
        except (OSError, ValueError, http.client.HTTPException) as e:  # the slot limit unknown: as without the gate
            st = None
            self.log(f"gate: /v1/stats unreadable ({type(e).__name__}); no slot limit this time")
        if st is not None:
            slots = int(((st or {}).get("nodes") or {}).get("slots") or 0)
            held = [t for t in tasks if uses_model(t) and t.get("state") == "queued" and t.get("prefer_worker")
                    and epoch(t.get("prefer_until")) > now]
            leased = [t for t in tasks if uses_model(t) and t.get("state") == "leased"]
            known = inflight_known(leased)
            busy = len(held) + sum(holds_slot(t, now, self.idle_s, known, self.startup_s) for t in leased)
            idle = len(held) + len(leased) - busy
            cap = max(self.max_per_slot, self.per_slot)
            if busy >= slots * self.per_slot:
                why = (f"{busy} task(s) running for {slots} model slot(s) online" if slots
                       else "no model slots online") + f" ({self.per_slot} per slot)" + \
                      (f"; {idle} more leased, idle" if idle else "")
                boot = not slots and len(held) + len(leased) < self.bootstrap   # the demand for a pilot (see the top)
            elif self.idle_s is not None and busy + idle >= slots * cap:
                why = f"{busy + idle} model task(s) leased ({idle} idle) for {slots} model slot(s) online (at most {cap} per slot)"
        if why is None and self.memory is not None:
            why = self.memory()
        self.skip = {k: v for k, v in self.skip.items() if v > now}
        queue = sorted((t for t in tasks if t.get("state") == "queued" and (not self.kinds or t.get("kind") in self.kinds)
                        and not held_for_other(t, self.worker, now)),
                       key=lambda t: (not t.get("requeued"), int(t.get("order") or 0), int(t.get("seq") or 0)))
        ok = [t for t in queue if t.get("id") not in self.skip and self.runnable(t) is None]
        if boot and any(uses_model(t) for t in ok):
            why = self.memory() if self.memory is not None else None
            if why is None and uses_model(ok[0]):
                self.log(f"gate: no model slots online; claiming a model task ({ok[0].get('id')}) so the gateway "
                         f"sees the demand for one")
        if why is not None:
            ok = [t for t in ok if not uses_model(t)]
            if not ok:
                return why
        elif not ok:
            if not queue:
                return None             # nothing queued: the claim says so (204)
            return f"{len(queue)} queued task(s), none this release can run (e.g. {queue[0].get('id')}: " \
                   f"{self.runnable(queue[0]) or 'skipped here'})"
        self.pick = ok[0]["id"]         # always by id (see the top)
        return None


class Picking:
    """The worker's client with the gate's pick: a new claim takes the task the gate picked (by id), else the
    gateway's choice. Everything else is the client's."""

    def __init__(self, client, gate: SlotGate):
        self._c, self._gate = client, gate

    def __getattr__(self, k):
        return getattr(self._c, k)

    def claim(self, worker: str, lease_seconds: int = DEFAULT_LEASE_S, kinds: list[str] | None = None,
              task: str | None = None, **kw):
        if task is None and self._gate.pick:
            task, self._gate.pick = self._gate.pick, None
        return self._c.claim(worker, lease_seconds, kinds=kinds, task=task, **kw)


def spare_capacity(client, stats: Callable[[], dict], per_slot: int = 1, idle_s: float | None = None,
                   max_per_slot: int = MAX_PER_SLOT, startup_s: float = STARTUP_S,
                   uses_model: Callable[[dict], bool] | None = None, exclude: str | None = None,
                   clock=time) -> dict | None:
    """The model capacity a project's next tasks would meet, {"free", "queued", "slots"}, or None when it cannot be
    read. free = slots online (/v1/stats `nodes.slots`) x `per_slot`, less the larger of the gateway's busy nodes
    (`nodes.busy`) and the project's leased model tasks (`exclude`, e.g. the task asking, not counted: its slot frees
    now); queued = the project's queued model tasks. With idle release (`idle_s`) only the leased model tasks that
    hold a slot (`holds_slot`) count, and free is at most the room under the ceiling: `max_per_slot` (or `per_slot`,
    if larger) x slots, less all leased model tasks."""
    uses_model = uses_model or every_task
    try:
        st = stats()
        code, body = client.list()
        if code != 200:
            return None
        nodes = (st or {}).get("nodes") or {}
        slots = int(nodes.get("slots") or 0)
    except (OSError, ValueError, TypeError, AttributeError, http.client.HTTPException):
        return None
    now = clock.time()
    tasks = [t for t in body.get("tasks") or [] if uses_model(t) and t.get("id") != exclude]
    leased = [t for t in tasks if t.get("state") == "leased"]
    queued = sum(t.get("state") == "queued" for t in tasks)
    known = inflight_known(leased)
    busy = max(int(nodes.get("busy") or 0), sum(holds_slot(t, now, idle_s, known, startup_s) for t in leased))
    free = slots * per_slot - busy
    if idle_s is not None:
        free = min(free, slots * max(max_per_slot, per_slot) - len(leased))
    return {"free": max(0, free), "queued": queued, "slots": slots}
