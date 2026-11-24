# SPDX-License-Identifier: Apache-2.0
"""A throwaway container for one host-side command over text nobody vouches for: a project's own build or check
of what a model wrote (Lean compiles and runs code while it elaborates a file: an elaborator, macro or `initialize`
registered by the file itself runs inside the compiler).

`Jail(image, mounts, env).run(argv)` runs `argv` in `docker run --rm` with:

- no network (`--network none`), a read-only root filesystem, a writable tmpfs at /tmp only;
- every capability dropped, no new privileges, Docker's default seccomp profile, `--init`;
- a uid:gid that is neither root nor the invoking user (JAIL_USER by default): the container can write only what the
  caller opened to that uid or to everyone, and owns nothing on the host;
- memory (no swap), process, CPU, CPU-time (RLIMIT_CPU per process), file-size and core-dump limits;
- no environment from the caller: only `env` (and HOME=/tmp unless `env` sets it);
- the mounts the caller lists (read-only unless a mount says otherwise), never the caller's home or secrets;
- its output read through a size cap (`output_limit`); past the cap the rest is dropped and counted.

The caller reads back the exit code and the capped output, and whatever the container wrote into a writable mount
(treat it as untrusted: never follow a symlink in it). On a timeout the container is killed by its name and
`subprocess.TimeoutExpired` is raised, as `subprocess.run` does. Standard library only (Docker's CLI).
"""
from __future__ import annotations

import os
import secrets
import shlex
import subprocess
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path

from .container import Mount
from .sandbox import HARDEN, PREFIX

JAIL_UID = 64000            # owns nothing on a runner; never the invoking user (refused) or root
JAIL_USER = f"{JAIL_UID}:{JAIL_UID}"
LABEL = "harness.jail"


def _uid(user: str) -> int | None:
    head = user.split(":")[0]
    return int(head) if head.isdigit() else None


@dataclass
class Jail:
    image: str                                   # any image with /bin/sh and what `argv` needs (e.g. a glibc)
    mounts: list[Mount] = field(default_factory=list)
    env: dict = field(default_factory=dict)
    workdir: str = "/tmp"
    user: str = JAIL_USER
    memory_gb: float = 8
    pids: int = 512
    cpus: float | None = None
    cpu_s: int | None = None                     # RLIMIT_CPU of every process (seconds); None: no limit
    tmp_mb: int = 1024
    fsize_mb: int = 4096                         # largest file a process may write
    output_limit: int = 4 << 20                  # bytes of output kept
    umask: str = "0000"                          # what the container writes stays removable by the host's user
    name_prefix: str = f"{PREFIX}-jail"

    def __post_init__(self):
        uid = _uid(self.user)
        if uid is None or uid == 0 or self.user.split(":")[0] == "root":
            raise ValueError(f"a jail runs as a numeric non-root uid, not {self.user!r}")
        if uid == os.getuid():
            raise ValueError(f"a jail never runs as the invoking user (uid {uid}): it could write that user's files")
        for m in self.mounts:
            for p in (str(m.source), str(m.target)):
                if "," in p or "=" in p:
                    raise ValueError(f"mount path {p!r} contains ',' or '='")

    def args(self, argv: list[str], name: str) -> list[str]:
        """The `docker run` command line for `argv` in a container named `name`."""
        lim = ["--memory", f"{self.memory_gb}g", "--memory-swap", f"{self.memory_gb}g",
               "--pids-limit", str(self.pids), "--ulimit", "core=0",
               "--ulimit", f"fsize={self.fsize_mb << 20}:{self.fsize_mb << 20}"]
        if self.cpus:
            lim += ["--cpus", str(self.cpus)]
        if self.cpu_s:
            lim += ["--ulimit", f"cpu={int(self.cpu_s)}:{int(self.cpu_s)}"]
        vols = []
        for m in self.mounts:
            vols += ["--mount", f"type=bind,src={m.source},dst={m.target}" + (",readonly" if m.ro else "")]
        env = {"HOME": "/tmp", **self.env}
        envs = sum((["-e", f"{k}={v}"] for k, v in env.items()), [])
        return (["docker", "run", "--rm", "--name", name, "--label", f"{LABEL}=1", "--network", "none",
                 "-u", self.user, *HARDEN, "--init", "--read-only",
                 "--tmpfs", f"/tmp:rw,nosuid,nodev,size={self.tmp_mb}m,mode=1777", *lim, *vols, *envs,
                 "-w", self.workdir, "--entrypoint", "/bin/sh", self.image,
                 "-c", f'umask {self.umask} && exec "$@"', "jail", *argv])

    def run(self, argv: list[str], timeout: float | None = None, log: Path | None = None) -> tuple[int, str]:
        """(exit code, output: stdout and stderr together, capped). `log` gets the command, the output and the exit,
        as Host.shell writes it. Raises subprocess.TimeoutExpired after killing the container."""
        name = f"{self.name_prefix}-{secrets.token_hex(6)}"
        cmd = self.args(list(argv), name)
        t0 = time.time()
        p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        buf, dropped = bytearray(), [0]

        def read():
            while True:
                chunk = p.stdout.read(65536)
                if not chunk:
                    return
                room = self.output_limit - len(buf)
                if room > 0:
                    buf.extend(chunk[:room])
                dropped[0] += max(0, len(chunk) - max(room, 0))

        reader = threading.Thread(target=read, daemon=True)
        reader.start()
        try:
            rc = p.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            kill(name)
            p.wait()
            reader.join(5)
            out = _text(buf, dropped[0])
            _log(log, argv, out, f"timed out after {timeout:.0f}s", t0)
            raise subprocess.TimeoutExpired(cmd, timeout, output=out)
        reader.join()
        out = _text(buf, dropped[0])
        _log(log, argv, out, f"exit {rc}", t0)
        return rc, out


def _text(buf: bytearray, dropped: int) -> str:
    out = buf.decode("utf-8", errors="replace")
    return out + (f"\n[output cut: {dropped} more bytes]\n" if dropped else "")


def _log(log: Path | None, argv: list[str], out: str, end: str, t0: float):
    if not log:
        return
    Path(log).parent.mkdir(parents=True, exist_ok=True)
    with open(log, "a") as f:
        f.write(f"$ {shlex.join(argv)}\n{out}\n[{end}, {time.time() - t0:.0f}s]\n")


def kill(name: str):
    """Stop and remove a jail container (by its name), whether or not it still runs."""
    subprocess.run(["docker", "rm", "-f", name], capture_output=True)


def sweep():
    """Remove every jail container left on this host (e.g. by a worker killed while one ran)."""
    p = subprocess.run(["docker", "ps", "-aq", "--filter", f"label={LABEL}"], capture_output=True, text=True)
    for cid in p.stdout.split():
        subprocess.run(["docker", "rm", "-f", cid], capture_output=True)
