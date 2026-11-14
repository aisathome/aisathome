# SPDX-License-Identifier: Apache-2.0
"""Task checkpoints and evidence in a store, so that any worker on any host resumes a task: the state a task
needs to go on elsewhere, packed into one archive with a manifest, uploaded after every round and when the
task is given back, and restored on another machine.

The project says where its state lives (`Layout`: named parts, each a directory on this host); nothing here
knows a project's own layout. A store is any object with six calls (`DirStore` here, `aws.S3Store` for S3):

    put(key, data: bytes)   put_file(key, path)   get(key) -> bytes   get_file(key, path)
    copy(src_key, dst_key)  list(prefix) -> [keys]

each raising `StoreError` when the store refuses or cannot answer. Existence is always asked with a prefix
listing (a role that may only Get/Put/List-by-prefix reads a missing key as 403, not 404).

A checkpoint under `prefix` (the project's choice, e.g. ckpt/<trial>/<task>/):

    <prefix>latest.tar.zst + latest.json        the newest
    <prefix>previous.tar.zst + previous.json    the one before (copied before each upload, if intact)

The archive holds each part under its name (a part's `exclude` paths left out, symbolic links kept as links);
the manifest: v, task, the project's `meta`, seq (one more than the newest anywhere), why, created, sha256 and
size of the archive, work, worker. The archive goes up before its manifest; a restore checks the sha256 and
falls back to `previous` when `latest` does not match (an upload cut between the two), and fails with
StoreError when neither is intact. Each host notes the seq it holds (<notes>/ckpt/<task>.json): a restore
runs only when the store's seq is newer than the local one, or the local copy is incomplete (`Layout.needs`).

Evidence: a directory to <prefix><path> in the store, changed files only, with <prefix>_index.json last
({...meta, files: {path: {sha256, size}}, synced}); `fetch_evidence` brings it back, checking every file's
sha256 against the index.

Needs `zstd` and GNU tar (the restore unpacks with `tar --zstd`). Standard library only.
"""
from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
import tarfile
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

MANIFEST_V = 1


class StoreError(Exception):
    """A request the store refused or could not answer (after its retries)."""


# ----------------------------------------------------------------------------- a directory as the store

class DirStore:
    """A directory standing in for a bucket (tests, dry runs, one host): the six calls. Each call is appended
    to `calls` (op, key), so a test can see what went up."""

    def __init__(self, root: Path):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.calls: list[tuple[str, str]] = []

    def _p(self, key: str) -> Path:
        if key.startswith("/") or ".." in key.split("/"):
            raise StoreError(f"bad key {key!r}")
        return self.root / key

    def put_file(self, key: str, path: Path):
        self.calls.append(("put", key))
        p = self._p(key)
        p.parent.mkdir(parents=True, exist_ok=True)
        tmp = p.with_name(p.name + ".part")
        shutil.copyfile(path, tmp)
        tmp.replace(p)

    def put(self, key: str, data: bytes):
        self.calls.append(("put", key))
        p = self._p(key)
        p.parent.mkdir(parents=True, exist_ok=True)
        tmp = p.with_name(p.name + ".part")
        tmp.write_bytes(data)
        tmp.replace(p)

    def get(self, key: str) -> bytes:
        self.calls.append(("get", key))
        try:
            return self._p(key).read_bytes()
        except FileNotFoundError:
            raise StoreError(f"{key}: not found") from None

    def get_file(self, key: str, path: Path):
        Path(path).write_bytes(self.get(key))

    def copy(self, src: str, dst: str):
        self.calls.append(("copy", dst))
        try:
            data = self._p(src).read_bytes()
        except FileNotFoundError:
            raise StoreError(f"{src}: not found") from None
        p = self._p(dst)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)

    def list(self, prefix: str) -> list[str]:
        self.calls.append(("list", prefix))
        out = (str(p.relative_to(self.root)) for p in self.root.rglob("*") if p.is_file() and not p.name.endswith(".part"))
        return sorted(k for k in out if k.startswith(prefix))


# ----------------------------------------------------------------------------- where a task's state lives

