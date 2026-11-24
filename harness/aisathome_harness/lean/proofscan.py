# SPDX-License-Identifier: Apache-2.0
"""An allow-list for a proof file written by a model: what it may contain, not what it must not.

`axioms.scan` is a denylist: it finds words a proof must not use. A denylist misses what it does not name, and
Lean has many ways to run code while a file compiles (an elaborator, macro, tactic or simproc registered through an
attribute, `#eval`-like commands, `initialize`) and to change what a statement means (instances, notation,
unification hints). `refusals` reads the file the other way round:

- **Structure.** Every line that starts at column 0 (code only: comments and literal text blanked) must start with
  one of the allowed commands: `import`, `open`, `namespace`, `section`, `end`, `universe`, `variable`, `omit`,
  `include`, `set_option`, and the declarations `theorem`, `lemma`, `def`, `abbrev`, `example` (with `private`,
  `protected`, `noncomputable`, `nonrec`; `axiom` too, which `axioms.scan` and the kernel's axiom check refuse
  in a proof), plus the continuations a declaration may put there (`|` of a pattern
  match, `where`, `termination_by`, `decreasing_by`). Anything else at column 0 is refused: indent a declaration's
  continuation lines.
- **No extension of the language, anywhere.** A command can start in the middle of a line, so these are refused as
  tokens wherever they stand: any attribute (`@[...]`, `attribute [...]`); `syntax`, `macro`, `elab`, `notation`,
  `infix` and their kin; `instance`, `class`, `structure`, `inductive`, `deriving`, `unif_hint`; commands and
  terms that run code while the file compiles (`#`-commands, `run_cmd`, `run_tac`, `by_elab`, `initialize`,
  `simproc`, ...) and syntax quotations; `unsafe`, `partial`, `opaque`, `implemented_by`, `extern`; any name under
  `Lean`, `IO` or `System`; `set_option` outside a short list of options that do not change what is checked.
- **Imports** only in the header (every `import` token, however laid out), only plain names, only from the allowed
  roots (by default Mathlib, its dependencies a proof uses, and the project's own `Definitions`/`Theorems`; the
  caller narrows `Theorems` further); no `module` or `prelude` header (a `module` file's .olean exports its
  theorems as axioms, and its proofs are not a proof's).

Lines given by the task (`given`: its preamble and the statement's own lines, e.g. a `local notation` or an
`attribute [local instance]` the statement needs) are allowed as they are, wherever they stand, and so is
`attribute [local instance] Classical.propDecidable ...` with `Classical` instances only (Mathlib's idiom for
classical decidability; a step cut out of a proof carries it without its task's preamble).

`refusals` returns {reason: [line, ...]} (1-based), empty when the file passes. It does not look for `sorry`,
`admit`, `native_decide` or `axiom` (whether a proof is complete): that is `axioms.scan`, and the kernel's axiom
check after the build. Neither scan is the last word: the build runs in a throwaway container (sandbox.jail), and
the verdict comes from a kernel replay against a target compiled without the proof (lean.replay). Standard library
only.
"""
from __future__ import annotations

import re

from .lexer import braced_strings, code_mask

COMMANDS = ("import", "open", "namespace", "section", "end", "universe", "variable", "omit", "include",
            "set_option", "theorem", "lemma", "def", "abbrev", "example", "private", "protected", "noncomputable",
            "nonrec", "where", "termination_by", "decreasing_by", "axiom")
IMPORT_ROOTS = ("Mathlib", "Definitions", "Theorems", "Batteries", "Aesop", "Qq", "Plausible")
# options that change messages, limits and linters, never what the kernel checks or what code runs
OPTIONS = re.compile(r"(?:maxHeartbeats|maxRecDepth|maxSynthPendingDepth|synthInstance\.\w+|linter(?:\.[\w.]+)?|"
                     r"autoImplicit|relaxedAutoImplicit|pp\.[\w.]+|trace\.[\w.]+|push_neg\.\w+|simprocs|"
                     r"exponentiation\.threshold|backward\.[\w.]+|diagnostics(?:\.threshold)?|warningAsError|"
                     r"profiler(?:\.threshold)?|tactic\.hygienic)$")

