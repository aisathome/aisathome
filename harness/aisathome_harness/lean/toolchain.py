# SPDX-License-Identifier: Apache-2.0
"""A Lean toolchain (elan, Lake) for host-side builds and for the model's container.

`LeanToolchain` holds where elan lives and how Lake caches: `host_env()` is the environment for the
project's own builds on the host (Host.shell), `recipe_parts(tree)` what a sandbox Recipe needs for a
tree: elan mounted read-only at the same path, the tree's package farm read-only, the Lake cache and the
tree's build directory as per-session overlays (the model's writes never reach the shared files), and
`lake` run through a build slot.

`jail(image, ws)` is the throwaway container (sandbox.jail) for a build or check of text a model wrote: elan at
JAIL_ELAN (read-only), the workspace at JAIL_WS with its package farm read-only over it, only Lean's own variables in
the environment, no network. `open_build_dirs(ws)` lets the jail's uid write the workspace's `.lake` (not its
packages, not its sources): run it before a jailed build. Host-side builds with `host_env()` are for text nobody
else wrote (the environment's own smoke test, a warm-up).
"""
from __future__ import annotations

import os
import stat
from dataclasses import dataclass
from pathlib import Path

from .. import safefs
from ..sandbox.container import Mount
from ..sandbox.jail import Jail

JAIL_ELAN = "/opt/elan"      # elan inside a jail: a fixed path, so the host's home need not be traversable
JAIL_WS = "/ws"              # the workspace inside a jail


@dataclass
class LeanToolchain:
    elan_home: Path                    # holds bin/lake, bin/lean and the toolchains
    cache_dir: Path | None = None      # Lake's artifact cache (LAKE_CACHE_DIR); None: Lake's default
    artifact_cache: bool = False       # use and fill Lake's artifact cache
    threads: int = 8

    def env(self) -> dict:
        e = {"ELAN_HOME": str(self.elan_home), "LAKE_ARTIFACT_CACHE": str(self.artifact_cache).lower(),
             "LAKE_RESTORE_ARTIFACTS": str(self.artifact_cache).lower(), "LAKE_NO_CACHE": "true",
             "LEAN_NUM_THREADS": str(self.threads)}
        if self.cache_dir:
            e["LAKE_CACHE_DIR"] = str(self.cache_dir)
        return e

    def host_env(self, base: dict | None = None, **extra) -> dict:
        e = dict(os.environ if base is None else base)
        e.update(self.env())
        e["PATH"] = f"{self.elan_home}/bin:" + e.get("PATH", "/usr/bin:/bin")
        e.update(extra)
        return e

    def recipe_parts(self, tree: Path) -> dict:
        """Recipe.merged(**parts) for a session on `tree`."""
        tree = Path(tree)
        if self.cache_dir:
            Path(self.cache_dir).mkdir(parents=True, exist_ok=True)
        overlays = [(tree / ".lake" / "build", tree / ".lake" / "build")]
        if self.cache_dir:
            overlays.insert(0, (Path(self.cache_dir), Path(self.cache_dir)))
        return {"mounts": [Mount(self.elan_home, self.elan_home),
                           Mount(tree / ".lake" / "packages", tree / ".lake" / "packages")],
                "env": self.env(), "path": [f"{self.elan_home}/bin"], "overlays": overlays,
                "slot_commands": {"lake": f"{self.elan_home}/bin/lake"}}

    def jail_env(self) -> dict:
        """Lean's environment inside a jail: elan, Lake's settings without a cache directory, HOME on the tmpfs."""
        e = {k: v for k, v in self.env().items() if k != "LAKE_CACHE_DIR"}
        e.update(ELAN_HOME=JAIL_ELAN, HOME="/tmp", PATH=f"{JAIL_ELAN}/bin:/usr/local/bin:/usr/bin:/bin",
                 # Lake asks git for each package's URL; the farm belongs to the host's user, and git refuses a
                 # repository another uid owns (Lake then takes the URL as changed and tries to clone again)
                 GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="safe.directory", GIT_CONFIG_VALUE_0="*")
        return e

    def jail(self, image: str, ws: Path, writable: bool = True, mounts: list[Mount] | tuple = (), **limits) -> Jail:
        """A jail for Lake or Lean in workspace `ws` (at JAIL_WS; read-only unless `writable`, and its .lake/packages
        read-only either way), elan read-only, `mounts` besides. `limits`: Jail's (memory_gb, pids, cpus, cpu_s,
        user, ...)."""
        ws = Path(ws).resolve()
        ms = [Mount(Path(self.elan_home).resolve(), JAIL_ELAN), Mount(ws, JAIL_WS, ro=not writable)]
        pk = packages_dir(ws)
        if pk is not None:
            ms.append(Mount(pk, f"{JAIL_WS}/.lake/packages"))
        return Jail(image, mounts=ms + list(mounts), env=self.jail_env(), workdir=JAIL_WS, **limits)


def packages_dir(ws: Path) -> Path | None:
    """The workspace's package farm as a bind source: ws/.lake/packages reached without a symlink anywhere below the
    workspace (UnsafePath otherwise: Docker would follow it to a host directory), or None when there is none. The
    workspace itself is the caller's (trusted, resolved)."""
    ws = Path(ws).resolve()
    if safefs.is_dir(ws, ".lake/packages"):              # raises UnsafePath on a symlink on the way
        return ws / ".lake" / "packages"
    if os.path.lexists(ws / ".lake") and not safefs.is_dir(ws, ".lake") or os.path.lexists(ws / ".lake" / "packages"):
        raise safefs.UnsafePath(f"{ws}/.lake/packages is not a directory")
    return None


def open_build_dirs(ws: Path):
    """Let a jail's uid write a workspace's build state: `.lake`, and everything under `.lake/build` and Lake's
    configuration cache `.lake/config`, become writable for everyone, nothing else (the sources, the Lake files and
    the package farm, hard links into a shared set, stay the host user's). Symlinks are neither followed nor
    changed: a symlink at `.lake`, `.lake/build` or `.lake/config` (or anywhere on the way) is an error
    (safefs.UnsafePath), and below them every entry is opened without following one before its mode changes."""
    ws = Path(ws).resolve()
    for sub in ("build", "config"):
        safefs.mkdirs(ws, f".lake/{sub}")                # each component created or opened without a symlink
    _open(str(ws / ".lake"), 0o777, directory=True)
    for sub in ("build", "config"):
        for root, dirs, files in os.walk(ws / ".lake" / sub):     # never descends through a symlinked directory
            _open(root, 0o777, directory=True)
            for n in files:
                _open(os.path.join(root, n), 0o666)


def _open(p: str, mode: int, directory: bool = False):
    """Set the mode of an entry the invoking user owns (what a jail wrote is its uid's, created with umask 0 already),
    through a descriptor opened without following a symlink (a symlink is left alone); a file keeps its execute bits
    (Lake's built executables)."""
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0) | (os.O_DIRECTORY if directory else 0)
    try:
        fd = os.open(p, flags)
    except OSError:                                      # a symlink (ELOOP), a vanished entry, an unreadable one
        if directory and os.path.islink(p):
            raise safefs.UnsafePath(f"{p} is a symlink")
        return
    try:
        st = os.fstat(fd)
        if st.st_uid != os.getuid() or not (stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode)):
            return
        want = mode | (st.st_mode & 0o111 if stat.S_ISREG(st.st_mode) else 0)
        if stat.S_IMODE(st.st_mode) != want:
            os.fchmod(fd, want)
    finally:
        os.close(fd)
