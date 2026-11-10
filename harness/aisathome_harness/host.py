# SPDX-License-Identifier: Apache-2.0
"""The machine the sessions run on: its limits, its shared build slots, and host-side commands.

One `Host` describes one machine for every project and session on it. Its `state` directory (host-only)
holds what they share: the build-slot locks, the egress forwarders' lock. Two projects that use the same
state directory share the slots, so their builds queue for the same CPUs; that is the point.

Build slots. Compiling (Lean, here) is what saturates a machine, so builds take one of `slots` slots, on
the host (`Host.slot`, `Host.shell(slot=True)`) and inside the sandboxes (sandbox/buildslot.sh, mounted
into each container, the same lock files): `flock` on <state>/slots/slot.<i>, and a build starts only
while the machine has at least `build_min_free_gb` GB of available memory. Sessions themselves are not
limited here; how many run at once is the caller's (a queue's) decision.

`Host.sized()` sizes the limits from the machine; every field can be set instead (an adapter's config).
Standard library only.
"""
from __future__ import annotations

import fcntl
import os
import shlex
import stat
import subprocess
import time
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path


def default_state() -> Path:
    return Path(os.environ.get("AISATHOME_HARNESS_STATE") or Path.home() / ".local" / "state" / "aisathome-harness")


def _open_slot(path: Path, create: bool = False) -> int:
    """A build slot's lock file, opened read-only (flock needs no more) and never through a symlink: the
    sandboxes see the slot directory read-only, and one written by an older harness is not trusted."""
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | (os.O_CREAT if create else 0)   # a FIFO never blocks
    fd = os.open(path, flags, 0o644)
    if not stat.S_ISREG(os.fstat(fd).st_mode):
        os.close(fd)
        raise OSError(f"build slot {path} is not a regular file")
    return fd


def mem_available_gb() -> float:
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemAvailable:"):
                return int(line.split()[1]) / 2**20
    except OSError:
        pass
    return float("inf")


def mem_total_gb() -> float:
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemTotal:"):
                return int(line.split()[1]) / 2**20
    except OSError:
        pass
    return 16.0


@dataclass
class Host:
    state: Path = field(default_factory=default_state)
    slots: int = 2                    # concurrent builds on this machine
    build_min_free_gb: float = 10     # a build starts only above this much available memory
    cpus: float = 8                   # per sandbox container
    memory_gb: int = 16               # per sandbox container (no swap)
    pids: int = 1024                  # per sandbox container
    threads: int = 8                  # compiler threads per build
    nice: int = 0                     # host-side commands run at this niceness
    slot_poll_s: float = 3

    @classmethod
    def sized(cls, state: Path | None = None, **over) -> "Host":
        """Limits from this machine: up to 8 CPUs and threads per session, 40% of memory per container
        (16 GB on machines with 32 GB or more), a quarter of memory (10 GB above 32) as the build floor."""
        cpus = os.cpu_count() or 2
        mem = mem_total_gb()
        h = cls(state=Path(state) if state else default_state(), cpus=min(8, cpus), threads=min(8, cpus),
                memory_gb=16 if mem >= 32 else max(2, int(mem * 0.4)),
                build_min_free_gb=10 if mem >= 32 else max(2, round(mem * 0.25)))
        for k, v in over.items():
            if not hasattr(h, k):
                raise KeyError(f"unknown Host setting {k}")
            setattr(h, k, v)
        return h

    @property
    def slot_dir(self) -> Path:
        d = self.state / "slots"
        d.mkdir(parents=True, exist_ok=True)
        for i in range(1, self.slots + 1):
            os.close(_open_slot(d / f"slot.{i}", create=True))
        return d

    def limits(self) -> dict:
        return {"docker_memory_gb": self.memory_gb, "docker_pids": self.pids}

    @contextmanager
    def slot(self, log: Path | None = None, sleep=time.sleep):
        """Hold one build slot (waiting for a free one and for enough available memory)."""
        d, t0 = self.slot_dir, int(time.time())
        while True:
            if mem_available_gb() >= self.build_min_free_gb:
                for i in range(1, self.slots + 1):
                    fh = os.fdopen(_open_slot(d / f"slot.{i}"), "rb")
                    try:
                        fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        fh.close()
                        continue
                    try:
                        if log:
                            with open(log, "a") as lf:
                                lf.write(f"{t0} {int(time.time())} slot{i}\n")
                        yield i
                    finally:
                        fh.close()
                    return
            sleep(self.slot_poll_s)

    def shell(self, cmd: str | list[str], cwd: Path | None = None, env: dict | None = None, log: Path | None = None,
              slot: bool = False, timeout: float | None = None, check: bool = False) -> tuple[int, str]:
        """Run a host-side command (a string runs in bash); returns (exit code, combined output). `log`
        gets the command, its output and its exit; `slot` holds a build slot while it runs."""
        argv = ["bash", "-c", cmd] if isinstance(cmd, str) else list(cmd)
        if self.nice:
            argv = ["nice", "-n", str(self.nice), *argv]
        t0 = time.time()

        def run():
            return subprocess.run(argv, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                  text=True, timeout=timeout)
        if slot:
            with self.slot():
                p = run()
        else:
            p = run()
        if log:
            Path(log).parent.mkdir(parents=True, exist_ok=True)
            shown = cmd if isinstance(cmd, str) else shlex.join(cmd)
            with open(log, "a") as f:
                f.write(f"$ {shown}\n{p.stdout}\n[exit {p.returncode}, {time.time() - t0:.0f}s]\n")
        if check and p.returncode != 0:
            raise RuntimeError(f"command failed ({p.returncode}): {cmd}\n{p.stdout[-3000:]}")
        return p.returncode, p.stdout