STRUCTURE = "a line at column 0 that is not an allowed command (only imports, open, namespace, section, end, " \
            "universe, variable, set_option, and theorem, lemma, def, abbrev or example declarations; indent " \
            "continuation lines)"
ATTRIBUTE = "an attribute (`@[...]` or `attribute [...]`)"
SYNTAX = "a syntax extension (syntax, notation, macro, elab and their kin)"
INSTANCE = "an instance or a new type (instance, class, structure, inductive, deriving, unif_hint)"
META = "code that runs while the file compiles (a `#` command, run_cmd, run_tac, by_elab, initialize, simproc, " \
       "a syntax quotation)"
LOWLEVEL = "unsafe, partial, opaque, implemented_by or extern"
LEAN_NAME = "a name under `Lean`, `IO` or `System`"
OPTION = "a `set_option` outside the allowed ones (limits, linters, pretty-printing, autoImplicit)"
IMPORT = "an import outside the allowed modules (or not a plain name), an import after the first command, or a " \
         "`module`/`prelude` header"
BRACED = "a string literal with `{` (possible interpolation, whose code the scan cannot be sure to see)"

_B = r"(?<![\w.'!?«])"       # a token starts here: not inside a longer name (`Foo.instance`, `h'`)
_E = r"(?![\w'!?»])"         # and ends here
TOKENS = [
    (r"@\[", ATTRIBUTE),
    (_B + r"attribute" + _E, ATTRIBUTE),
    (_B + r"(?:syntax|macro|macro_rules|elab|elab_rules|notation\d?|infix[lr]?|prefix|postfix|declare_syntax_cat|"
          r"binder_predicate|declare_simp_like_tactic|syntax_cat|scoped\[)" + _E, SYNTAX),
    (_B + r"(?:instance|class|structure|inductive|coinductive|mutual|deriving|unif_hint|export|irreducible_def)" + _E,
     INSTANCE),
    # `#` commands (the toolchain's, Mathlib's and its dependencies'); a `#` before another name is Mathlib's
    # cardinality notation (`#s`), and an unknown command at column 0 is a STRUCTURE refusal anyway
    (r"(?<![\w.'!?])#(?:adaptation_note|aesop_\w+|allow_unused_tactic|browse|check\w*|clear_deprecations|"
     r"click_suggestions|computability|conv|count_heartbeats!?|create_deprecated_module|defeq_abuse|discr_tree_\w+|"
     r"dump_\w+|ensure_defeq|eval!?|exit|explode|expr_graph|find\w*|grind_lint|guard\w*|help|html|import_\w+|"
     r"info_trees|instances|lint|list_linters|leansearch|loogle|moogle|log_keys|long_\w+|minimize_imports|"
     r"min_imports|norm_num|parse|print\w*|pull|push\w*|reduce|redundant_imports|reexport|reset_min_imports|"
     r"rw\?\?|sample|search|show_\w+|simp|\w+_tags|statesearch|string_diagram|synth|test|time|trans_imports|"
     r"unfold\?|unused_transitive_imports|version|whats_new|where|whnfR?|widget|with_exporting)(?![\w'!?])", META),
    (_B + r"(?:run_cmd|run_elab|run_meta|run_tac|by_elab|initialize|builtin_initialize|\w*simproc\w*|"
          r"register_\w+|declare_\w+|test_extern|add_aesop_rules|erase_aesop_rules|grind_pattern|"
          r"initialize_simps_projections|eval%|elab%)" + _E, META),
    (r"`\(|`\[|`\{|``\(", META),
    (_B + r"(?:unsafe\w*|partial|opaque|implemented_by|extern|csimp)" + _E, LOWLEVEL),
    (_B + r"(?:Lean|IO|System|EIO|BaseIO)(?:\.|" + _E + r")", LEAN_NAME),
]
_LINE_START = re.compile(r"(?m)^(?=\S)")
_FIRST_WORD = re.compile(r"[^\s(\[{:]+")
_HEADER_IMPORT = re.compile(r"\s*import(?![\w'!?»])\s*(\S*)")
_ANY_IMPORT = re.compile(_B + r"import" + _E)
_MODULE = re.compile(r"\s*(module|prelude)(?![\w'!?»])")
_PLAIN = re.compile(r"[A-Za-z_][A-Za-z0-9_']*(?:\.[A-Za-z_][A-Za-z0-9_']*)*")
_SET_OPTION = re.compile(_B + r"set_option\s+([^\s]+)")


