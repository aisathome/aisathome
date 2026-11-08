# SPDX-License-Identifier: Apache-2.0
"""Lake package sets: one directory per set of package revisions (Mathlib and friends), so tasks on
projects or commits that pin different Mathlib revisions can run side by side without a full copy each.

    <store>/<key>/        lakefile.toml or lakefile.lean, lake-manifest.json, lean-toolchain
                          .lake/packages      Mathlib's published oleans, completed by a build
                          set.json            written last: the set is complete

key = <mathlib rev[:12]>-<sha256 of the sorted (package, rev) list>[:8]. A task's tree takes its set as
a hard-link farm (`cp -al`); the sandbox mounts the tree's packages read-only.

No duplicates: a new set is built in full (`lake exe cache get`, then `lake build Mathlib` while it is
still writable, since the cache leaves files Lake still writes), then every file that is byte-identical
to the same path in another complete set becomes a hard link to it (`dedup`). Between two nearby Mathlib
revisions that leaves a small fraction of a set's size; `measure` says how much for two given sets.
Mathlib's download cache (*.ltar) goes to a private directory that is emptied after a build.

Which sets to keep is the project's policy; `prune` removes the others. Standard library only.
"""
from __future__ import annotations

import fcntl
import filecmp
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Callable

LAKE_FILES = ("lakefile.toml", "lakefile.lean", "lake-manifest.json", "lean-toolchain")
MATHLIB_OLEAN = Path(".lake/packages/mathlib/.lake/build/lib/lean/Mathlib.olean")
DEFAULT_SET_GB = 7.5     # a Mathlib set unpacked in full, before the dedup gives most of it back


# ----------------------------------------------------------------------------- keys

def revs(manifest: str) -> list[tuple[str, str | None]]:
    return sorted((p["name"], p.get("rev")) for p in json.loads(manifest)["packages"])


def key(manifest: str) -> str:
    r = revs(manifest)
    mathlib = next((v for n, v in r if n == "mathlib"), None) or "nomathlib"
    return f"{mathlib[:12]}-{hashlib.sha256(json.dumps(r).encode()).hexdigest()[:8]}"


def complete(d: Path) -> bool:
    return (d / "set.json").exists() and (d / MATHLIB_OLEAN).exists()


def find(store: Path, manifest: str) -> Path | None:
    """The complete set whose package revisions equal this manifest's, or None."""
    d = store / key(manifest)
    return d if complete(d) else None


def sets(store: Path) -> list[Path]:
    return sorted(d for d in store.iterdir() if d.is_dir() and not d.name.startswith(".") and complete(d)) \
        if store.exists() else []


@contextmanager
def store_lock(store: Path):
    """The store's lock: one build (and one Mathlib download) at a time; prune holds it too."""
    store.mkdir(parents=True, exist_ok=True)
    with open(store / ".lock", "w") as fh:
        fcntl.flock(fh, fcntl.LOCK_EX)
        yield


# ----------------------------------------------------------------------------- dedup

def _files(root: Path):
    for dirpath, _, names in os.walk(root):
        for n in names:
            p = Path(dirpath) / n
            if p.is_file() and not p.is_symlink():
                yield p


def dedup(new: Path, others: list[Path]) -> dict:
    """Replace each file under new/.lake/packages that is byte-identical to the same path in
    another set by a hard link to it. Only before `new` is in use (nothing writes a set later)."""
    n = saved = 0
    root = new / ".lake" / "packages"
    for p in _files(root):
        rel = p.relative_to(root)
        st = p.stat()
        for o in others:
            q = o / ".lake" / "packages" / rel
            try:
                qs = q.stat()
            except FileNotFoundError:
                continue
            if qs.st_ino == st.st_ino or qs.st_size != st.st_size or not q.is_file():
                continue
            if filecmp.cmp(p, q, shallow=False):
                tmp = p.with_name(p.name + ".pkgsets-link")
                os.link(q, tmp)
                os.replace(tmp, p)
                n += 1
                saved += st.st_size
                break
    return {"files_linked": n, "bytes_linked": saved}