@dataclass
class Part:
    """One directory of a task's state: packed under `name` (e.g. "tree", "run/home")."""
    name: str
    path: Path
    exclude: tuple[str, ...] = ()       # paths inside the part left out (rebuilt on restore, e.g. ".lake/packages")
    optional: bool = False              # packed when present; a checkpoint without it removes it here
    clear: Path | None = None           # removed (Layout.remove) and made empty before the part is placed


@dataclass
class Layout:
    """A task's state on this host, as the project lays it out."""
    task: str
    parts: list[Part]
    notes: Path                         # this host's notes: ckpt/<task>.json (the seq held), tmp/ (staging)
    needs: tuple[Path, ...] = ()        # all present: the local copy can be resumed
    work: str = ""                      # recorded in the manifest; a restore under another one is logged
    remove: Callable[[Path], None] | None = None     # how a part's `clear` goes (default shutil.rmtree)

    @property
    def marker(self) -> Path:
        return Path(self.notes) / "ckpt" / f"{self.task}.json"

    @property
    def tmp(self) -> Path:
        d = Path(self.notes) / "tmp"
        d.mkdir(parents=True, exist_ok=True)
        return d

    def complete(self) -> bool:
        return all(Path(p).exists() for p in self.needs)


def local_seq(lay: Layout) -> int:
    try:
        return int(json.loads(lay.marker.read_text()).get("seq") or 0)
    except (OSError, ValueError):
        return 0


def mark(lay: Layout, m: dict):
    lay.marker.parent.mkdir(parents=True, exist_ok=True)
    tmp = lay.marker.with_suffix(".tmp")
    tmp.write_text(json.dumps({k: m.get(k) for k in ("seq", "sha256", "created", "why")}) + "\n")
    tmp.replace(lay.marker)


def forget(lay: Layout):
    """The host's note of a task's checkpoint (at the task's cleanup)."""
    lay.marker.unlink(missing_ok=True)


def remote_manifest(st, prefix: str, which: str = "latest") -> dict | None:
    """The store's manifest, or None when the task has none (a prefix listing: see the docstring)."""
    if prefix + f"{which}.json" not in st.list(prefix):
        return None
    return json.loads(st.get(prefix + f"{which}.json"))


# ----------------------------------------------------------------------------- pack and restore

def _tar_filter(skip: tuple[str, ...]):
    def f(ti: tarfile.TarInfo):
        if any(ti.name == s or ti.name.startswith(s + "/") for s in skip):
            return None
        ti.uid = ti.gid = 0
        ti.uname = ti.gname = ""
        return ti
    return f


