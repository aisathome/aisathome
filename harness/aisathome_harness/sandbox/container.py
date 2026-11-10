# SPDX-License-Identifier: Apache-2.0
"""The model's container for one session: what a project needs inside it (`Recipe`) and the per-session
object the session loop drives (`Sandbox`).

A `Recipe` is per project: the image (with opencode as its entrypoint and the project's toolchain), extra
read-only mounts (a toolchain), environment, PATH entries, names to sinkhole, shared caches to overlay,
commands that take a build slot, and a knowledge folder mounted read-only at ~/knowledge.

A `Sandbox` is per session. Everything that could collide between sessions on one host is derived from
its `key` (unique per session on the host, e.g. "<project>/<task>"): the container name, the session's
internal network (sandbox.session_network), the overlay volumes; everything it writes on the host is under
its `state` directory (host-only), never in the model's HOME or tree. The model's container gets:

    /home/sbx                 the session's HOME (rw; opencode's config and database live here)
    <tree>                    the task's tree at the same path (rw)
    /run/harness/bin          wrappers that run slot commands through a build slot (ro, generated in state)
    /run/harness/slots        the host's build-slot lock files (ro: flock needs only a read descriptor, and
                              the model can neither replace a lock file nor plant a link the host follows)
    /run/harness/buildslot.sh the slot script (ro)
    ~/knowledge               the project's knowledge folder (ro), if any
    the recipe's mounts and overlays

The endpoint's API key is never in the model's container: opencode talks plain HTTP to the session's egress
forwarder (`model_url`), which holds the key and adds it to each request (sandbox.ensure_egress).

Standard library only (Docker is driven through its CLI).
"""
from __future__ import annotations

import hashlib
import os
import shlex
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

from .. import loop, safefs
from ..host import Host
from . import sandbox

HOME = "/home/sbx"
RUN = "/run/harness"
BUILDSLOT = Path(__file__).resolve().parent / "buildslot.sh"


@dataclass
class Mount:
    source: Path
    target: Path | str
    ro: bool = True

    def args(self) -> list[str]:
        return ["-v", f"{self.source}:{self.target}{':ro' if self.ro else ''}"]


@dataclass
class Recipe:
    image: str                                         # opencode as entrypoint, plus the project's toolchain
    # uid:gid the model's container runs as: by default the invoking user's, who owns the tree and HOME the
    # project prepared (a fixed 1000 could not write them on a host whose user is not 1000). Never root.
    user: str = field(default_factory=lambda: f"{os.getuid()}:{os.getgid()}")
    mounts: list[Mount] = field(default_factory=list)
    env: dict = field(default_factory=dict)
    path: list[str] = field(default_factory=list)      # prepended to PATH, after the slot wrappers
    sinkhole: list[str] = field(default_factory=list)  # names that resolve to nowhere inside
    overlays: list[tuple[Path, Path]] = field(default_factory=list)   # (shared lower dir, target in the container)
    slot_commands: dict[str, str] = field(default_factory=dict)       # name -> real path inside the container
    knowledge: Path | None = None
    forwarder_image: str | None = None                 # runs the egress forwarder (python3, CA certificates); default: image

    def merged(self, **parts) -> "Recipe":
        """A copy with lists and dicts extended (e.g. by a toolchain's parts)."""
        r = Recipe(**{**self.__dict__})
        r.mounts = [*self.mounts, *parts.get("mounts", [])]
        r.env = {**self.env, **parts.get("env", {})}
        r.path = [*self.path, *parts.get("path", [])]
        r.sinkhole = [*self.sinkhole, *parts.get("sinkhole", [])]
        r.overlays = [*self.overlays, *parts.get("overlays", [])]
        r.slot_commands = {**self.slot_commands, **parts.get("slot_commands", {})}
        return r


def container_name(key: str) -> str:
    return f"{sandbox.PREFIX}-{sandbox.slug(key, 40)}-{hashlib.sha256(key.encode()).hexdigest()[:8]}"


