# SPDX-License-Identifier: Apache-2.0
"""One proof, many holes: the `sorry`s of a Lean outline as stand-alone lemmas, and the outline put back
together from their proofs. Text only (standard library); the project compiles what this module writes.

The flow a project runs (each compile is the project's own Lake build in its pinned environment):

1. `standin_copy(text)`: a copy of the outline in which every `sorry` in code becomes a stand-in that, where
   Lean elaborates it, marks its position (tactic or term) and runs Mathlib's `extract_goal *` (the whole
   local context, inaccessible names renamed), then closes the goal with `sorry` as before. Compile it.
2. `extracted(output, copy)`: per hole, its position and the extracted statement (`theorem <name> <sig>`),
   or why there is none (not reached, elaborated more than once, a `def`, metavariables, ...).
3. Each statement as a lemma file of its own (`scope_lines` and `context_text` give its context), compiled
   with `sorry` by the project; `dry_assembly(text, holes)`: the outline with each hole's lemma inserted as
   `namespace <block> ... theorem solution <sig> := sorry ... end <block>` above the declaration that holds
   it, and the hole replaced by an application of `<block>.solution`. Compiled, it must fail only on
   `sorryAx`; a hole whose replacement does not elaborate is proved in place instead.
4. `assemble(base, proofs)`: each block's body replaced by its proof file (imports hoisted), for the
   project's full check.

`statement_key` identifies a hole across attempts and outlines: a hash of the environment's key, the
normalised context and the normalised statement (the lemma's own name dropped).

`extract_goal` relies on delaboration, so an extracted statement may not mean the goal (Mathlib's own
caveat); the lemma file's compile and the dry assembly are what make a hole count as extracted.
"""
from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass, field

from .lexer import code_mask

STANDIN = "aisathome_hole"
EXTRACTED = "aisathome_extracted"
MARK = "aisathome_hole:"                       # + "tactic" / "term"
STANDIN_IMPORTS = ("Mathlib.Tactic.ExtractGoal", "Mathlib.Tactic.Trace")
# `trace` (Mathlib.Tactic.Trace) logs the position marker at the stand-in; `extract_goal * using <name>` logs
# `theorem <name> ... := sorry` (or `def`); without a goal it can extract, `trace_state` prints the goal. The
# macros are not hygienic, so the name stays as written.
STANDIN_HEADER = (
    "-- each `sorry` became `" + STANDIN + "`: it marks its position and extracts its goal\n"
    f'syntax (name := aisathomeHoleTactic) "{STANDIN}" : tactic\n'
    f'syntax (name := aisathomeHoleTerm) "{STANDIN}" : term\n'
    "set_option hygiene false in\n"
    f"macro_rules | `(tactic| {STANDIN}) => `(tactic| focus (trace \"{MARK}tactic\"; "
    f"(first | extract_goal * using {EXTRACTED} | trace_state); sorry))\n"
    "set_option hygiene false in\n"
    f"macro_rules | `(term| {STANDIN}) => `(term| by trace \"{MARK}term\"; "
    f"(first | extract_goal * using {EXTRACTED} | trace_state); sorry)\n")

SORRY = re.compile(r"(?<![\w.'])sorry(?![\w'])")
IMPORT = re.compile(r"^[ \t]*import[ \t]+(\S+)[ \t]*$", re.M)
_MSG = re.compile(r"^(info|warning|error|trace): (?:\./)*(\S+?):(\d+):(\d+): ?(.*)$")
_NOT_MSG = ("info:", "warning:", "error:", "trace:", "✔", "⚠", "✖", "Build completed", "Some required targets",
            "[exit ", "$ ")


def blank(text: str) -> str:
    """Comments and literal text blanked (offsets and lines kept)."""
    return code_mask(text)


def sorry_offsets(text: str) -> list[int]:
    """Offsets of the `sorry`s in code (not in comments or strings), in order."""
    return [m.start() for m in SORRY.finditer(blank(text))]


def line_col(text: str, off: int) -> tuple[int, int]:
    """(1-based line, 0-based column in characters) of an offset, as Lean reports positions."""
    line = text.count("\n", 0, off) + 1
    return line, off - (text.rfind("\n", 0, off) + 1)


