# SPDX-License-Identifier: Apache-2.0
"""A queue worker's lifecycle: claim a task, keep its lease, run it in a child process, report, and survive
stops. The project supplies what a task means; this module only moves tasks between the queue and a child.

    w = Worker(client, "host-1", state_dir, child=[sys.executable, "-u", "myproject.py"], kinds=["other"])
    w.run_forever()

The loop (one task at a time; run several workers, on one host or several: the gateway decides who gets what):

  - held tasks first: a task this worker claimed and has not reported (its note in <state>/held/) is resumed
    with its lease if still valid, else claimed back by id (the gateway holds a given-back task for its last
    worker, which has its checkpoint); then a new claim (`kinds`, `hold_seconds`);
  - the claimed task's spec goes through `on_claim(task)` (the project's: validate, add what the child needs;
    `Refuse` fails the task at once) and is held, then the child runs it:
        child + ["run-task", SPEC, OUT, ERR]
    while a thread renews the lease every `heartbeat_s` (409: expired or cancelled; the worker claims it back
    when it is queued again, else stops the child: "lease lost");
  - the child exits 0 with OUT written: `complete` with OUT's result; with ERR {"give_back": true} and exit
    GIVE_BACK (the task cannot start on this host: its checkpoint unreadable): given back, counted as a crash;
    otherwise a crash: `fail` with ERR's error (type and message only), the worker goes on with the next task;
    `max_crashes` in a row pause it (<state>/paused, with the reason; remove it to go on);
  - a stop (SIGTERM/SIGINT, or a watcher's notice such as a spot interruption): no new work; the child gets
    SIGTERM (`stop_wait_s` to exit, then SIGKILL to its process group); with `checkpoints` the task's
    checkpoint is taken by child + ["checkpoint-task", SPEC] (within `checkpoint_wait_s`); the task is given
    back (fail with requeue: its try is kept) and stays held for the next start;
  - after a report (or a lost lease) the task's folders go: child + ["cleanup-task", SPEC] (unless
    `keep_trees` and the task did not fail);
  - reports retry `report_tries` times `report_wait_s` apart when the gateway is unreachable; one that never
    lands stays held (phase unreported/failed) and is reported at the next step.

No new claim while `pause_file` exists, below `min_free_gb` free on `disk_path`, or while paused after crashes,
nor while `gate()` (optional, the project's) returns a reason, e.g. as many tasks running as the model has
slots (held tasks still resume: they are running already). With `claim_lock` (a file), the gate and the claim
run under an exclusive lock on it, so the workers of one host never pass the gate together.
`busy_file` (optional) holds the task id while any child runs, for a scale-in check; it is removed when the
child ends.

The child side: `child_main(argv, run=..., cleanup=..., checkpoint=..., lock=...)` dispatches the three
commands to the project's callbacks and writes OUT/ERR as the worker expects. `GiveBack` from `run` means
"give the task back, not failed".

The child command, the clock (`clock.time`, `clock.monotonic`) and the process calls (`popen`, `run`) are
parameters, for tests. Standard library only.
"""
from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import threading
import time
import traceback
from contextlib import contextmanager
from pathlib import Path
from typing import Callable

from .queue import error_text, last_note, safe_text

GIVE_BACK = 4                  # the child's exit code for "give the task back" (it cannot start on this host)
CRASH = 3                      # the child's exit code for a crash (ERR holds the error)
STOP_WAIT_S = 45               # a stopped child's time to exit before it is killed
CHECKPOINT_WAIT_S = 50         # the checkpoint child after a stop (a spot notice gives about 120 s in all)
CLEANUP_WAIT_S = 600
LEASE_S, HEARTBEAT_S, POLL_S = 600, 60, 60
REPORT_TRIES, REPORT_WAIT_S = 5, 30
MAX_CRASHES_IN_A_ROW = 3
GIVE_BACK_REASON = "interrupted: the worker stopped (spot stop, reboot or service stop); resumes from its checkpoint"


class Refuse(Exception):
    """`on_claim`: the task cannot run on this worker (an unknown spec version): it is failed with this reason."""


class GiveBack(Exception):
    """In the child: the task cannot start here for a reason of the host, not the task's (its checkpoint
    unreadable): the worker gives it back instead of failing it."""


