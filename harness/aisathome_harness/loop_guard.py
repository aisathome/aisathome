# SPDX-License-Identifier: Apache-2.0
"""Stuck-loop detection: the same search or command, again and again, with the same empty or failing
result. A model can spend most of a session on variants of one search that cannot match (a pattern
that misses a qualified name, say). The variants differ in their paths, pipes and filters, so commands
are compared by what they ask for: `signature` keeps a search's pattern and a command's words, without
`cd`, redirections and the filters after a pipe. An edit starts the count again: the same failing build
after an edit is ordinary work.

`Tracker.see` takes one tool call and returns the signature once it has come back with the same empty or
failing result `REPEATS` times since the last edit. `loop.LoopWatch` follows a round's events with it, and
the session loop then sends one message (see `loop.Prompts.loop`). Standard library only.
"""
from __future__ import annotations

import hashlib
import json
import re
import shlex

REPEATS = 5
SEARCHES = ("grep", "egrep", "fgrep", "rg", "ag")
FILTERS = ("head", "tail", "wc", "sort", "uniq", "cut", "tr")
WRITES = ("edit", "write", "patch", "apply_patch", "multiedit")
EMPTY = ("", "(no output)")
NO_FILE = re.compile(r"^(?:grep|rg|ls|sed|cat): .*(?:No such file or directory|cannot access).*$", re.M)
WRITE_CMD = re.compile(r"<<|\bsed\s+-i|\btee\b|\bcp\b|\bmv\b|\brm\b|(?:^|[^0-9&])>{1,2}\s*(?!/dev/null|&)\S")
SPLIT = re.compile(r"\s*(?:;|&&|\|\||\n)\s*")


def tokens(part: str) -> list[str]:
    try:
        lex = shlex.shlex(part, posix=True, punctuation_chars="|")
        lex.whitespace_split = True
        lex.commenters = ""
        return list(lex)
    except ValueError:
        return part.split()


def search_pattern(words: list[str]) -> str:
    """The pattern of a grep-like command: after -e, else its first word that is not an option."""
    i = 1
    while i < len(words):
        w = words[i]
        if w in ("-e", "--regexp"):
            return words[i + 1] if i + 1 < len(words) else ""
        if w.startswith("--include") or w.startswith("--exclude"):
            i += 1 if "=" in w else 2
            continue
        if w.startswith("-") and len(w) > 1:
            i += 1
            continue
        return w
    return ""


def signature(command: str) -> str | None:
    """What a shell command asks for; None for a command that writes (an edit, see `is_edit`)."""
    if is_edit(command):
        return None
    for part in SPLIT.split(command.strip()):
        words = tokens(part)
        if not words or words[0] in ("cd", "export", "set", "true"):
            continue
        words = words[:words.index("|")] if "|" in words else words
        words = [w for w in words if not re.fullmatch(r"\d?>&?\S+|\d?>", w)]
        while words and re.fullmatch(r"[A-Za-z_]\w*=.*", words[0]):
            words = words[1:]
        if not words:
            continue
        if words[0] in SEARCHES:
            return "grep " + search_pattern(words)
        return " ".join(words)
    return None


def is_edit(command: str) -> bool:
    return bool(WRITE_CMD.search(command))


def result_of(output: str, exit_code) -> str | None:
    """The kind of result that counts: a key for an empty or failing one, None for one that answered."""
    text = NO_FILE.sub("", output or "").strip()
    if text in EMPTY:
        return "empty"
    if exit_code not in (0, None):
        return "failed:" + hashlib.sha256(text.encode()).hexdigest()[:12]
    return None


def call(ev: dict) -> tuple[str, str, str, object] | None:
    """(tool, shell command or "", output, exit code) of a tool_use event."""
    try:
        part = ev["part"]
        st = part["state"]
        meta = st.get("metadata") or {}
        code = (meta.get("metadata") or meta).get("exit")
        return str(part.get("tool") or ""), str((st.get("input") or {}).get("command") or ""), str(st.get("output") or ""), code
    except (KeyError, TypeError, AttributeError):
        return None


class Tracker:
    """The count per (signature, result) since the last edit."""

    def __init__(self, repeats: int = REPEATS):
        self.repeats = repeats
        self.counts: dict[tuple[str, str], int] = {}

    def see(self, ev: dict) -> str | None:
        c = call(ev)
        if c is None:
            return None
        tool, command, output, code = c
        if tool in WRITES or (command and is_edit(command)):
            self.counts.clear()
            return None
        sig = signature(command) if command else None
        res = result_of(output, code) if sig else None
        if not res:
            return None
        n = self.counts[(sig, res)] = self.counts.get((sig, res), 0) + 1
        return sig if n >= self.repeats else None


def events_of(lines) -> list[dict]:
    out = []
    for line in lines:
        if '"tool_use"' in line:
            try:
                out.append(json.loads(line))
            except ValueError:
                continue
    return out