_CLASSICAL = re.compile(r"attribute \[local instance(?: \d+)?\](?: Classical\.\w+)+")


def _norm(line: str) -> str:
    return " ".join(line.split())


def given_lines(*texts: str) -> set[str]:
    """The code lines (comments blanked, whitespace collapsed) of the task's own text: its preamble and the lines
    of its statement. A proof may repeat them as they are."""
    out = set()
    for t in texts:
        for line in code_mask(t or "").splitlines():
            if line.strip():
                out.add(_norm(line))
    return out


def refusals(text: str, given: set[str] | frozenset = frozenset(), import_roots: tuple[str, ...] = IMPORT_ROOTS,
             commands: tuple[str, ...] = COMMANDS) -> dict[str, list[int]]:
    """Why `text` is refused, with the lines of each reason; empty when it passes (see the module's docstring).
    `given`: lines the task gave (given_lines), allowed as they are."""
    code = code_mask(text)                       # comments and literal text blanked; offsets kept
    lines = code.splitlines()
    out: dict[str, list[int]] = {}

    def hit(why: str, n: int):
        if n not in out.setdefault(why, []):
            out[why].append(n)

    exempt = {i for i, line in enumerate(lines, 1)
              if line.strip() and (_norm(line) in given or _CLASSICAL.fullmatch(_norm(line)))}
    for i, line in enumerate(lines, 1):
        if not line.strip() or i in exempt:
            continue
        if not line[0].isspace():
            word = _FIRST_WORD.match(line)
            w = word.group(0) if word else line[0]
            if w != "|" and w not in commands:
                hit(STRUCTURE, i)
    # the header, as Lean reads it: `import <name>` tokens (any layout: indented, several on a line) before anything
    # else; every name plain (no `«..»` component a file path could climb out with) and under an allowed root; an
    # `import` token after the header, or a `module`/`prelude` header, is refused
    line_of = lambda pos: code.count("\n", 0, pos) + 1
    if _MODULE.match(code):
        hit(IMPORT, line_of(_MODULE.match(code).start(1)))
    pos = 0
    while (m := _HEADER_IMPORT.match(code, pos)):
        name, pos = m.group(1), m.end()
        if line_of(m.start(1)) not in exempt and not (_PLAIN.fullmatch(name) and name.split(".")[0] in import_roots):
            hit(IMPORT, line_of(m.start(1)))
    for m in _ANY_IMPORT.finditer(code, pos):
        hit(IMPORT, line_of(m.start()))
    for pat, why in TOKENS:
        for m in re.finditer(pat, code):
            n = code.count("\n", 0, m.start()) + 1
            if n not in exempt:
                hit(why, n)
    for m in _SET_OPTION.finditer(code):
        n = code.count("\n", 0, m.start()) + 1
        if n not in exempt and not OPTIONS.match(m.group(1)):
            hit(OPTION, n)
    braced = [n for n in braced_strings(text) if n not in exempt]
    if braced:
        out[BRACED] = braced
    return {k: sorted(v) for k, v in out.items()}
