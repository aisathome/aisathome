# SPDX-License-Identifier: Apache-2.0
"""Lean 4 source with comments and literal text blanked, code kept: what the text checks (sorry_text,
axioms.scan) read, so that neither prose nor data can hide code from them or trip them.

`code_mask(text)` returns `text` with every character of a comment or of a string or char literal
replaced by a space (newlines kept, so offsets, line numbers and columns stay). Code is kept, including
code inside string interpolation (`s!"… {expr} …"`, `m!`, `f!`). The lexer follows Lean's:

- line comments `--` to the end of the line; block comments `/- … -/` (also `/--`, `/-!`), nested;
- string literals `"…"` with backslash escapes, over several lines; raw strings `r"…"`, `r#"…"#`;
- char literals `'a'`, `'\\n'`, `'\\x41'`, `'\\u{2200}'`, where a `'` after an identifier character is part of
  the identifier (`h'`), not a literal;
- `«…»` names, which may contain anything (also `/-`, `--` or `"`), kept as code;
- inside an interpolated string, `{` … `}` is code (nested braces, strings and comments in it too).

Unterminated comments and strings run to the end of the text (Lean rejects the file then; a check that
reads the mask stays strict on everything before). Standard library only.
"""
from __future__ import annotations

import re

RAW_STR = re.compile(r'r(#*)"')
CHAR = re.compile(r"'(?:\\(?:x[0-9a-fA-F]{2}|u\{[0-9a-fA-F]+\}|u[0-9a-fA-F]{4}|.)|[^\\'\n])'")
INTERP = re.compile(r"(?<![\w.'])[smf]!\s*$")     # s!, m!, f! and whitespace before the quote


def _ident(ch: str) -> bool:
    return ch.isalnum() or ch in "_'!?.₀₁₂₃₄₅₆₇₈₉ₐₑₒₓ"


def code_mask(text: str, literals: bool = True) -> str:
    """`literals=False` blanks comments only and keeps string and char literals as they are."""
    return _lex(text, literals)[0]


def strings(text: str) -> list[tuple[int, int, bool]]:
    """(start, end, interpolated) of every ordinary string literal (not raw strings, not chars), in order."""
    return _lex(text, True)[1]


def braced_strings(text: str) -> list[int]:
    """Lines (1-based) of string literals with a `{` in them. Whether such a string is interpolated (its
    braces hold code that may contain strings and comments of its own) depends on the syntax that takes
    it, which a lexer cannot always know, so a check that must not miss code refuses them (axioms.scan)."""
    return [text.count("\n", 0, a) + 1 for a, b, _ in strings(text) if "{" in text[a:b]]


def _lex(text: str, literals: bool) -> tuple[str, list]:
    out = list(text)
    spans: list[tuple[int, int, bool]] = []

    def blank(a: int, b: int, comment: bool = False):
        if not (comment or literals):
            return
        for k in range(a, b):
            if out[k] != "\n":
                out[k] = " "

    def string(i: int, interp: bool) -> int:
        """`i` at the opening quote; returns the index after the closing one."""
        n, j, seg = len(text), i + 1, i          # seg: where the current literal piece starts
        while j < n:
            c = text[j]
            if c == "\\":
                j += 2
            elif c == '"':
                blank(seg, j + 1)
                spans.append((i, j + 1, interp))
                return j + 1
            elif interp and c == "{":
                blank(seg, j + 1)
                j = code(j + 1, closing="}")     # after the matching close brace
                seg = j - 1                      # the close brace belongs to the literal
            else:
                j += 1
        blank(seg, n)
        spans.append((i, n, interp))
        return n

    def code(i: int, closing: str | None = None) -> int:
        """Lex code from `i`; with `closing`, stop after the matching close brace and return the index
        after it."""
        n, depth = len(text), 0
        while i < n:
            c = text[i]
            if closing and c == "{":
                depth += 1
                i += 1
            elif closing and c == "}":
                if depth == 0:
                    return i + 1
                depth -= 1
                i += 1
            elif c == "-" and text.startswith("--", i):
                j = text.find("\n", i)
                j = n if j < 0 else j
                blank(i, j, comment=True)
                i = j
            elif c == "/" and text.startswith("/-", i):
                d, j = 1, i + 2
                while j < n and d:
                    if text.startswith("/-", j):
                        d, j = d + 1, j + 2
                    elif text.startswith("-/", j):
                        d, j = d - 1, j + 2
                    else:
                        j += 1
                blank(i, j, comment=True)
                i = j
            elif c == "«":
                j = text.find("»", i + 1)
                i = n if j < 0 else j + 1
            elif c == '"':
                i = string(i, bool(INTERP.search(text[max(0, i - 40):i])))
            elif c == "r" and (i == 0 or not _ident(text[i - 1])) and RAW_STR.match(text, i):
                m = RAW_STR.match(text, i)
                end = text.find('"' + m.group(1), m.end())
                j = n if end < 0 else end + 1 + len(m.group(1))
                blank(i, j)
                i = j
            elif c == "'" and (i == 0 or not _ident(text[i - 1])) and CHAR.match(text, i):
                j = CHAR.match(text, i).end()
                blank(i, j)
                i = j
            elif _ident(c):
                j = i
                while j < n and _ident(text[j]) and text[j] != "«":
                    j += 1
                i = max(j, i + 1)
            else:
                i += 1
        return n

    code(0)
    return "".join(out), sorted(spans)