class Worker:
    """The loop: held tasks first (this worker's, after a stop), then new claims, one task at a time."""

    def __init__(self, client, name: str, state: Path, child: list[str], *,
                 on_claim: Callable[[dict], dict] | None = None, kinds: list[str] | None = None,
                 lease_s: int = LEASE_S, heartbeat_s: float = HEARTBEAT_S, poll_s: float = POLL_S,
                 stop_wait_s: float = STOP_WAIT_S, checkpoint_wait_s: float = CHECKPOINT_WAIT_S,
                 cleanup_wait_s: float = CLEANUP_WAIT_S, report_tries: int = REPORT_TRIES,
                 report_wait_s: float = REPORT_WAIT_S, max_crashes: int = MAX_CRASHES_IN_A_ROW,
                 min_free_gb: float = 0, disk_path: Path | None = None, pause_file: Path | None = None,
                 keep_trees: bool = False, checkpoints: bool = False, hold_s: int | None = None,
                 watchers: tuple = (), busy_file: Path | None = None, give_back_reason: str = GIVE_BACK_REASON,
                 gate: Callable[[], str | None] | None = None, claim_lock: Path | None = None,
                 describe: str = "", log=print, clock=time, popen=subprocess.Popen, run=subprocess.run):
        self.c, self.name, self.state, self.child = client, name, Path(state), list(child)
        self.on_claim = on_claim or (lambda task: {**(task.get("spec") or {}), "task": task["id"]})
        self.kinds = list(kinds) if kinds else None
        self.lease_s, self.hb_s, self.poll_s, self.stop_wait_s = lease_s, heartbeat_s, poll_s, stop_wait_s
        self.checkpoint_wait_s, self.cleanup_wait_s = checkpoint_wait_s, cleanup_wait_s
        self.report_tries, self.report_wait_s, self.max_crashes = report_tries, report_wait_s, max_crashes
        self.min_free_gb, self.disk_path, self.pause_file = min_free_gb, Path(disk_path or state), pause_file
        self.keep_trees, self.checkpoints, self.hold_s = keep_trees, checkpoints, hold_s
        self.watchers, self.busy_file, self.give_back_reason = tuple(watchers), busy_file, give_back_reason
        self.gate, self.claim_lock = gate, claim_lock
        self.gated: str | None = None    # the gate's last reason (for the waiting line)
        self._said_hold = False
        self.describe, self.log, self.clock, self.popen, self.run = describe, log, clock, popen, run
        self.stopping = threading.Event()
        self.crashes = 0                 # in a row, this run
        self.proc: subprocess.Popen | None = None
        self.lock = threading.Lock()
        self.notice_at: float | None = None      # clock.monotonic() of a watcher's notice
        for d in ("held", "tasks", "logs"):
            (self.state / d).mkdir(parents=True, exist_ok=True)

    # --- local state: the tasks this worker holds (claimed and not yet reported)
    def held_file(self, tid: str) -> Path:
        return self.state / "held" / f"{tid}.json"

    def hold(self, rec: dict):
        f = self.held_file(rec["task"])
        tmp = f.with_suffix(".tmp")
        tmp.write_text(json.dumps(rec) + "\n")
        tmp.replace(f)

    def held(self) -> list[dict]:
        out = []
        for f in sorted((self.state / "held").glob("*.json")):
            try:
                out.append(json.loads(f.read_text()))
            except ValueError:
                f.unlink()
        return out

    def release(self, tid: str):
        self.held_file(tid).unlink(missing_ok=True)

    # --- signals
    def stop(self, *_):
        """SIGTERM/SIGINT: no new work; the running child is stopped and its task given back."""
        self.stopping.set()
        self._signal_child(signal.SIGTERM)

    def notice(self, why: str = ""):
        """A watcher's notice (a spot interruption: about 2 minutes left): the SIGTERM path, before the SIGTERM."""
        self.notice_at = self.clock.monotonic()
        self.stop()

    def _signal_child(self, sig, p=None):
        """`sig` to the running child's process group (or to `p`'s, a given child, while it still runs). No lock:
        the signal handler runs this on the main thread, which may hold `self.lock` while it starts the child
        (a child started meanwhile gets its SIGTERM from `_wait`)."""
        if p is None:
            p = self.proc
        if p and p.poll() is None:
            try:
                os.killpg(p.pid, sig)
            except ProcessLookupError:
                pass

    @contextmanager
    def _busy(self, tid: str):
        if self.busy_file is None:
            yield
            return
        try:
            Path(self.busy_file).parent.mkdir(parents=True, exist_ok=True)
            Path(self.busy_file).write_text(tid + "\n")
        except OSError as e:
            self.log(f"busy marker {self.busy_file}: {type(e).__name__}")
        try:
            yield
        finally:
            try:
                Path(self.busy_file).unlink(missing_ok=True)
            except OSError:
                pass

    # --- the loop
    def paused(self) -> str | None:
        if self.pause_file is not None and Path(self.pause_file).exists():
            return f"paused ({Path(self.pause_file).parent.name}/{Path(self.pause_file).name})"
        if (self.state / "paused").exists():
            return f"paused after {self.max_crashes} crashes in a row ({self.state / 'paused'}; remove it to go on)"
        if self.crashes >= self.max_crashes:
            self.crashes = 0                 # the owner removed the file: count afresh
        if self.min_free_gb > 0:
            d = self.disk_path if self.disk_path.exists() else self.disk_path.parent
            free = shutil.disk_usage(d).free / 2**30
            if free < self.min_free_gb:
                return f"disk: {free:.1f} GB free, below {self.min_free_gb:g}"
        return None

    def run_forever(self, once: bool = False) -> int:
        if threading.current_thread() is threading.main_thread():      # signals reach the main thread only
            signal.signal(signal.SIGTERM, self.stop)
            signal.signal(signal.SIGINT, self.stop)
        self.log(f"worker {self.name}: project queue {self.c.base}{self.describe}")
        watches = []
        for make in self.watchers:
            w = make(self.notice)
            w.start()
            watches.append(w)
        said = None
        while not self.stopping.is_set():
            did = self.step()
            if once and not did:
                break
            if not did:
                why = self.paused() or self.gated or "nothing queued"
                if why != said:
                    self.log(f"waiting: {why}")
                    said = why
                self.stopping.wait(self.poll_s)
            else:
                said = None
        for w in watches:
            w.stop()
        self.log(f"worker {self.name}: stopped")
        return 0

    def step(self) -> bool:
        """One unit of work: a held task (resumed or reported), else a new claim. False when there was none."""
        for rec in self.held():
            if self.stopping.is_set():
                return True
            self.resume(rec)
            return True
        if self.paused():
            return False
        with self._claim_lock():
            self.gated = self._gate()
            if self.gated:
                return False
            try:
                code, body = self.c.claim(self.name, self.lease_s, kinds=self.kinds, hold_seconds=self.hold_s)
            except OSError as e:
                self.log(f"claim: gateway unreachable ({type(e).__name__})")
                return False
        if getattr(self.c, "hold_refused", False) and not self._said_hold:
            self._said_hold = True
            self.log("claim: the gateway has no hold_seconds; a given-back task waits its default hold for this worker")
        if code == 204:
            return False
        if code != 200:
            self.log(f"claim: HTTP {code} {error_text(body)}")
            return False
        self.start(body["task"], body["lease"]["id"])
        return True

    def _gate(self) -> str | None:
        if self.gate is None:
            return None
        try:
            return self.gate()
        except Exception as e:  # noqa: BLE001  (a broken gate must not stop the queue: claim as without one)
            self.log(f"gate: {type(e).__name__}: {safe_text(e, 200)}; claiming as without it")
            return None

    @contextmanager
    def _claim_lock(self):
        if self.claim_lock is None:
            yield
            return
        import fcntl
        Path(self.claim_lock).parent.mkdir(parents=True, exist_ok=True)
        with open(self.claim_lock, "a") as fh:
            fcntl.flock(fh, fcntl.LOCK_EX)
            try:
                yield
            finally:
                fcntl.flock(fh, fcntl.LOCK_UN)

    def start(self, task: dict, lease: str):
        try:
            spec = self.on_claim(task)
        except Refuse as e:
            self.report_fail({"task": task["id"], "lease": lease}, str(e))
            return
        rec = {"task": task["id"], "lease": lease, "spec": spec, "phase": "running", "since": self.clock.time()}
        self.hold(rec)
        self.log(f"[{task['id']}] claimed ({last_note(task)})")
        self.execute(rec)

    def resume(self, rec: dict):
        """A task this worker holds: its lease if still valid, else claimed back by id; then run on (the
        child resumes from the checkpoint) or report what is owed. Only 404 or 409 (no such task, not claimable)
        mean it is not ours any more; any other answer (503: the queue's disk) or none keeps everything held
        and asks again at the next step."""
        tid = rec["task"]
        try:
            code, body = self.c.heartbeat(tid, self.name, rec["lease"])
            if code in (404, 409):
                code, body = self.c.claim(self.name, self.lease_s, task=tid, hold_seconds=self.hold_s)
                if code in (404, 409):
                    self.log(f"[{tid}] not ours any more (HTTP {code} {error_text(body)}); its folders go")
                    self.release(tid)
                    self.cleanup(rec)
                    return
                if code == 200:
                    rec["lease"] = body["lease"]["id"]
                    self.log(f"[{tid}] taken back ({last_note(body['task'])})")
            if code != 200:
                self.log(f"[{tid}] gateway answered HTTP {code} {error_text(body)}; held, trying again")
                self.stopping.wait(self.poll_s)
                return
        except OSError as e:
            self.log(f"[{tid}] gateway unreachable ({type(e).__name__}); trying again")
            self.stopping.wait(self.poll_s)
            return
        self.hold(rec)
        if rec.get("phase") == "failed":
            self.report_fail(rec, rec.get("error") or "failed")
            return
        self.execute(rec)

    def _beat(self, rec: dict, done: threading.Event, lost: threading.Event, p):
        """The lease's heartbeats while the child `p` runs; a lost lease stops it (that child only: never a later
        task's). `execute` joins this thread before it reports, so a late answer acts before the report."""
        tid = rec["task"]
        while not done.wait(self.hb_s):
            try:
                code, body = self.c.heartbeat(tid, self.name, rec["lease"])
            except OSError:
                continue                     # the lease has minutes left; try again at the next beat
            if code == 409:            # expired (the gateway unreachable too long) or cancelled: take it back if queued
                try:
                    code, body = self.c.claim(self.name, self.lease_s, task=tid, hold_seconds=self.hold_s)
                except OSError:
                    continue
                if code == 200:
                    rec["lease"] = body["lease"]["id"]
                    self.hold(rec)
                    self.log(f"[{tid}] lease had expired; taken back ({last_note(body['task'])})")
                    continue
            if code in (404, 409):
                self.log(f"[{tid}] lease lost ({error_text(body)}); stopping the task")
                lost.set()
                self._signal_child(signal.SIGTERM, p)
                return

    def execute(self, rec: dict):
        tid, spec = rec["task"], rec["spec"]
        d = self.state / "tasks" / tid
        d.mkdir(parents=True, exist_ok=True)
        specf, out, err = d / "spec.json", d / "result.json", d / "error.json"
        specf.write_text(json.dumps({**spec, "worker": self.name}) + "\n")
        for f in (out, err):
            f.unlink(missing_ok=True)
        lost = threading.Event()
        done = threading.Event()
        with open(self.state / "logs" / f"{tid}.log", "ab") as logf, self._busy(tid):
            with self.lock:
                if self.stopping.is_set():
                    self.proc = None
                else:
                    self.proc = self.popen(self.child + ["run-task", str(specf), str(out), str(err)], stdout=logf,
                                           stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, start_new_session=True)
            p = self.proc
            if p is None:
                done.set()
                self.give_back(rec)
                return
            hb = threading.Thread(target=self._beat, args=(rec, done, lost, p), daemon=True)
            hb.start()
            rc = self._wait(p, lost)
        done.set()
        hb.join()                  # a heartbeat still in flight acts now (a new lease, lost), not after the report
        with self.lock:
            self.proc = None
        if lost.is_set():
            self.release(tid)
            self.cleanup(rec)
            return
        if self.stopping.is_set() and not out.exists():
            self.give_back(rec)
            return
        if rc == 0 and out.exists():
            self.crashes = 0
            self.report_done(rec, json.loads(out.read_text()))
            return
        e = json.loads(err.read_text()) if err.exists() else {}
        if rc == GIVE_BACK and e.get("give_back"):
            self.log(f"[{tid}] cannot start here: {safe_text(e.get('error'), 300)}; giving it back")
            self.give_back(rec, save=False)
            self.release(tid)
            self.crashed(tid, f"its checkpoint could not be restored: {e.get('error')}")
            self.stopping.wait(self.poll_s)
            return
        why = e.get("error") if err.exists() else \
            f"the task's process ended with {'signal ' + str(-rc) if rc < 0 else 'exit ' + str(rc)} and no error record"
        self.log(f"[{tid}] crashed: {safe_text(why, 300)}")
        rec.update(phase="failed", error=f"crashed: {why}")
        self.hold(rec)
        self.report_fail(rec, rec["error"])
        self.crashed(tid, why)

    def crashed(self, tid: str, why: str):
        """One more task in a row that could not run here; at `max_crashes` the worker pauses (a broken host, or
        a checkpoint store it cannot read, must not use up the queue's tries and interruptions)."""
        self.crashes += 1
        if self.crashes >= self.max_crashes:
            stamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(self.clock.time()))
            (self.state / "paused").write_text(f"{stamp} {self.crashes} tasks crashed in a row, the last {tid}: "
                                               f"{safe_text(why, 300)}\n")
            self.log(f"{self.crashes} tasks crashed in a row: no new task until {self.state / 'paused'} is removed")

    def _wait(self, p, lost: threading.Event | None = None) -> int:
        """The child's exit code; after a stop or a lost lease (both sent SIGTERM), its time to exit, then
        SIGKILL to its group."""
        while True:
            try:
                return p.wait(timeout=1)
            except subprocess.TimeoutExpired:
                pass
            if self.stopping.is_set() or (lost is not None and lost.is_set()):
                self._signal_child(signal.SIGTERM, p)       # again: a stop while the child started missed it
                try:
                    return p.wait(timeout=self.stop_wait_s)
                except subprocess.TimeoutExpired:
                    self._signal_child(signal.SIGKILL, p)
                    return p.wait()

    def _report(self, what: str, fn) -> tuple[int, dict] | None:
        for i in range(self.report_tries):
            try:
                return fn()
            except OSError as e:
                self.log(f"{what}: gateway unreachable ({type(e).__name__}), try {i + 1} of {self.report_tries}")
                if self.stopping.wait(self.report_wait_s):
                    break
        return None

    def report_done(self, rec: dict, result: dict):
        tid = rec["task"]
        r = self._report(f"[{tid}] complete", lambda: self.c.complete(tid, self.name, rec["lease"], result))
        if r is None:
            rec["phase"] = "unreported"       # held: at the next step the child finds the task finished and reports again
            self.hold(rec)
            return
        code, body = r
        if code == 200:
            self.log(f"[{tid}] done: {result.get('outcome')}")
        else:
            self.log(f"[{tid}] complete refused: HTTP {code} {error_text(body)}")
            if code in (409, 503):         # lease lost or the queue's disk: held, taken back and reported again
                rec["phase"] = "unreported"
                self.hold(rec)
                return
        self.release(tid)
        self.cleanup(rec)

    def report_fail(self, rec: dict, reason: str):
        tid = rec["task"]
        r = self._report(f"[{tid}] fail", lambda: self.c.fail(tid, self.name, rec["lease"], reason))
        if r is None or r[0] == 503:
            return                            # held with phase failed: reported at the next step
        code, body = r
        if code != 200:
            self.log(f"[{tid}] fail refused: HTTP {code} {error_text(body)}")
        else:
            self.log(f"[{tid}] failed, reported; going on with the next task")
        if code in (200, 404) or (code == 409 and "failed" in error_text(body)):
            self.release(tid)
        if rec.get("spec"):
            self.cleanup(rec)

    def give_back(self, rec: dict, save: bool = True):
        """A stop: the task back to the queue with its try (requeue), held here for the next start; with
        `checkpoints`, its checkpoint is taken first, so any worker can resume it."""
        tid = rec["task"]
        rec["phase"] = "interrupted"
        self.hold(rec)
        if self.checkpoints and save:
            self.checkpoint(rec)
        try:
            code, body = self.c.fail(tid, self.name, rec["lease"], self.give_back_reason, requeue=True)
            since = f", {self.clock.monotonic() - self.notice_at:.1f} s after the spot notice" if self.notice_at else ""
            self.log(f"[{tid}] given back (HTTP {code}{' ' + error_text(body) if code != 200 else ''}{since})")
        except OSError:
            self.log(f"[{tid}] could not give it back (gateway unreachable); the lease will expire and it comes back here")

    def _child_run(self, rec: dict, verb: str, specf: Path, timeout: float) -> int:
        with open(self.state / "logs" / f"{rec['task']}.log", "ab") as logf, self._busy(rec["task"]):
            try:
                return self.run(self.child + [verb, str(specf)], stdout=logf, stderr=subprocess.STDOUT,
                                stdin=subprocess.DEVNULL, timeout=timeout).returncode
            except subprocess.TimeoutExpired:
                return -1

    def checkpoint(self, rec: dict):
        """The task's checkpoint, in a child (`checkpoint-task`), within `checkpoint_wait_s`."""
        tid = rec["task"]
        d = self.state / "tasks" / tid
        d.mkdir(parents=True, exist_ok=True)
        (d / "ckpt-spec.json").write_text(json.dumps({**rec["spec"], "worker": self.name, "why": "give-back"}) + "\n")
        t0 = self.clock.monotonic()
        rc = self._child_run(rec, "checkpoint-task", d / "ckpt-spec.json", self.checkpoint_wait_s)
        self.log(f"[{tid}] checkpoint {'saved' if rc == 0 else f'failed (exit {rc}; see logs/{tid}.log)'} "
                 f"in {self.clock.monotonic() - t0:.1f} s")

    def cleanup(self, rec: dict):
        """The task's folders, in a child (`cleanup-task`; a crash there must not end the worker)."""
        if self.keep_trees and rec.get("phase") != "failed":
            return
        d = self.state / "tasks" / rec["task"]
        d.mkdir(parents=True, exist_ok=True)
        (d / "spec.json").write_text(json.dumps(rec["spec"]) + "\n")
        rc = self._child_run(rec, "cleanup-task", d / "spec.json", self.cleanup_wait_s)
        if rc != 0:
            self.log(f"[{rec['task']}] cleanup failed (exit {rc}); see logs/{rec['task']}.log")
        shutil.rmtree(d, ignore_errors=True)


