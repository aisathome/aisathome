# SPDX-License-Identifier: Apache-2.0
"""`sorry`, `admit` or `native_decide` in the Lean CODE that a diff adds, leaving prose alone.

A whole-word regex over every added line fails on ordinary prose ("... subspaces that admit a frame"),
so the check reads each added line in the file's new text with comments and string literals blanked out:

- Only `.lean` files. Other files hold no proofs.
- Each added line is read in the file's new text with comments and literal text blanked out
  (lexer.code_mask: nested comments, strings, raw strings, char literals, `«…»` names, code inside
  string interpolation kept), so a docstring line in the middle of a block, with no `/-` of its own,
  is still prose, and a `/-` inside a string hides nothing.
- Strict when unsure: a file that cannot be read, or whose line differs from the diff's added line
  (the tree is not the diff's), is checked on the raw added line.

A project's own build is the real check (Lean reports a `sorry` as `sorryAx` in the axioms of a
declaration); this is an early, strict guard on code. Standard library only.
"""
from __future__ import annotations

import re
from typing import Callable

from .lexer import code_mask  # noqa: E402,F401  (comments and literal text blanked; see lexer.py)

WORD_RE = re.compile(r"\b(sorry|admit|native_decide)\b")


HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def added_lines(diff: str) -> dict[str, list[tuple[int, str]]]:
    """New path -> [(new-side line number, added text)] for every line a unified diff adds."""
    out: dict[str, list[tuple[int, str]]] = {}
    path, new_no, header = None, 0, False
    for line in diff.splitlines():
        if line.startswith("diff --git "):
            path, header = None, True
        elif header and line.startswith("+++ "):
            p = line[4:].strip()
            path = None if p == "/dev/null" else (p[2:] if p.startswith("b/") else p)
        elif line.startswith("@@"):
            m = HUNK_RE.match(line)
            new_no, header = (int(m.group(1)) if m else 0), False
        elif header or path is None:
            continue
        elif line.startswith("+"):
            out.setdefault(path, []).append((new_no, line[1:]))
            new_no += 1
        elif line.startswith(" "):
            new_no += 1
        # "-" lines and "\ No newline at end of file" do not advance the new side
    return out


def hits(diff: str, read: Callable[[str], str | None]) -> list[dict]:
    """The added code lines with `sorry`/`admit`/`native_decide`: {"file", "line", "word", "text",
    "how"} ("code", or "raw line" when the file could not be matched to the diff). `read(path)`
    returns the file's new text, or None."""
    found = []
    for path, lines in added_lines(diff).items():
        if not path.endswith(".lean"):
            continue
        text = read(path)
        src = text.splitlines() if text is not None else None
        masked = code_mask(text).splitlines() if text is not None else None
        for no, added in lines:
            if src is not None and 0 < no <= len(src) and src[no - 1] == added:
                m, how = WORD_RE.search(masked[no - 1]), "code"
            else:
                m, how = WORD_RE.search(added), "raw line"
            if m:
                found.append({"file": path, "line": no, "word": m.group(1), "text": added.strip()[:160],
                              "how": how})
    return found
