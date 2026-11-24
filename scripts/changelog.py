#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Changelog fragments: one file per change in changelog.d/, folded into CHANGELOG.md at a release.

A pull request adds its entry as changelog.d/<short-slug>.md instead of editing CHANGELOG.md, so
pull requests no longer conflict on the top of the Unreleased section. A fragment is front matter
naming the section, then the entry as it would stand in CHANGELOG.md (one or more bullets):

    ---
    section: Fixed
    ---

    - **What changed, in one line.** Why it matters and what to do. (node)

  scripts/changelog.py check [--base REV]   check the fragments; with --base, also fail when
                                            CHANGELOG.md's Unreleased section differs from REV's
                                            (allowed only when a new version section is added)
  scripts/changelog.py fold VERSION [--date YYYY-MM-DD] [--dry-run]
                                            move Unreleased and every fragment into a new section
                                            for VERSION, delete the fragments, update the links

Standard library only.
"""

from __future__ import annotations

import argparse
import datetime
import os
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

# Keep a Changelog's sections, in the order a version lists them. Other sections a release
# writes by hand ("Known issues", "Updating a node") follow these, in their own order.
SECTIONS = ["Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"]

ROOT = Path(__file__).resolve().parent.parent
FRAGMENT_DIR = "changelog.d"
CHANGELOG = "CHANGELOG.md"
UNRELEASED_RE = re.compile(r"^## \[Unreleased\][ \t]*$", re.M)
VERSION_HEADING_RE = re.compile(r"^## \[([^\]]+)\]", re.M)
FRONT_RE = re.compile(r"\A---\n((?:[a-z_]+:[^\n]*\n)+)---\n")
SLUG_RE = re.compile(r"^[a-z0-9][a-z0-9-]*\.md$")
VERSION_RE = re.compile(r"^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$")
LINK_RE = re.compile(r"^\[Unreleased\]: (\S+)/compare/(\S+)\.\.\.(\S+)$", re.M)


class Error(Exception):
    pass


@dataclass
class Fragment:
    path: Path
    section: str
    body: str


def parse_fragment(path: Path) -> Fragment:
    """Read one fragment; raise Error naming the file and the problem."""
    name = path.name
    if not SLUG_RE.match(name):
        raise Error(f"{FRAGMENT_DIR}/{name}: name it <short-slug>.md (lower case, digits, dashes)")
    text = path.read_text(encoding="utf-8").replace("\r\n", "\n")
    m = FRONT_RE.match(text)
    if not m:
        raise Error(f"{FRAGMENT_DIR}/{name}: starts with front matter: ---, section: <name>, ---")
    fields = {}
    for line in m.group(1).splitlines():
        key, _, value = line.partition(":")
        fields[key.strip()] = value.strip()
    if set(fields) != {"section"}:
        raise Error(f"{FRAGMENT_DIR}/{name}: the front matter has one field, section")
    section = fields["section"]
    if section not in SECTIONS:
        raise Error(f"{FRAGMENT_DIR}/{name}: section {section!r} is not one of {', '.join(SECTIONS)}")
    body = text[m.end():].strip("\n")
    if not body.startswith("- "):
        raise Error(f"{FRAGMENT_DIR}/{name}: the entry is a Markdown bullet (\"- **...** ...\")")
    if re.search(r"^#", body, re.M):
        raise Error(f"{FRAGMENT_DIR}/{name}: no headings in an entry; the section comes from the front matter")
    return Fragment(path, section, body.rstrip() + "\n")


def fragments(root: Path) -> list[Fragment]:
    """Every fragment in changelog.d/, by file name. README.md is the folder's documentation."""
    d = root / FRAGMENT_DIR
    if not d.is_dir():
        return []
    out = []
    for p in sorted(d.iterdir()):
        if p.name == "README.md" or p.name.startswith("."):
            continue
        if not p.is_file():
            raise Error(f"{FRAGMENT_DIR}/{p.name}: fragments are files, no subfolders")
        out.append(parse_fragment(p))
    return out


def unreleased_span(text: str) -> tuple[int, int]:
    """The Unreleased section's body: from after its heading to the next version heading."""
    m = UNRELEASED_RE.search(text)
    if not m:
        raise Error(f"{CHANGELOG}: no '## [Unreleased]' heading")
    start = m.end() + 1 if text[m.end():m.end() + 1] == "\n" else m.end()
    nxt = VERSION_HEADING_RE.search(text, start)
    if nxt:
        end = nxt.start()
    else:
        # The last section: it ends where the link references start, or at the end.
        links = re.search(r"^\[[^\]]+\]: \S+$", text[start:], re.M)
        end = start + links.start() if links else len(text)
    return start, end


