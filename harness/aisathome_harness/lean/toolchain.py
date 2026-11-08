# SPDX-License-Identifier: Apache-2.0
"""A Lean toolchain (elan, Lake) for host-side builds and for the model's container.

`LeanToolchain` holds where elan lives and how Lake caches: `host_env()` is the environment for the
project's own builds on the host (Host.shell), `recipe_parts(tree)` what a sandbox Recipe needs for a
tree: elan mounted read-only at the same path, the tree's package farm read-only, the Lake cache and the
tree's build directory as per-session overlays (the model's writes never reach the shared files), and
`lake` run through a build slot.
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from ..sandbox.container import Mount


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
