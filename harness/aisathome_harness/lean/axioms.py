# SPDX-License-Identifier: Apache-2.0
"""Refuse a Lean proof before compiling it, and check it after: the text scan for what a proof must not
contain, and a Lean command that checks a declaration's type and axioms.

`scan` reads the text with comments blanked (lexer.code_mask; string contents are read too, and a string
with a `{` is refused, so nothing can hide inside a literal) and returns why it is refused: `sorry`,
`admit`, `native_decide`, a declared axiom, `implemented_by`/`extern`, skipping the kernel's type check,
meta-level commands, elaborator extensions, `unsafe`; a project adds its own patterns (for example,
imports of files the proof must not use).

`check_command` is a Lean command a check file runs after the proof has compiled: the named declaration
must be a theorem whose type is definitionally equal to the target's, with the same universe parameters,
using only the allowed axioms. It reports `<MARK>_OK ...` or `<MARK>_FAIL <reason>: ...`, which
`parse_check` reads; the mark carries a nonce per check (`new_mark`), and success counts only on a clean
run (exit 0, no error) with the success line standing on its own. A project's check file imports the proof, restates the target under a fresh name and
runs the command on it. Standard library only (the Lean side runs in the project's own toolchain).
"""
from __future__ import annotations

import re
import secrets

from .lexer import braced_strings, code_mask

ALLOWED_AXIOMS = ("propext", "Classical.choice", "Quot.sound")
# Read on code only (lexer.code_mask) and not anchored at the start of a line: Lean commands can follow each
# other on one line, and a tactic such as `run_tac` sits inside a proof. `(?<![\w.'])` keeps a keyword from
# matching inside a longer name (`Foo.elab`, `h'`), which Lean's reserved keywords cannot be anyway.
_KW = r"(?<![\w.'])"
FORBIDDEN = [
    (r"\bsorry\b", "sorry"),
    (r"\badmit\b", "admit"),
    (r"\bnative_decide\b", "native_decide"),
    (_KW + r"axiom\b", "declares an axiom"),
    (r"\bimplemented_by\b|@\[\s*extern\b", "implemented_by/extern"),
    (r"\bskipKernelTC\b", "debug.skipKernelTC"),
    (_KW + r"(?:run_cmd|run_elab|run_meta|run_tac|#eval|#exit)\b", "meta-level command"),
    (_KW + r"(?:elab|elab_rules|macro|macro_rules|initialize|builtin_initialize)\b", "elaborator extension"),
    (r"\bunsafe\b", "unsafe"),
]


def blank_comments(s: str) -> str:
    """`s` with every comment character (line comments, nested block comments, docstrings) replaced by a
    space, newlines kept; string and char literals are lexed, so a `/-` or `--` inside one is no comment."""
    return code_mask(s, literals=False)


BRACED = "a string literal with `{` (possible interpolation, whose code the scan cannot be sure to see)"


def scan(text: str, forbidden: list[tuple[str, str]] | None = None) -> list[str]:
    """Why the proof text is refused; empty when nothing is found. Fails closed: comments do not count,
    but string contents do (a string holding `#eval` is refused, a miss is worse), and a string with a
    `{` is refused outright, since whether its braces hold code depends on syntax a lexer cannot know."""
    return list(scan_lines(text, forbidden))


def scan_lines(text: str, forbidden: list[tuple[str, str]] | None = None) -> dict[str, list[int]]:
    """Why the text is refused, with the line numbers (1-based) of each reason (see `scan`)."""
    code = code_mask(text, literals=False)
    out: dict[str, list[int]] = {}
    for pat, why in (FORBIDDEN if forbidden is None else forbidden):
        for m in re.finditer(pat, code, re.M):
            out.setdefault(why, []).append(code.count("\n", 0, m.start()) + 1)
    lines = braced_strings(text)
    if lines:
        out[BRACED] = lines
    return out


def check_command(command: str = "check_solution", mark: str = "CHECK", decl: str = "solution",
                  allowed: tuple[str, ...] = ALLOWED_AXIOMS) -> str:
    """The Lean source of a command `#<command> <target>`: `decl` is a theorem of the target's type
    (definitional equality, same universe parameters) whose axioms are among `allowed`."""
    cond = " || ".join(f"a == ``{a}" for a in allowed) or "false"
    return f'''
open Lean Elab Command Meta in
elab "#{command} " tgt:ident : command => do
  let env ← getEnv
  let some sol := env.find? `{decl}
    | throwError "{mark}_FAIL no_solution: no top-level declaration `{decl}`"
  let some t := env.find? tgt.getId
    | throwError "{mark}_FAIL internal: target {{tgt.getId}} not found"
  unless (sol matches .thmInfo _) do
    throwError "{mark}_FAIL not_theorem: `{decl}` is not a theorem"
  unless sol.levelParams.length == t.levelParams.length do
    throwError "{mark}_FAIL type_mismatch: universe parameters {{sol.levelParams}} vs {{t.levelParams}}"
  let solTy := sol.instantiateTypeLevelParams (t.levelParams.map mkLevelParam)
  let same := solTy == t.type
  let ok ← liftTermElabM <| isDefEq solTy t.type
  unless ok do
    throwError "{mark}_FAIL type_mismatch: `{decl}` does not have the target's type"
  let axs ← collectAxioms `{decl}
  let bad := axs.filter fun a => !({cond})
  unless bad.isEmpty do
    throwError "{mark}_FAIL axioms: {{bad.toList}}"
  logInfo m!"{mark}_OK same_expr={{same}} axioms={{axs.toList}}"
'''


def new_mark(prefix: str = "CHECK") -> str:
    """A mark for one check: the prefix and a random nonce. The check file is written after the proof
    is compiled, so nothing in the proof can know the mark and print it."""
    return f"{prefix}_{secrets.token_hex(8)}"


ERROR_LINE = re.compile(r"(?m)^(?:\S+:\d+:\d+:\s*)?error\b")


def parse_check(output: str, mark: str = "CHECK", rc: int = 0) -> dict:
    """The verdict of a check run (`lake env lean <check file>`): {"ok": True, "same_expr", "axioms"} only
    when the run exited 0, printed no error, and our command's success line stands on its own (the
    message starts with the mark; one with the mark inside, e.g. in another diagnostic, does not count).
    Otherwise {"ok": False, "reason", "detail"}: reason from our command's failure line, None when there
    is none (the check file did not elaborate). Use a fresh `mark` per check (new_mark)."""
    m = re.escape(mark)
    fail = re.search(m + r"_FAIL (\w+): ?(.*)", output)
    ok = re.findall(r"(?m)^(?:\S+:\d+:\d+:\s*(?:info:\s*)?)?" + m + r"_OK same_expr=(true|false) axioms=\[([^\]\n]*)\]\s*$",
                    output)
    if rc == 0 and not ERROR_LINE.search(output) and not fail and len(ok) == 1:
        same, axs = ok[0]
        return {"ok": True, "same_expr": same == "true", "axioms": [a.strip() for a in axs.split(",") if a.strip()]}
    return {"ok": False, "reason": fail.group(1) if fail else None, "detail": fail.group(2) if fail else None}