# ----------------------------------------------------------------------------- 1. the stand-in copy

@dataclass
class Copy:
    """The stand-in copy of an outline and how its positions map back."""
    text: str
    holes: list[tuple[int, int, int]]            # per hole: (offset in the outline, line, column) in the outline
    copy_pos: list[tuple[int, int]]               # per hole: (line, column) in the copy
    inserted: int                                 # lines inserted after the outline's imports
    after: int                                    # the outline's line after which they were inserted


def standin_copy(text: str, extra_imports: tuple[str, ...] = STANDIN_IMPORTS) -> Copy:
    """The outline with every code `sorry` replaced by the stand-in, `extra_imports` added after its imports
    and the stand-in's definition after them."""
    offs = sorry_offsets(text)
    holes = [(o, *line_col(text, o)) for o in offs]
    s = text
    for o in reversed(offs):
        s = s[:o] + STANDIN + s[o + len("sorry"):]
    imps = list(IMPORT.finditer(blank(s)))
    after = s.count("\n", 0, imps[-1].end()) + 1 if imps else 0
    lines = s.splitlines(True)
    if after and not lines[after - 1].endswith("\n"):
        lines[after - 1] += "\n"
    have = {m.group(1) for m in imps}
    add = "".join(f"import {m}\n" for m in extra_imports if m not in have)
    head = add + STANDIN_HEADER
    inserted = head.count("\n")
    grow = len(STANDIN) - len("sorry")
    pos, seen = [], {}
    for _, ln, col in holes:
        k = seen.get(ln, 0)
        seen[ln] = k + 1
        pos.append((ln + inserted if ln > after else ln, col + k * grow))
    return Copy("".join(lines[:after]) + head + "".join(lines[after:]), holes, pos, inserted, after)


# ----------------------------------------------------------------------------- 2. Lean's messages

def messages(output: str, rel: str) -> list[dict]:
    """Lake's messages about the file `rel`: {"sev", "line", "col", "text"} (a message's further lines
    joined to its text), in output order."""
    out, cur = [], None
    for line in output.splitlines():
        m = _MSG.match(line)
        if m or line.startswith(_NOT_MSG):
            cur = None
            if m and m.group(2) == rel:
                cur = {"sev": m.group(1), "line": int(m.group(3)), "col": int(m.group(4)), "text": [m.group(5)]}
                out.append(cur)
            continue
        if cur is not None:
            cur["text"].append(line)
    for c in out:
        c["text"] = "\n".join(c["text"]).strip("\n")
    return out


_HEAD = re.compile(r"^(theorem|def)\s+" + EXTRACTED + r"(?:\.\{([^}]*)\})?(?=[\s:({\[⦃]|$)")
_TAIL = re.compile(r":=\s*sorry\s*$")


def parse_statement(msg: str) -> dict | None:
    """`theorem <EXTRACTED>.{u, v} <sig> := sorry` -> {"cmd", "levels", "sig"}; None for any other text."""
    s = msg.strip()
    m = _HEAD.match(s)
    if not m or not _TAIL.search(s):
        return None
    sig = _TAIL.sub("", s[m.end():]).strip()
    levels = [x.strip() for x in (m.group(2) or "").split(",") if x.strip()]
    return {"cmd": m.group(1), "levels": levels, "sig": sig}


@dataclass
class Hole:
    """One `sorry` of the outline, as the stand-in copy's compile saw it."""
    index: int
    offset: int
    line: int
    col: int
    position: str | None = None          # "tactic" / "term" (None: Lean never reached it)
    statement: dict | None = None        # parse_statement's, when exactly one was extracted
    goal: str | None = None              # trace_state's goal when extract_goal failed
    why: str | None = None               # why the hole is not extracted (None: it is)

    @property
    def extracted(self) -> bool:
        return self.why is None