def measure(a: Path, b: Path) -> dict:
    """Read-only: how many of B's bytes a store would not need to keep next to A."""
    total = same_path = 0
    a_hashes = set()
    sha = lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
    for p in _files(a):
        a_hashes.add((p.stat().st_size, sha(p)))
    files = 0
    any_path = 0
    for p in _files(b):
        files += 1
        sz = p.stat().st_size
        total += sz
        q = a / p.relative_to(b)
        if q.is_file() and q.stat().st_size == sz and filecmp.cmp(p, q, shallow=False):
            same_path += sz
        if (sz, sha(p)) in a_hashes:
            any_path += sz
    return {"a": str(a), "b": str(b), "b_files": files, "b_bytes": total,
            "b_bytes_identical_same_path": same_path, "b_bytes_identical_any_path": any_path,
            "b_bytes_new_with_same_path_links": total - same_path,
            "b_bytes_new_with_content_store": total - any_path}


def du_own(d: Path) -> tuple[int, int]:
    """(bytes of files only this set links, bytes it shares with other links)."""
    own = shared = 0
    for p in _files(d):
        st = p.stat()
        if st.st_nlink == 1:
            own += st.st_size
        else:
            shared += st.st_size
    return own, shared


def free_gb(path: Path) -> float:
    return shutil.disk_usage(path).free / 1e9


# ----------------------------------------------------------------------------- build

def run_logged(cmd: str, cwd: Path, log, env: dict | None = None, timeout: int = 3 * 3600) -> int:
    log.write(f"$ {cmd}\n")
    log.flush()
    return subprocess.run(["bash", "-c", cmd], cwd=cwd, stdout=log, stderr=subprocess.STDOUT,
                          env=env, timeout=timeout).returncode


def lake_build(tmp: Path, log, env: dict | None = None, ltar_dir: Path | None = None, tries: int = 3,
               retry_wait_s: float = 30):
    """Fill tmp/.lake/packages for tmp's lakefile and manifest: the toolchain (elan), Mathlib's
    published oleans (`lake exe cache get`, `tries` times), then `lake build Mathlib` while the
    packages are still writable. Mathlib's downloads go to `ltar_dir` (MATHLIB_CACHE_DIR), emptied
    afterwards, so a user's own ~/.cache/mathlib is never touched."""
    ltars = ltar_dir or (tmp.parent / ".ltar-cache")
    env = {**(env if env is not None else os.environ), "MATHLIB_CACHE_DIR": str(ltars)}
    tc = (tmp / "lean-toolchain").read_text().strip()
    lst = subprocess.run(["elan", "toolchain", "list"], capture_output=True, text=True, env=env).stdout
    if tc.split(":")[-1] not in lst:
        if run_logged(f"elan toolchain install {tc}", tmp, log, env) != 0:
            raise RuntimeError(f"pkgsets: elan could not install {tc}")
    for _ in range(tries):
        if run_logged("lake exe cache get", tmp, log, env) == 0:
            break
        time.sleep(retry_wait_s)
    else:
        raise RuntimeError(f"pkgsets: `lake exe cache get` failed {tries} times")
    if run_logged("lake build --iofail Mathlib", tmp, log, env) != 0:
        raise RuntimeError("pkgsets: `lake build Mathlib` failed in the new set")
    shutil.rmtree(ltars, ignore_errors=True)