def split_sections(body: str) -> tuple[str, list[tuple[str, str]]]:
    """Split a version's body into the text before its first ### heading and (heading, text) pairs."""
    parts = re.split(r"^### (.+?)[ \t]*$", body, flags=re.M)
    pre = parts[0].strip("\n")
    pairs = [(parts[i].strip(), parts[i + 1].strip("\n")) for i in range(1, len(parts), 2)]
    return pre, pairs


def fold_text(text: str, frags: list[Fragment], version: str, date: str) -> str:
    """CHANGELOG.md with Unreleased (as it is) and the fragments moved into a section for version."""
    if not VERSION_RE.match(version):
        raise Error(f"version {version!r}: want a tag like v0.7.0")
    if re.search(rf"^## \[{re.escape(version)}\]", text, re.M):
        raise Error(f"{CHANGELOG}: already has a section for {version}")
    start, end = unreleased_span(text)
    pre, pairs = split_sections(text[start:end])

    # Same-named subsections (an Unreleased that grew two "### Added") become one; fragments
    # follow what Unreleased already had, by file name.
    merged: dict[str, list[str]] = {}
    for heading, chunk in pairs:
        if chunk:
            merged.setdefault(heading, []).append(chunk)
        else:
            merged.setdefault(heading, [])
    for f in frags:
        merged.setdefault(f.section, []).append(f.body.rstrip("\n"))
    if not (pre or any(merged.values())):
        raise Error("nothing to release: Unreleased is empty and there are no fragments")
    order = [s for s in SECTIONS if s in merged] + [h for h in merged if h not in SECTIONS]

    out = [f"## [{version}] — {date}\n"]
    if pre:
        out.append(pre + "\n")
    for heading in order:
        if not merged[heading]:
            continue
        out.append(f"### {heading}\n")
        out.append("\n".join(merged[heading]) + "\n")
    section = "\n".join(out)

    new = text[:start] + "\n" + section + "\n" + text[end:]

    # The links at the end: Unreleased compares from the new tag; the new tag from the previous.
    m = LINK_RE.search(new)
    if m:
        base, prev, head = m.group(1), m.group(2), m.group(3)
        new = (new[:m.start()] + f"[Unreleased]: {base}/compare/{version}...{head}\n"
               f"[{version}]: {base}/compare/{prev}...{version}" + new[m.end():])
    return new


def unreleased_of(text: str) -> str:
    start, end = unreleased_span(text)
    return text[start:end].strip()


def versions_of(text: str) -> set[str]:
    return set(VERSION_HEADING_RE.findall(text)) - {"Unreleased"}


def check_base(root: Path, base: str) -> None:
    """Fail when this change edits Unreleased directly, unless it is a release (adds a version)."""
    try:
        old = subprocess.run(["git", "show", f"{base}:{CHANGELOG}"], cwd=root, check=True,
                             capture_output=True, text=True).stdout
    except subprocess.CalledProcessError as e:
        raise Error(f"git show {base}:{CHANGELOG}: {e.stderr.strip()}") from e
    new = (root / CHANGELOG).read_text(encoding="utf-8")
    if unreleased_of(old) == unreleased_of(new):
        return
    added = versions_of(new) - versions_of(old)
    if added:
        print(f"{CHANGELOG}: Unreleased changed with a new section ({', '.join(sorted(added))}): a release")
        return
    raise Error(
        f"{CHANGELOG}: this change edits the Unreleased section. Add the entry as a file in "
        f"{FRAGMENT_DIR}/ instead (see {FRAGMENT_DIR}/README.md); only a release moves "
        f"entries into CHANGELOG.md (scripts/changelog.py fold).")


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("check", help="check the fragments (and, with --base, Unreleased)")
    c.add_argument("--base", help="a git revision to compare CHANGELOG.md's Unreleased section with")
    f = sub.add_parser("fold", help="fold Unreleased and the fragments into a version section")
    f.add_argument("version", help="the tag, e.g. v0.7.0")
    f.add_argument("--date", default=datetime.datetime.now(datetime.timezone.utc).date().isoformat())
    f.add_argument("--dry-run", action="store_true", help="print the new CHANGELOG.md, change nothing")
    args = ap.parse_args(argv)
    root = Path(os.environ.get("CHANGELOG_ROOT", ROOT))
    try:
        frags = fragments(root)
        if args.cmd == "check":
            if args.base:
                check_base(root, args.base)
            print(f"{len(frags)} changelog fragment(s), all well-formed")
            return 0
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", args.date):
            raise Error(f"--date {args.date!r}: want YYYY-MM-DD")
        path = root / CHANGELOG
        new = fold_text(path.read_text(encoding="utf-8"), frags, args.version, args.date)
        if args.dry_run:
            sys.stdout.write(new)
            return 0
        path.write_text(new, encoding="utf-8")
        for fr in frags:
            fr.path.unlink()
        print(f"{CHANGELOG}: {args.version} with {len(frags)} fragment(s); removed them from {FRAGMENT_DIR}/")
        return 0
    except Error as e:
        print(f"error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