def extracted(output: str, rel: str, copy: Copy) -> list[Hole]:
    """Per hole of the outline: its position and its one extracted statement, or `why` not. Messages are
    matched to holes by line, then by column; the marker opens each elaboration of a stand-in, so a hole that
    Lean elaborated twice (under `<;>`, `all_goals`, a `first` alternative) shows two and is not extracted."""
    by_line: dict[int, list[dict]] = {}
    for m in messages(output, rel):
        if m["sev"] == "info":
            by_line.setdefault(m["line"], []).append(m)
    holes = [Hole(i, o, ln, col) for i, (o, ln, col) in enumerate(copy.holes)]
    on_line: dict[int, list[int]] = {}
    for i, (ln, _) in enumerate(copy.copy_pos):
        on_line.setdefault(ln, []).append(i)
    for ln, idx in on_line.items():
        groups: list[list[dict]] = []
        for m in sorted(by_line.get(ln, []), key=lambda m: m["col"]):
            if m["text"].startswith(MARK):
                groups.append([m])
            elif groups and groups[-1][0]["col"] == m["col"]:
                groups[-1].append(m)
        cols = {copy.copy_pos[i][1]: i for i in idx}
        per: dict[int, list[list[dict]]] = {i: [] for i in idx}
        rest = []
        for g in groups:
            i = cols.get(g[0]["col"])
            (per[i].append(g) if i is not None else rest.append(g))
        if rest and len(idx) == 1:                # one hole on the line: its column need not match exactly
            per[idx[0]] += rest
        for i in idx:
            _judge(holes[i], per[i])
    return holes


def _judge(h: Hole, groups: list[list[dict]]):
    if not groups:
        h.why = "not reached (an error before it, or code that does not run)"
        return
    h.position = groups[0][0]["text"][len(MARK):].strip() or None
    stmts, goals = [], []
    for g in groups:
        for m in g[1:]:
            st = parse_statement(m["text"])
            if st:
                stmts.append(st)
            elif "⊢" in m["text"]:
                goals.append(m["text"])
    h.goal = goals[0] if goals else None
    if len({g[0]["text"] for g in groups}) > 1:
        h.why = "elaborated in tactic and term position"
    elif len(groups) > 1:
        h.why = f"elaborated {len(groups)} times (one `sorry` for several goals)"
    elif not stmts:
        h.why = "extract_goal gave no statement" + (" (goal printed)" if goals else "")
    elif stmts[0]["cmd"] != "theorem":
        h.why = "the hole is data (a `def`), not a proposition"
    elif re.search(r"\bsorry(Ax)?\b|\?[\w.]+|✝", stmts[0]["sig"]):
        h.why = "the statement holds a `sorry`, a metavariable or an inaccessible name"
    else:
        h.statement = stmts[0]


# ----------------------------------------------------------------------------- statements: binders, key

_OPEN, _CLOSE = "([{⦃⟨", ")]}⦄⟩"


def split_signature(sig: str) -> tuple[list[str], str]:
    """(binder groups, type) of `<binders> : <type>`; a signature that starts with `:` has no binders."""
    i, depth, groups, start = 0, 0, [], None
    while i < len(sig):
        c = sig[i]
        if c in _OPEN:
            if depth == 0:
                start = i
            depth += 1
        elif c in _CLOSE:
            depth -= 1
            if depth == 0 and start is not None:
                groups.append(sig[start:i + 1])
                start = None
        elif depth == 0 and c == ":" and not sig.startswith(":=", i):
            return groups, sig[i + 1:].strip()
        i += 1
    return groups, ""


def explicit_names(sig: str) -> list[str] | None:
    """The names of the explicit binders, in order (what an application passes); None when the signature
    puts its hypotheses in a `∀` after the colon (extract_goal's form for a goal that is itself a `∀`)."""
    groups, ty = split_signature(sig)
    if not groups and ty.startswith(("∀", "forall")):
        return None
    out = []
    for g in groups:
        if g[0] != "(":
            continue
        inner = g[1:-1]
        k = _top_colon(inner)
        if k < 0:
            return None
        out += inner[:k].split()
    return out


def _top_colon(s: str) -> int:
    depth = 0
    for i, c in enumerate(s):
        if c in _OPEN:
            depth += 1
        elif c in _CLOSE:
            depth -= 1
        elif c == ":" and depth == 0 and not s.startswith(":=", i):
            return i
    return -1


def normalise(text: str) -> str:
    """Lean text for comparison: comments blanked, whitespace runs collapsed."""
    return " ".join(code_mask(text, literals=False).split())