def build_set(store: Path, manifest: str, fill: Callable[[Path], None], builder: Callable, log,
              env: dict | None = None, meta: dict | None = None) -> Path:
    """Build the set for `manifest` in the store (the caller holds store_lock): `fill(tmp)` writes the
    Lake files into a fresh directory, `builder(tmp, log, env)` fills its packages (lake_build, or a
    test's stand-in), then the dedup against the store's complete sets and set.json. Returns the set."""
    k = key(manifest)
    d = store / k
    tmp = store / f".tmp-{k}"
    if tmp.exists():
        shutil.rmtree(tmp)
    tmp.mkdir(parents=True)
    t0 = time.time()
    fill(tmp)
    builder(tmp, log, env)
    if not (tmp / MATHLIB_OLEAN).exists():
        raise RuntimeError(f"pkgsets: the build of {k} left no Mathlib.olean")
    d_stats = dedup(tmp, sets(store))
    own, _ = du_own(tmp)
    m = {"key": k, **(meta or {}), "revs": revs(manifest), "built_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
         "seconds": round(time.time() - t0), "own_bytes": own, **d_stats}
    (tmp / "set.json").write_text(json.dumps(m, indent=1) + "\n")
    if d.exists():
        shutil.rmtree(d)
    os.rename(tmp, d)
    log.write(f"pkgsets: built {k} in {m['seconds']} s; {own / 1e9:.2f} GB of its own, "
              f"{d_stats['bytes_linked'] / 1e9:.2f} GB linked to other sets\n")
    return d


def adopt(store: Path, workspace: Path) -> Path | None:
    """A workspace's complete packages join the store as hard links: no download, no extra disk.
    None if the store already has the set or the workspace has none."""
    if not (workspace / MATHLIB_OLEAN).exists():
        return None
    manifest = (workspace / "lake-manifest.json").read_text()
    k = key(manifest)
    with store_lock(store):
        d = store / k
        if complete(d):
            return None
        tmp = store / f".tmp-{k}"
        if tmp.exists():
            shutil.rmtree(tmp)
        tmp.mkdir(parents=True)
        for f in LAKE_FILES:
            if (workspace / f).exists():
                shutil.copy(workspace / f, tmp / f)
        (tmp / ".lake").mkdir()
        subprocess.run(["cp", "-al", str(workspace / ".lake" / "packages"), str(tmp / ".lake" / "packages")], check=True)
        (tmp / "set.json").write_text(json.dumps({"key": k, "revs": revs(manifest), "adopted_from": str(workspace),
                                                  "built_at": time.strftime("%Y-%m-%dT%H:%M:%S%z")}, indent=1) + "\n")
        if d.exists():
            shutil.rmtree(d)
        os.rename(tmp, d)
        return d


def link_packages(pset: Path, tree: Path, run: Callable[[str], object] | None = None):
    """The tree's .lake/packages as a hard-link farm of the package set (no extra disk); a tree linked
    to another set is relinked. `run(cmd)` runs the copy (default: subprocess)."""
    pk, mark = tree / ".lake" / "packages", tree / ".lake" / "pkgset"
    if pk.exists() and (not mark.exists() or mark.read_text().strip() != pset.name):
        shutil.rmtree(pk)       # a tree without a mark is relinked once
    if not pk.exists():
        pk.parent.mkdir(parents=True, exist_ok=True)
        cmd = f"cp -al {pset / '.lake' / 'packages'} {pk}"
        if run:
            run(cmd)
        else:
            subprocess.run(["cp", "-al", str(pset / ".lake" / "packages"), str(pk)], check=True)
    mark.write_text(pset.name + "\n")


def prune(store: Path, keep: set[str] | dict, dry: bool = False, out=sys.stdout) -> list[str]:
    """Remove every complete set whose key is not in `keep` and any interrupted build; the caller holds
    store_lock. A set a tree still links stays readable for that tree (hard links); its disk comes back
    when the tree lets go."""
    gone = []
    for d in sets(store):
        if d.name in keep:
            continue
        own, _ = du_own(d)
        out.write(f"pkgsets: {'would prune' if dry else 'pruning'} {d.name} ({own / 1e9:.2f} GB of its own)\n")
        if not dry:
            shutil.rmtree(d)
        gone.append(d.name)
    if not dry and store.exists():
        for t in store.glob(".tmp-*"):          # an interrupted build (the caller holds the store lock)
            shutil.rmtree(t, ignore_errors=True)
    return gone