# ----------------------------------------------------------------------------- the child

LOCK_WAIT_S = {"run-task": 120, "cleanup-task": 0, "checkpoint-task": 20}


def child_main(argv: list[str], run: Callable[[dict], dict], cleanup: Callable[[dict], None] | None = None,
               checkpoint: Callable[[dict], int] | None = None, lock: Callable[[str, float], object] | None = None) -> int:
    """`run-task SPEC OUT ERR` / `cleanup-task SPEC` / `checkpoint-task SPEC`: the worker's child. `run(spec)`
    returns the result (written to OUT, exit 0); `GiveBack` writes ERR with give_back (exit GIVE_BACK); any other
    exception (SystemExit too) writes ERR with the error's type and message only (exit CRASH). `cleanup(spec)`
    and `checkpoint(spec)` (its exit code) never write OUT/ERR. `lock(task, wait_s)` (optional): the task's lock
    on this host, a context manager or None when another process holds it (then a cleanup or checkpoint leaves
    everything alone, a run fails)."""
    if not argv or argv[0] not in LOCK_WAIT_S or len(argv) < (4 if argv[0] == "run-task" else 2):
        print(f"usage: run-task SPEC OUT ERR | cleanup-task SPEC | checkpoint-task SPEC (got {argv[:1]})", file=sys.stderr)
        return 2
    cmd, spec = argv[0], json.loads(Path(argv[1]).read_text())
    held = lock(spec["task"], LOCK_WAIT_S.get(cmd, 120)) if lock else None
    if lock and held is None:
        if cmd in ("cleanup-task", "checkpoint-task"):
            print(f"[{spec['task']}] another process on this host runs the task; its folders stay", file=sys.stderr)
            return 0
        Path(argv[3]).write_text(json.dumps({"error": "another process on this host holds the task's lock"}) + "\n")
        return CRASH
    if held is None:
        return _child(cmd, spec, argv, run, cleanup, checkpoint)
    with held:
        return _child(cmd, spec, argv, run, cleanup, checkpoint)


def _child(cmd: str, spec: dict, argv: list[str], run, cleanup, checkpoint) -> int:
    try:
        if cmd == "cleanup-task":
            if cleanup:
                cleanup(spec)
            return 0
        if cmd == "checkpoint-task":
            return checkpoint(spec) if checkpoint else 0
        res = run(spec)
        tmp = Path(argv[2] + ".tmp")
        tmp.write_text(json.dumps(res, ensure_ascii=False) + "\n")
        tmp.replace(argv[2])
        return 0
    except GiveBack as e:
        Path(argv[3]).write_text(json.dumps({"error": str(e)[:2000], "give_back": True}) + "\n")
        return GIVE_BACK
    except (Exception, SystemExit) as e:          # noqa: BLE001  (a crash of this task, whatever it is)
        if cmd in ("cleanup-task", "checkpoint-task"):
            print(f"{cmd} failed: {type(e).__name__}: {e}", file=sys.stderr)
            return CRASH
        msg = f"{type(e).__name__}: {e}" if not isinstance(e, SystemExit) else f"exit: {e.code}"
        traceback.print_exc()
        Path(argv[3]).write_text(json.dumps({"error": msg[:2000]}) + "\n")
        return CRASH