class Sandbox:
    """One session's container on one host. Implements what loop.Session takes from a sandbox: command,
    before, stop, export, close, and model_url (the endpoint as the model's client sees it)."""

    def __init__(self, recipe: Recipe, host: Host, key: str, tree: Path, home: Path, state: Path,
                 base_url: str, key_file: str | None, model: str, provider: "loop.Provider | None" = None):
        if recipe.user.split(":")[0] in ("0", "root"):
            raise ValueError("the model's container never runs as root: run the harness as an ordinary user, "
                             "or set Recipe.user to one that owns the tree and HOME")
        self.recipe, self.host, self.key = recipe, host, key
        self.tree, self.home, self.state = Path(tree), Path(home), Path(state)
        self.base_url, self.key_file, self.model = base_url, key_file, model
        self.provider = provider or loop.DEFAULT_PROVIDER
        self.name = container_name(key)
        self.network = sandbox.session_network(key)[0]

    # --- what the container needs on the host
    def volumes(self) -> list[str]:
        return [f"{self.name}-ov{i}" for i in range(len(self.recipe.overlays))]

    def _bin(self) -> Path:
        """Host-only, read-only inside: wrappers that run a command through a build slot."""
        d = self.state / "bin"
        d.mkdir(parents=True, exist_ok=True)
        for name, real in self.recipe.slot_commands.items():
            (d / name).write_text(
                "#!/bin/sh\n"
                "# Builds take one of the host's build slots; nested calls skip the slot.\n"
                f"[ -n \"${{HARNESS_SLOT_HELD:-}}\" ] && exec {shlex.quote(real)} \"$@\"\n"
                f"export HARNESS_SLOT_HELD=1 BUILDSLOT_LOG=$HOME/{loop.LOCKWAIT}\n"
                f"exec sh {RUN}/buildslot.sh {RUN}/slots {shlex.quote(real)} \"$@\"\n")
            (d / name).chmod(0o755)
        return d

    def _source(self, path: Path | str, create: bool = False) -> Path:
        """A mount source that Docker may use. Docker (and the overlay driver, as root) follow symlinks, so
        a source inside the model's tree or HOME is checked component by component without following any
        (safefs), and created that way when `create`; a source anywhere else must not resolve into them.
        Anything else is refused (UnsafePath): the session does not start."""
        path = Path(path)
        for root in (self.tree, self.home):
            if path == root:
                return path
            if root in path.parents:
                rel = path.relative_to(root)
                if create:
                    safefs.mkdirs(root, rel)
                elif not safefs.is_dir(root, rel):
                    raise safefs.UnsafePath(f"mount source {path} is missing or not a directory")
                return path
        real = path.resolve()
        for root in (self.tree, self.home):
            r = root.resolve()
            if real == r or r in real.parents:
                raise safefs.UnsafePath(f"mount source {path} resolves into the model's {root}")
        return path

    def _mounts(self) -> list[str]:
        r = self.recipe
        # every source checked before anything is created: a refusal leaves no volume behind
        mounts = [(self._source(m.source), m) for m in r.mounts]
        lowers = [(self._source(lower, create=True), target) for lower, target in r.overlays]
        v = ["-v", f"{self.home}:{HOME}", "-v", f"{self.tree}:{self.tree}"]
        for src, m in mounts:
            v += ["-v", f"{src}:{m.target}{':ro' if m.ro else ''}"]
        for i, (lower, target) in enumerate(lowers):
            v += sandbox.overlay_mount(self.volumes()[i], lower, self.state / "overlay" / str(i), Path(target))
        if r.slot_commands:
            v += ["-v", f"{self._bin()}:{RUN}/bin:ro", "-v", f"{self.host.slot_dir}:{RUN}/slots:ro",
                  "-v", f"{BUILDSLOT}:{RUN}/buildslot.sh:ro"]
        if r.knowledge and Path(r.knowledge).is_dir():
            safefs.mkdirs(self.home, "knowledge")       # the mount point, inside the model's HOME
            v += ["-v", f"{self._source(r.knowledge)}:{HOME}/knowledge:ro"]
        return v

    def _env(self) -> list[str]:
        r = self.recipe
        path = ([f"{RUN}/bin"] if r.slot_commands else []) + r.path + \
            ["/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"]
        env = {"HOME": HOME, "BUILD_MIN_FREE_GB": str(self.host.build_min_free_gb), **r.env, "PATH": ":".join(path)}
        return sum((["-e", f"{k}={v}"] for k, v in env.items()), [])

    def _key_file(self) -> str | None:
        """The key file for the forwarder's mount: a file outside the model's tree and HOME, or refused."""
        if not self.key_file:
            return None
        key = Path(self.key_file).resolve()
        self._source(key)
        if not key.is_file():
            raise safefs.UnsafePath(f"key file {self.key_file} is missing or not a file")
        return str(key)

    # --- the loop's interface
    def model_url(self, base_url: str) -> str:
        """The endpoint's URL in opencode's config: plain HTTP to the forwarder, which adds the key."""
        return sandbox.model_url(base_url)

    def command(self, message: str, session_id: str | None) -> list[str]:
        """`docker run` for one segment: the session's network and forwarder, hardened, with limits."""
        self.before()
        sandbox.ensure_session_network(self.key)
        hosts = sandbox.sinkhole(self.recipe.sinkhole) + sandbox.ensure_egress(
            self.base_url, self.recipe.forwarder_image or self.recipe.image, self.network, self._key_file())
        return sandbox.run_args(
            self.name, self.recipe.image, self.tree, loop.opencode_args(self.model, session_id, self.provider) + [message],
            network=self.network, user=self.recipe.user, cpu=["--cpus", str(self.host.cpus), "--cpu-shares", "128"],
            limit=sandbox.limits(self.host.limits()), hosts=hosts, volumes=self._mounts(), env=self._env())

    def before(self):
        """Remove a leftover container of this session (it would hold the overlay volumes, and could still
        change the tree). Raises if it is still there afterwards: nothing may read or run on that tree then."""
        subprocess.run(["docker", "rm", "-f", self.name], capture_output=True)
        left = subprocess.run(["docker", "container", "inspect", "--format", "{{.Id}}", self.name],
                              capture_output=True, text=True)
        if left.returncode == 0:
            raise RuntimeError(f"container {self.name} could not be removed: stop it before the session goes on")
        err = (left.stderr or "").lower()
        if not any(f"no such {kind}: {self.name.lower()}" in err for kind in ("container", "object")):
            # Docker did not say that this container is absent (its daemon or context is unavailable?)
            raise RuntimeError(f"cannot confirm that container {self.name} is gone: {(left.stderr or '').strip()[:200]}")

    def stop(self):
        subprocess.run(["docker", "kill", self.name], capture_output=True)

    def export(self, session_id: str) -> str:
        """opencode's session export, in a container without network; the export, or opencode's error output."""
        p = subprocess.run(["docker", "run", "--rm", "-u", self.recipe.user, "--network", "none", *sandbox.HARDEN,
                            "-v", f"{self.home}:{HOME}", "-e", f"HOME={HOME}", "-w", str(self.tree),
                            "-v", f"{self.tree}:{self.tree}:ro", self.recipe.image,
                            "session", "export", "--standalone", session_id], capture_output=True, text=True)
        return p.stdout if p.returncode == 0 else p.stderr

    def close(self):
        """After the session: its network goes; the overlay volumes go (their files stay in state, so a
        resume sees the session's writes again)."""
        self.before()
        sandbox.remove_session_network(self.key)
        sandbox.drop_overlays(self.volumes())

    def drop(self):
        """Everything of the session on the host: container, network, volumes and their files."""
        self.close()
        sandbox.drop_overlays([], self.state / "overlay")
