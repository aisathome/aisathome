# SPDX-License-Identifier: Apache-2.0
"""Host-side file access inside a directory the model can write (the sandbox HOME, the task's tree).

The model controls every name under such a root: any file or directory may be a symlink to a host
path. A plain `Path.write_text` or `shutil.copy` follows it and writes or reads the host file. Every
function here walks from a trusted root one component at a time with O_NOFOLLOW, so a symlink anywhere
on the way is an error (`UnsafePath`), never followed; writes go to a new temporary file in the same
directory and are renamed over the target (a rename replaces a symlink, it does not follow it).

`rel` is a path relative to `root` with no `..`. The root itself is trusted (the project created it).
Standard library only; Linux and other POSIX systems with O_NOFOLLOW.
"""
from __future__ import annotations

import errno
import os
import stat
from contextlib import contextmanager
from pathlib import Path

_DIR = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)


class UnsafePath(OSError):
    """A path under an untrusted root is a symlink or not the kind of file expected."""


def _parts(rel: str | Path) -> list[str]:
    parts = [p for p in Path(rel).parts if p not in ("", ".")]
    if not parts or Path(rel).is_absolute() or ".." in parts:
        raise ValueError(f"not a relative path inside the root: {rel}")
    return parts


@contextmanager
def _dir(root: Path, parts: list[str], create: bool):
    """A file descriptor of root/parts..., each component opened without following symlinks."""
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_CLOEXEC", 0))
    try:
        for name in parts:
            try:
                nfd = os.open(name, _DIR, dir_fd=fd)
            except FileNotFoundError:
                if not create:
                    raise
                os.mkdir(name, 0o755, dir_fd=fd)
                nfd = os.open(name, _DIR, dir_fd=fd)
            except OSError as e:
                if e.errno in (errno.ELOOP, errno.ENOTDIR):
                    raise UnsafePath(f"{name} under {root} is a symlink or not a directory") from None
                raise
            os.close(fd)
            fd = nfd
        yield fd
    finally:
        os.close(fd)


def is_dir(root: Path, rel: str | Path) -> bool:
    """root/rel is a real directory, reached without symlinks (an existing symlink on the way is an error)."""
    try:
        with _dir(root, _parts(rel), create=False):
            return True
    except FileNotFoundError:
        return False


def mkdirs(root: Path, rel: str | Path):
    """Create root/rel and its parents as real directories (an existing symlink on the way is an error)."""
    with _dir(root, _parts(rel), create=True):
        pass


def _open_file(root: Path, rel, flags: int):
    parts = _parts(rel)
    ctx = _dir(root, parts[:-1], create=bool(flags & os.O_CREAT))
    fd_dir = ctx.__enter__()
    try:
        fd = os.open(parts[-1], flags | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0), 0o644, dir_fd=fd_dir)
    except OSError as e:
        ctx.__exit__(None, None, None)
        if e.errno == errno.ELOOP:
            raise UnsafePath(f"{rel} under {root} is a symlink") from None
        raise
    return fd, ctx


def read_bytes(root: Path, rel: str | Path, limit: int = 256 * 2**20) -> bytes:
    """A regular file's bytes (at most `limit`); a symlink or a non-regular file is UnsafePath."""
    fd, ctx = _open_file(root, rel, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0))
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise UnsafePath(f"{rel} under {root} is not a regular file")
        out, n = [], 0
        while n <= limit:
            b = os.read(fd, 1 << 20)
            if not b:
                break
            out.append(b)
            n += len(b)
        if n > limit:
            raise UnsafePath(f"{rel} under {root} is larger than {limit} bytes")
        return b"".join(out)
    finally:
        os.close(fd)
        ctx.__exit__(None, None, None)


def read_text(root: Path, rel: str | Path, **kw) -> str:
    return read_bytes(root, rel, **kw).decode(errors="replace")


def exists(root: Path, rel: str | Path) -> bool:
    """A regular file at root/rel, reached without symlinks."""
    try:
        fd, ctx = _open_file(root, rel, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0))
    except (FileNotFoundError, UnsafePath, NotADirectoryError):
        return False
    try:
        return stat.S_ISREG(os.fstat(fd).st_mode)
    finally:
        os.close(fd)
        ctx.__exit__(None, None, None)


def write_bytes(root: Path, rel: str | Path, data: bytes, mode: int = 0o644):
    """Write root/rel atomically: a new temporary file in the same (real) directory, renamed over the
    target. A symlink at the target is replaced, never followed; missing parents are created."""
    parts = _parts(rel)
    with _dir(root, parts[:-1], create=True) as d:
        tmp = f".{parts[-1]}.{os.getpid()}.tmp"
        try:
            os.unlink(tmp, dir_fd=d)
        except FileNotFoundError:
            pass
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode, dir_fd=d)
        try:
            try:
                _write_all(fd, data)
                os.fchmod(fd, mode)
            finally:
                os.close(fd)
        except BaseException:
            os.unlink(tmp, dir_fd=d)     # the target stays as it was
            raise
        os.rename(tmp, parts[-1], src_dir_fd=d, dst_dir_fd=d)


def _write_all(fd: int, data: bytes):
    """All of data to fd: os.write may write less (a full disk, a quota, a signal); a write that makes no
    progress raises, so a truncated file never replaces the target."""
    view = memoryview(data)
    while view:
        n = os.write(fd, view)
        if n <= 0:
            raise OSError(errno.ENOSPC, f"short write: {len(view)} of {len(data)} bytes not written")
        view = view[n:]


def write_text(root: Path, rel: str | Path, text: str, mode: int = 0o644):
    write_bytes(root, rel, text.encode(), mode)


def unlink(root: Path, rel: str | Path):
    """Remove root/rel if it exists (a symlink is removed as a link, its target untouched)."""
    parts = _parts(rel)
    try:
        with _dir(root, parts[:-1], create=False) as d:
            os.unlink(parts[-1], dir_fd=d)
    except FileNotFoundError:
        pass


def copy_out(root: Path, rel: str | Path, dest: Path, limit: int = 64 * 2**20) -> bool:
    """Copy a regular file from the untrusted root to a host path; False when it is missing, a symlink
    or not a regular file (nothing is copied then)."""
    try:
        data = read_bytes(root, rel, limit=limit)
    except (FileNotFoundError, UnsafePath, NotADirectoryError):
        return False
    tmp = dest.with_name(f".{dest.name}.{os.getpid()}.tmp")
    tmp.write_bytes(data)
    tmp.replace(dest)
    return True