def statement_key(sig: str, context: str = "", env: str = "") -> str:
    """The hole's identity: sha256 of the environment key, the normalised context and the normalised
    signature (the lemma's name is not part of it)."""
    h = hashlib.sha256()
    for part in ("holes-v1", env, normalise(context), normalise(sig)):
        h.update(part.encode())
        h.update(b"\0")
    return h.hexdigest()


def signature_key(sig: str) -> str:
    """The statement alone (any context): finds a proof made elsewhere, to be checked again here."""
    return hashlib.sha256(("holes-sig-v1\0" + normalise(sig)).encode()).hexdigest()


# ----------------------------------------------------------------------------- commands, scope, context

_DECL = r"(?:theorem|lemma|def|abbrev|instance|example|structure|class|inductive|opaque|axiom|irreducible_def)"
_MOD = r"(?:(?:private|protected|noncomputable|nonrec|partial|unsafe|scoped|local)\s+)*"
_ATTR = r"(?:@\[[^\]]*\]\s*)*"
_CMD = re.compile(r"^" + _ATTR + _MOD + r"(" + _DECL + r"|namespace|section|end|open|universe|variable|set_option|"
                  r"noncomputable\s+section|attribute|notation|infixl?|infixr|prefix|postfix|macro|syntax|elab|"
                  r"macro_rules|elab_rules|mutual|import|export|alias|#\w+)\b")
_ATTR_LINE = re.compile(r"^@\[[^\]]*\]\s*$")
_NAME = re.compile(_ATTR + _MOD + _DECL + r"\s+([^\s(:{\[⦃]+)")


@dataclass
class Command:
    start: int                 # offset of the command (its doc comment and attributes included)
    end: int                   # offset after it (the next command's start)
    kind: str                  # theorem, example, def, scope (namespace/section/end/open/...), import, other
    keyword: str
    name: str | None = None
    head: str = ""             # the command's first code line


def commands(text: str) -> list[Command]:
    """The file's top-level commands: a command starts at column 0 with a command keyword (Mathlib's layout,
    which model-written outlines follow); the comment lines right above it belong to it; `open ... in` and
    `set_option ... in` lines belong to the command after them."""
    code = blank(text)
    lines = code.splitlines(True)
    raw = text.splitlines(True)
    starts, off = [], 0
    pending = None
    for i, ln in enumerate(lines):
        m = _CMD.match(ln) if ln[:1] not in (" ", "\t", "\n") else None
        if m:
            s = off
            k = i
            while k > 0 and raw[k - 1].strip() and (not lines[k - 1].strip() or _ATTR_LINE.match(lines[k - 1])):
                # the comment and attribute lines right above
                k -= 1
                s -= len(lines[k])
            if pending is not None:
                s = pending
            kw = re.sub(r"\s+", " ", m.group(1))
            if kw in ("open", "set_option") and re.search(r"\bin\s*$", ln):
                pending = s if pending is None else pending
            else:
                starts.append((s, kw, i))
                pending = None
        off += len(ln)
    out = []
    for j, (s, kw, i) in enumerate(starts):
        e = starts[j + 1][0] if j + 1 < len(starts) else len(text)
        kind = ("theorem" if kw in ("theorem", "lemma") else "example" if kw == "example" else
                "def" if re.fullmatch(_DECL, kw) else "import" if kw == "import" else
                "scope" if kw in ("namespace", "section", "end", "open", "universe", "variable", "set_option",
                                  "noncomputable section") else "other")
        nm = _NAME.search(code[s:min(e, s + 400)]) if kind in ("theorem", "def") else None
        out.append(Command(s, e, kind, kw, nm.group(1) if nm else None, lines[i].strip()))
    return out


def enclosing(cmds: list[Command], offset: int) -> int | None:
    """The index of the command that holds `offset`."""
    for i, c in enumerate(cmds):
        if c.start <= offset < c.end:
            return i
    return None