def pack(lay: Layout, out: Path, level: int = 3) -> int:
    """The task's state as one tar.zst at `out`; its size. Symbolic links are kept as links, never followed."""
    if shutil.which("zstd") is None:
        raise StoreError("zstd is not installed")
    with open(out, "wb") as fh:
        z = subprocess.Popen(["zstd", f"-{level}", "-T0", "-q", "-c"], stdin=subprocess.PIPE, stdout=fh)
        try:
            with tarfile.open(fileobj=z.stdin, mode="w|", format=tarfile.PAX_FORMAT) as tf:
                for p in lay.parts:
                    if p.optional and not Path(p.path).is_dir():
                        continue
                    tf.add(p.path, p.name, filter=_tar_filter(tuple(f"{p.name}/{x.strip('/')}" for x in p.exclude)))
        finally:
            z.stdin.close()
            rc = z.wait()
        if rc != 0:
            raise StoreError(f"zstd exited with {rc}")
    return out.stat().st_size


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for b in iter(lambda: fh.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def save(st, lay: Layout, prefix: str, why: str, worker: str = "", meta: dict | None = None, log=print) -> dict:
    """Pack the task and upload it as the newest checkpoint (the newest one before becomes `previous` when its
    archive matches its manifest: it is downloaded and checked); the manifest. Raises StoreError (the caller
    decides whether that stops anything)."""
    if not lay.complete():
        raise StoreError(f"{lay.task}: nothing to checkpoint here (the local copy is incomplete)")
    t0 = time.monotonic()
    with tempfile.TemporaryDirectory(dir=lay.tmp) as d:
        arc = Path(d) / "ckpt.tar.zst"
        size = pack(lay, arc)
        t_pack = time.monotonic() - t0
        keys = st.list(prefix)
        old = json.loads(st.get(prefix + "latest.json")) if prefix + "latest.json" in keys else None
        m = {"v": MANIFEST_V, "task": lay.task, **(meta or {}),
             "seq": max(local_seq(lay), int((old or {}).get("seq") or 0)) + 1, "why": why,
             "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "sha256": sha256_file(arc), "size": size,
             "work": lay.work, "worker": worker}
        if old and prefix + "latest.tar.zst" in keys:
            # only an intact `latest` becomes `previous`: after an upload cut between archive and manifest,
            # `previous` is the one checkpoint left to restore, and stays
            if _fetch(st, prefix, "latest", Path(d) / "latest.tar.zst") is not None:
                st.copy(prefix + "latest.tar.zst", prefix + "previous.tar.zst")
                st.copy(prefix + "latest.json", prefix + "previous.json")
            else:
                log(f"[{lay.task}] checkpoint `latest` is not intact (an upload cut); `previous` stays")
            (Path(d) / "latest.tar.zst").unlink(missing_ok=True)
        st.put_file(prefix + "latest.tar.zst", arc)
        st.put(prefix + "latest.json", (json.dumps(m, indent=1) + "\n").encode())
    mark(lay, m)
    log(f"[{lay.task}] checkpoint {m['seq']} ({why}): {size / 1e6:.2f} MB, packed {t_pack:.1f} s, "
        f"total {time.monotonic() - t0:.1f} s")
    return m


def _fetch(st, prefix: str, which: str, dest: Path) -> dict | None:
    """The `which` pair downloaded to dest, if its archive matches its manifest; else None."""
    try:
        m = json.loads(st.get(prefix + f"{which}.json"))
        st.get_file(prefix + f"{which}.tar.zst", dest)
    except (StoreError, ValueError):
        return None
    return m if sha256_file(dest) == m.get("sha256") else None


def remove_path(p: Path):
    """p removed without following links (nothing when it is not there)."""
    if p.is_symlink() or p.is_file():
        p.unlink()
    elif p.exists():
        shutil.rmtree(p)


def replace_dir(src: Path, dst: Path):
    """dst becomes src (removed without following links, never as root)."""
    remove_path(dst)
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.move(str(src), str(dst))        # the parts may be on other file systems than the staging directory


def _label(m: dict) -> str:
    return f"{m.get('why')}, from {m.get('worker') or '?'}"


def restore(st, lay: Layout, prefix: str, after: Callable[[], None] | None = None, log=print, force: bool = False,
            label: Callable[[dict], str] = _label) -> dict | None:
    """Bring the store's newest checkpoint here when it is newer than this host's copy (or the local copy is
    incomplete, or `force`); its manifest, or None when the local copy stays (or there is no checkpoint).
    `after()` runs once the parts are in place (rebuild what the parts' `exclude` left out). Raises StoreError
    when a checkpoint exists but neither `latest` nor `previous` can be read intact, or it lacks a part."""
    keys = st.list(prefix)
    if prefix + "latest.json" not in keys:
        return None
    remote = json.loads(st.get(prefix + "latest.json"))
    if not force and lay.complete() and local_seq(lay) >= int(remote.get("seq") or 0):
        return None
    t0 = time.monotonic()
    stage = Path(tempfile.mkdtemp(prefix=f"{lay.task}.", dir=lay.tmp))
    try:
        arc = stage / "ckpt.tar.zst"
        m = _fetch(st, prefix, "latest", arc)
        if m is None and prefix + "previous.json" in keys:
            log(f"[{lay.task}] checkpoint `latest` is not intact; restoring `previous`")
            m = _fetch(st, prefix, "previous", arc)
        if m is None:
            raise StoreError(f"{lay.task}: no intact checkpoint under {prefix}")
        out = stage / "x"
        out.mkdir()
        # GNU tar: members are relative (no '..', no leading '/'), and links that point outside the target are
        # created only after every file (no write through a link from the archive)
        r = subprocess.run(["tar", "--zstd", "-xf", str(arc), "-C", str(out), "--no-same-owner", "--no-same-permissions"],
                           capture_output=True, text=True)
        if r.returncode != 0:
            raise StoreError(f"{lay.task}: the checkpoint does not unpack: {r.stderr.strip()[:300]}")
        for p in lay.parts:
            if not p.optional and not (out / p.name).exists():
                raise StoreError(f"{lay.task}: the checkpoint has no {p.name}/")
        if m.get("work") and lay.work and m["work"] != lay.work:
            log(f"[{lay.task}] the checkpoint was taken under {m['work']}, this host's work is {lay.work} (paths in the "
                "session may differ)")
        for p in lay.parts:
            if p.clear is not None:
                c = Path(p.clear)
                if c.exists():
                    (lay.remove or shutil.rmtree)(c)
                c.mkdir(parents=True)
            if (out / p.name).exists():
                replace_dir(out / p.name, Path(p.path))
            else:                                # an optional part the checkpoint has not: none here either
                remove_path(Path(p.path))
        if after:
            after()
        mark(lay, m)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    log(f"[{lay.task}] restored checkpoint {m['seq']} ({label(m)}) in {time.monotonic() - t0:.1f} s")
    return m


# ----------------------------------------------------------------------------- evidence

def sync_evidence(st, src: Path, prefix: str, index: Path, meta: dict, log=print) -> int:
    """The directory `src` to <prefix><path>: the files that changed since the last sync (this host's `index`
    file, else the store's), then <prefix>_index.json; the number of files sent."""
    try:
        old = json.loads(Path(index).read_text())
    except (OSError, ValueError):
        old = None
    if old is None:
        try:
            old = json.loads(st.get(prefix + "_index.json")) if prefix + "_index.json" in st.list(prefix) else {}
        except (StoreError, ValueError):
            old = {}
    have = (old or {}).get("files") or {}
    files, sent = {}, 0
    for f in sorted(Path(src).rglob("*")):
        if not f.is_file() or f.is_symlink():
            continue
        rel = str(f.relative_to(src))
        sha = sha256_file(f)
        files[rel] = {"sha256": sha, "size": f.stat().st_size}
        if (have.get(rel) or {}).get("sha256") != sha:
            st.put_file(prefix + rel, f)
            sent += 1
    ix = {**meta, "files": files, "synced": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    if sent or files.keys() != have.keys() or any((old or {}).get(k) != v for k, v in meta.items()):
        st.put(prefix + "_index.json", (json.dumps(ix, indent=1) + "\n").encode())
    Path(index).parent.mkdir(parents=True, exist_ok=True)
    Path(index).write_text(json.dumps(ix) + "\n")
    return sent


def fetch_evidence(st, prefix: str, dest_of: Callable[[dict], Path], log=print) -> tuple[Path, int] | None:
    """<prefix> back to `dest_of(index)` (files whose sha256 differs); (the folder, files fetched), or None when
    the store has no index there. A file that does not match the index after its download raises StoreError."""
    if prefix + "_index.json" not in st.list(prefix):
        return None
    ix = json.loads(st.get(prefix + "_index.json"))
    name = ix.get("task") or prefix.rstrip("/")
    dest = Path(dest_of(ix))
    n = 0
    for rel, meta in ix["files"].items():
        if rel.startswith("/") or ".." in rel.split("/"):
            log(f"[{name}] skipped {rel!r}: not a relative path")
            continue
        f = dest / rel
        if f.is_file() and sha256_file(f) == meta["sha256"]:
            continue
        f.parent.mkdir(parents=True, exist_ok=True)
        tmp = f.with_name(f.name + ".part")
        st.get_file(prefix + rel, tmp)
        if sha256_file(tmp) != meta["sha256"]:
            tmp.unlink()
            raise StoreError(f"{name}/{rel}: sha256 differs from the index (uploaded while read?)")
        tmp.replace(f)
        n += 1
    return dest, n
