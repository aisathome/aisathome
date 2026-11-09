# SPDX-License-Identifier: Apache-2.0
"""Host-side git on a tree the model controls, without running anything the tree controls.

The model can write the tree's `.git`: its config (filters, fsmonitor, textconv, external diff, includes),
its hooks, its attributes, its refs and its objects. Git run on the host in that repository would execute
the model's commands outside the sandbox. So the host never uses the tree's `.git` at all:

- `TreeGit` keeps its own git directory in a host-only state directory and points it at the tree as a
  work tree (`git --git-dir=<ours> --work-tree=<tree>`).
- `init_base()` snapshots the tree as the base commit, at a moment the project trusts: right after it
  prepared the tree, before the model's first step.
- `diff()` (the model's edits as a binary patch) and `changed()` (which paths differ) compare the tree with
  that base, through a temporary index of ours.
- Git runs with no system or global config (`GIT_CONFIG_NOSYSTEM`, `GIT_CONFIG_GLOBAL=/dev/null`, HOME in
  the state directory), hooks off, fsmonitor off, no external diff or textconv. The tree's own
  `.gitattributes` are still read by git, but every filter, diff or merge driver they could name is
  undefined in our config, so they cannot run anything.

Diffs are against the base, not the tree's HEAD: a model that commits its work cannot hide it from the
checkpoint. Paths ignored by the tree's `.gitignore` (build outputs) stay out, as before.
"""
from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

HARDEN = ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null",
          "-c", "core.untrackedCache=false", "-c", "diff.external=", "-c", "core.sshCommand=false",
          "-c", "protocol.allow=never", "-c", "core.symlinks=true", "-c", "core.quotePath=false"]
BASE_REF = "refs/harness/base"


class TreeGit:
    def __init__(self, tree: Path, state: Path):
        self.tree, self.state = Path(tree), Path(state)
        self.gitdir = self.state / "git"

    def _env(self, index: Path | None = None) -> dict:
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(self.state), "LC_ALL": "C",
               "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_DIR": str(self.gitdir),
               "GIT_WORK_TREE": str(self.tree), "GIT_AUTHOR_NAME": "harness", "GIT_AUTHOR_EMAIL": "harness@localhost",
               "GIT_COMMITTER_NAME": "harness", "GIT_COMMITTER_EMAIL": "harness@localhost",
               "GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z",
               "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"}
        if index is not None:
            env["GIT_INDEX_FILE"] = str(index)
        return env

    def _git(self, *args: str, index: Path | None = None, text: bool = True) -> subprocess.CompletedProcess:
        p = subprocess.run(["git", *HARDEN, *args], cwd=self.tree, env=self._env(index),
                           capture_output=True, text=text)
        if p.returncode != 0:
            raise subprocess.CalledProcessError(p.returncode, ["git", *args], p.stdout, p.stderr)
        return p

    def has_base(self) -> bool:
        if not (self.gitdir / "HEAD").exists():
            return False
        return subprocess.run(["git", *HARDEN, "rev-parse", "-q", "--verify", BASE_REF], cwd=self.gitdir,
                              env=self._env(), capture_output=True).returncode == 0

    def init_base(self) -> str:
        """Snapshot the tree as the base (call it before the model has run). Returns the commit id."""
        if self.gitdir.exists():
            shutil.rmtree(self.gitdir)
        self.state.mkdir(parents=True, exist_ok=True)
        subprocess.run(["git", *HARDEN, "init", "-q", "--bare", str(self.gitdir)], cwd=self.state,
                       env={k: v for k, v in self._env().items() if k not in ("GIT_DIR", "GIT_WORK_TREE")},
                       capture_output=True, check=True)
        subprocess.run(["git", "config", "core.bare", "false"], cwd=self.gitdir, env=self._env(),
                       capture_output=True, check=True)
        idx = self.state / f"base-{os.getpid()}.index"
        try:
            self._git("add", "-A", "--", ".", index=idx)
            tree = self._git("write-tree", index=idx).stdout.strip()
        finally:
            idx.unlink(missing_ok=True)
        commit = self._git("commit-tree", tree, "-m", "base").stdout.strip()
        self._git("update-ref", BASE_REF, commit)
        self._git("update-ref", "--no-deref", "HEAD", commit)     # status compares with the base
        return commit

    def _with_index(self, fn):
        idx = self.state / f"tmp-{os.getpid()}.index"
        try:
            self._git("read-tree", BASE_REF, index=idx)
            return fn(idx)
        finally:
            idx.unlink(missing_ok=True)

    def diff(self) -> bytes:
        """The tree against the base as a binary patch (`git apply` takes it), new files included. Bytes,
        exactly as git wrote them: decoding as text would turn a CRLF file's line ends into LF, and the
        patch would no longer apply to the base."""
        def run(idx):
            self._git("add", "-A", "--", ".", index=idx)
            return self._git("diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", BASE_REF,
                             index=idx, text=False).stdout
        return self._with_index(run)

    def changed(self, paths: list[str] = ()) -> list[str]:
        """Paths (relative to the tree) that differ from the base, new files included; limited to `paths`."""
        def run(idx):
            out = self._git("status", "--porcelain", "-uall", "--no-renames", "--", *(paths or ["."]), index=idx).stdout
            return [ln[3:].strip().strip('"') for ln in out.splitlines() if ln.strip()]
        return self._with_index(run)