def _scope_walk(text: str, cmds: list[Command], upto: int):
    """(the stack of open scopes [(kind, name, [lines])], universes declared) before command `upto`."""
    stack: list[tuple[str, str, list[str]]] = [("file", "", [])]
    universes: list[str] = []
    for c in cmds[:upto]:
        if c.kind != "scope":
            continue
        h = " ".join(c.head.split())
        if c.keyword == "namespace":
            stack.append(("namespace", h.split()[1] if len(h.split()) > 1 else "", []))
        elif c.keyword in ("section", "noncomputable section"):
            parts = h.split()
            stack.append(("section", parts[-1] if parts[-1] != "section" else "", []))
        elif c.keyword == "end":
            if len(stack) > 1:
                stack.pop()
        elif c.keyword in ("open", "set_option"):
            stack[-1][2].append(h)
        elif c.keyword == "universe":
            universes += h.split()[1:]
    return stack, universes


def namespaces(text: str, cmds: list[Command], upto: int) -> list[str]:
    """The namespaces open at command `upto`, outermost first, as full names (`A`, `A.B`)."""
    stack, _ = _scope_walk(text, cmds, upto)
    out, cur = [], []
    for k, name, _ in stack:
        if k == "namespace" and name:
            cur = cur + name.split(".")
            out.append(".".join(cur))
    return out


def universes_declared(text: str, cmds: list[Command], upto: int) -> list[str]:
    return _scope_walk(text, cmds, upto)[1]


def scope_lines(text: str, upto: int, cmds: list[Command] | None = None) -> list[str]:
    """The lines a lemma file needs to read a statement made at command `upto` of `text`: `open` for each
    open namespace, then the `open` and `set_option` lines in force, then the universes declared."""
    cmds = cmds if cmds is not None else commands(text)
    stack, universes = _scope_walk(text, cmds, upto)
    ns = namespaces(text, cmds, upto)
    out = ([f"open {' '.join(ns)}"] if ns else []) + [l for _, _, ls in stack for l in ls]
    if universes:
        out.append("universe " + " ".join(dict.fromkeys(universes)))
    return out


def context_text(text: str, upto: int, cmds: list[Command] | None = None) -> str | None:
    """The definitions a hole at command `upto` may use, as a module of their own: the text before that
    command without its theorems that still hold a `sorry` and without examples, the open namespaces and
    sections closed at the end. None when that leaves no declaration (imports and scope lines only)."""
    cmds = cmds if cmds is not None else commands(text)
    keep, decls = [], 0
    code = blank(text)
    for c in cmds[:upto]:
        body = text[c.start:c.end]
        if c.kind == "example" or (c.kind == "theorem" and SORRY.search(code[c.start:c.end])):
            continue
        if c.kind in ("theorem", "def", "other") and c.keyword not in ("macro", "syntax", "elab", "macro_rules", "elab_rules"):
            decls += 1
        keep.append(body)
    if not decls:
        return None
    pre = text[:cmds[0].start] if cmds else ""
    out = pre + "".join(keep)
    stack, _ = _scope_walk(text, cmds, upto)
    for k, name, _ in reversed(stack[1:]):
        out = out.rstrip("\n") + f"\n\nend{(' ' + name) if name else ''}\n"
    return out


def imports_of(text: str) -> list[str]:
    return [m.group(1) for m in IMPORT.finditer(blank(text))]


# ----------------------------------------------------------------------------- 3. the dry assembly

def application(block: str, sig: str, position: str) -> str:
    """The text that replaces a hole: the block's lemma applied to the hypotheses by name, else `apply` and
    `assumption` (a renamed hypothesis, or a signature in `∀` form)."""
    names = explicit_names(sig)
    alts = ([f"exact {block}.solution" + "".join(f" {n}" for n in names)] if names is not None else []) + \
        [f"(apply {block}.solution <;> assumption)"]
    tac = "first | " + " | ".join(alts)
    return f"({tac})" if position == "tactic" else f"(by {tac})"


def block_text(block: str, sig: str, levels: list[str], declared: list[str], body: str | None = None) -> str:
    """`namespace <block>` with the universes the outer file has not declared, the lemma (or a proof file's
    body), `end <block>`."""
    new = [u for u in levels if u not in declared]
    inner = body if body is not None else f"theorem solution {sig} := sorry\n"
    return (f"namespace {block}\n" + (f"universe {' '.join(new)}\n" if new else "") + inner.rstrip("\n") + "\n"
            + f"end {block}\n\n")


def dry_assembly(text: str, holes: list[dict]) -> str:
    """The outline with each given hole replaced by its lemma's application and its block inserted above the
    command that holds it. `holes`: {"offset", "block", "sig", "levels", "position"} (offsets in `text`)."""
    cmds = commands(text)
    by_cmd: dict[int, list[dict]] = {}
    for h in holes:
        i = enclosing(cmds, h["offset"])
        if i is None:
            raise ValueError(f"hole at offset {h['offset']} is in no command")
        by_cmd.setdefault(i, []).append(h)
    out = text
    edits = []                                   # (offset, length, new text), applied from the end
    for i, hs in by_cmd.items():
        declared = universes_declared(text, cmds, i)
        blocks = "".join(block_text(h["block"], h["sig"], h.get("levels") or [], declared)
                         for h in dict((h["block"], h) for h in hs).values())
        edits.append((cmds[i].start, 0, blocks))
        for h in hs:
            if text[h["offset"]:h["offset"] + 5] != "sorry":
                raise ValueError(f"no `sorry` at offset {h['offset']}")
            edits.append((h["offset"], 5, application(h["block"], h["sig"], h["position"])))
    for off, n, new in sorted(edits, key=lambda e: (e[0], -e[1]), reverse=True):
        out = out[:off] + new + out[off + n:]
    return out


def block_spans(text: str, block: str) -> tuple[int, int] | None:
    """(start of the block's first inner line, start of its `end` line) of `namespace <block>`, or None."""
    code = blank(text)
    a = re.search(rf"(?m)^namespace {re.escape(block)}[ \t]*\n", code)
    if not a:
        return None
    b = re.compile(rf"(?m)^end {re.escape(block)}[ \t]*$").search(code, a.end())
    return (a.end(), b.start()) if b else None


# ----------------------------------------------------------------------------- 4. the assembly

def proof_body(proof: str, drop: list[str] = ()) -> str:
    """A lemma file's text without its imports and without the scope lines its file was given (`drop`, which
    the outline has in force already)."""
    code = blank(proof)
    want = {" ".join(d.split()) for d in drop}
    out = []
    for raw, c in zip(proof.splitlines(True), code.splitlines(True)):
        if IMPORT.match(c.rstrip("\n")) or " ".join(raw.split()) in want and raw.strip():
            continue
        out.append(raw)
    return "".join(out).strip("\n") + "\n"


def assemble(base: str, proofs: dict[str, dict]) -> str:
    """`base` (a dry assembly) with each proved block's body replaced: `proofs` maps a block name to
    {"text": the proof file, "drop": the scope lines its file was given, "levels": the universes it needs}.
    The proof files' imports join the base's, after its last import. Blocks not in `proofs` stay as they are."""
    out = base
    imports = []
    for block, p in proofs.items():
        sp = block_spans(out, block)
        if sp is None:
            raise ValueError(f"block {block} is not in the base")
        inner = out[sp[0]:sp[1]]
        keep = "".join(l for l in inner.splitlines(True) if re.match(r"\s*universe\b", l))
        out = out[:sp[0]] + keep + proof_body(p["text"], p.get("drop") or []) + out[sp[1]:]
        imports += imports_of(p["text"])
    have = set(imports_of(out))
    add = [m for m in dict.fromkeys(imports) if m not in have]
    if add:
        imps = list(IMPORT.finditer(blank(out)))
        at = imps[-1].end() if imps else 0
        ins = "".join(f"\nimport {m}" for m in add) if imps else "".join(f"import {m}\n" for m in add)
        out = out[:at] + ins + out[at:]
    return out


def failed_blocks(output: str, rel: str, text: str, blocks: list[str]) -> set[str]:
    """The blocks of a dry assembly that Lean's errors point at: an error inside a block, or at the line of
    a hole's application of it."""
    errs = [m for m in messages(output, rel) if m["sev"] == "error"]
    bad = set()
    lines = text.splitlines()
    for b in blocks:
        sp = block_spans(text, b)
        rng = (text.count("\n", 0, sp[0]) + 1, text.count("\n", 0, sp[1]) + 1) if sp else None
        app_lines = {i + 1 for i, l in enumerate(lines) if f"{b}.solution" in l}
        for e in errs:
            if (rng and rng[0] <= e["line"] <= rng[1]) or e["line"] in app_lines:
                bad.add(b)
    return bad
