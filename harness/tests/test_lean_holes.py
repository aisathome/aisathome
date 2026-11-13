# SPDX-License-Identifier: Apache-2.0
"""lean.holes on fake Lean output: the stand-in copy, extraction, statements and keys, commands and context,
the dry assembly and the assembly. No Lean runs here; the compile is the project's."""
from __future__ import annotations

import unittest

from aisathome_harness.lean import holes as H

OUTLINE = """import Mathlib.Tactic.Ring
import Mathlib.Order.Basic

universe u

open Nat

namespace Foo

/-- a definition the holes may use -/
def double (n : ℕ) : ℕ := n + n

@[simp]
theorem helper (n : ℕ) (h : 0 < n) : 0 < double n := by
  sorry

theorem proved (n : ℕ) : double n = 2 * n := by unfold double; ring

end Foo

open Foo in
theorem solution (n : ℕ) (h : 0 < n) : 0 < double n ∧ 0 < n := by
  constructor
  · exact helper n h
  · have k : 0 < n := sorry
    sorry
"""


def lake_out(rel: str, msgs: list[tuple[str, int, int, str]]) -> str:
    """Lake's output: `<sev>: ./././<rel>:<line>:<col>: <text>` with further lines as they come."""
    out = ["✔ [10/12] Built Definitions.Def_x", "⚠ [11/12] Built Solutions.Sol_x"]
    for sev, ln, col, text in msgs:
        out.append(f"{sev}: ./././{rel}:{ln}:{col}: {text}")
    out.append("Build completed successfully.")
    return "\n".join(out) + "\n"


class StandinCopy(unittest.TestCase):
    def test_the_copy_raises_the_heartbeat_budget_before_the_outline(self):
        """Trial 4's C12: 8 holes in one proof ran out of the default 200,000 heartbeats at the 4th extraction."""
        c = H.standin_copy(OUTLINE)
        head, rest = c.text.split(H.STANDIN_HEADER)
        self.assertIn(f"set_option maxHeartbeats {H.STANDIN_HEARTBEATS}\n", H.STANDIN_HEADER)
        self.assertGreater(H.STANDIN_HEARTBEATS, 200_000)
        self.assertIn("theorem solution", rest)
        ln, col = c.copy_pos[0]
        self.assertEqual(c.text.splitlines()[ln - 1][col:col + len(H.STANDIN)], H.STANDIN)

    def test_every_code_sorry_becomes_the_standin(self):
        c = H.standin_copy(OUTLINE + "-- a sorry in a comment\n")
        self.assertEqual(c.text.count(H.STANDIN + "\n"), 3)
        self.assertNotIn(" sorry\n", c.text.split(H.STANDIN_HEADER)[1].replace("-- a sorry", ""))
        self.assertIn("import Mathlib.Tactic.ExtractGoal\nimport Mathlib.Tactic.Trace\n", c.text)
        self.assertEqual(len(c.holes), 3)
        self.assertEqual([h[1] for h in c.holes], [15, 25, 26])           # the outline's lines

    def test_positions_map_to_the_copy(self):
        c = H.standin_copy(OUTLINE)
        lines = c.text.splitlines()
        for (ln, col) in c.copy_pos:
            self.assertTrue(lines[ln - 1][col:].startswith(H.STANDIN), (ln, col, lines[ln - 1]))

    def test_two_holes_on_one_line_shift_the_second(self):
        t = "import Mathlib\n\ntheorem solution : True ∧ True := ⟨sorry, sorry⟩\n"
        c = H.standin_copy(t)
        line = c.text.splitlines()[c.copy_pos[1][0] - 1]
        self.assertTrue(line[c.copy_pos[1][1]:].startswith(H.STANDIN))
        self.assertEqual(c.copy_pos[0][0], c.copy_pos[1][0])


class Extraction(unittest.TestCase):
    rel = "Solutions/Sol_x.lean"

    def out_for(self, c: H.Copy, per_hole: list[list[str]]) -> str:
        msgs = []
        for (ln, col), texts in zip(c.copy_pos, per_hole):
            for t in texts:
                msgs.append(("info", ln, col, t))
        msgs.append(("warning", c.copy_pos[0][0] - 2, 8, "declaration uses `sorry`"))
        return lake_out(self.rel, msgs)

    def test_statements_positions_and_reasons(self):
        c = H.standin_copy(OUTLINE)
        out = self.out_for(c, [
            [H.MARK + "tactic", f"theorem {H.EXTRACTED} (n : ℕ) (h : 0 < n) :\n  0 < Foo.double n := sorry"],
            [H.MARK + "term", f"theorem {H.EXTRACTED}.{{u_1}} {{α : Sort u_1}} (n : ℕ) (h : 0 < n) : 0 < n := sorry"],
            [],                                                       # never reached
        ])
        hs = H.extracted(out, self.rel, c)
        self.assertEqual([h.position for h in hs], ["tactic", "term", None])
        self.assertEqual(hs[0].statement["sig"], "(n : ℕ) (h : 0 < n) :\n  0 < Foo.double n")
        self.assertEqual(hs[1].statement["levels"], ["u_1"])
        self.assertTrue(hs[0].extracted and hs[1].extracted)
        self.assertIn("not reached", hs[2].why)

    def test_a_hole_elaborated_twice_is_not_extracted(self):
        c = H.standin_copy(OUTLINE)
        st = f"theorem {H.EXTRACTED} (n : ℕ) : 0 < n := sorry"
        hs = H.extracted(self.out_for(c, [[H.MARK + "tactic", st, H.MARK + "tactic", st], [], []]), self.rel, c)
        self.assertIn("2 times", hs[0].why)

    def test_data_metavariables_and_failed_extraction(self):
        c = H.standin_copy(OUTLINE)
        hs = H.extracted(self.out_for(c, [
            [H.MARK + "term", f"def {H.EXTRACTED} (n : ℕ) : ℕ := sorry"],
            [H.MARK + "term", f"theorem {H.EXTRACTED} (n : ℕ) : n = ?m.12 := sorry"],
            [H.MARK + "tactic", "n : ℕ\n⊢ 0 < n"],
        ]), self.rel, c)
        self.assertIn("data", hs[0].why)
        self.assertIn("metavariable", hs[1].why)
        self.assertIn("goal printed", hs[2].why)
        self.assertEqual(hs[2].goal, "n : ℕ\n⊢ 0 < n")

    def test_messages_of_other_files_do_not_count(self):
        c = H.standin_copy(OUTLINE)
        ln, col = c.copy_pos[0]
        out = lake_out("Definitions/Def_y.lean", [("info", ln, col, H.MARK + "tactic")])
        self.assertTrue(all(not h.extracted for h in H.extracted(out, self.rel, c)))


class Statements(unittest.TestCase):
    def test_binders_and_explicit_names(self):
        sig = "{α : Type u_1} [inst : Ring α] (a b : α) (h : a = b ∧ (b : α) = a) ⦃c : α⦄ : a * 2 = a + a"
        groups, ty = H.split_signature(sig)
        self.assertEqual(len(groups), 5)
        self.assertEqual(ty, "a * 2 = a + a")
        self.assertEqual(H.explicit_names(sig), ["a", "b", "h"])
        self.assertIsNone(H.explicit_names(": ∀ (n : ℕ), 0 < n + 1"))
        self.assertEqual(H.explicit_names(": 1 = 1"), [])

    def test_the_key_ignores_layout_and_comments_but_not_context_or_env(self):
        a = H.statement_key("(n : ℕ) :\n   0 < n + 1", "import Mathlib\n", "env1")
        self.assertEqual(a, H.statement_key("(n : ℕ) : 0 < n + 1 -- c", "import  Mathlib", "env1"))
        self.assertNotEqual(a, H.statement_key("(n : ℕ) : 0 < n + 1", "import Mathlib\ndef x := 1", "env1"))
        self.assertNotEqual(a, H.statement_key("(n : ℕ) : 0 < n + 1", "import Mathlib\n", "env2"))
        self.assertNotEqual(a, H.statement_key("(m : ℕ) : 0 < m + 1", "import Mathlib\n", "env1"))
        self.assertEqual(H.signature_key("(n : ℕ) : 0 < n"), H.signature_key("(n : ℕ)  :  0 < n"))


class Commands(unittest.TestCase):
    def test_commands_kinds_names_and_docstrings(self):
        cs = H.commands(OUTLINE)
        kinds = [(c.kind, c.keyword, c.name) for c in cs]
        self.assertIn(("def", "def", "double"), kinds)
        self.assertIn(("theorem", "theorem", "helper"), kinds)
        d = next(c for c in cs if c.name == "double")
        self.assertTrue(OUTLINE[d.start:].startswith("/-- a definition"))
        h = next(c for c in cs if c.name == "helper")
        self.assertTrue(OUTLINE[h.start:].startswith("@[simp]"))
        s = next(c for c in cs if c.name == "solution")
        self.assertTrue(OUTLINE[s.start:].startswith("open Foo in\ntheorem solution"))

    def test_scope_lines_and_context(self):
        cs = H.commands(OUTLINE)
        hi = next(i for i, c in enumerate(cs) if c.name == "helper")
        self.assertEqual(H.scope_lines(OUTLINE, hi, cs), ["open Foo", "open Nat", "universe u"])
        ctx = H.context_text(OUTLINE, hi, cs)
        self.assertIn("def double", ctx)
        self.assertTrue(ctx.rstrip().endswith("end Foo"))
        si = next(i for i, c in enumerate(cs) if c.name == "solution")
        ctx = H.context_text(OUTLINE, si, cs)
        self.assertIn("theorem proved", ctx)            # proved: kept
        self.assertNotIn("theorem helper", ctx)         # still a `sorry`: left out
        self.assertEqual(ctx.count("end Foo"), 1)
        self.assertIsNone(H.context_text("import Mathlib\n\nopen Nat\n\ntheorem solution : 1 = 1 := sorry\n", 2))


class Assembly(unittest.TestCase):
    def holes(self, text):
        offs = H.sorry_offsets(text)
        return [{"offset": offs[0], "block": "P2MHole_a", "sig": "(n : ℕ) (h : 0 < n) : 0 < Foo.double n",
                 "levels": [], "position": "tactic"},
                {"offset": offs[1], "block": "P2MHole_b", "sig": "{α : Sort u_1} (n : ℕ) (h : 0 < n) : 0 < n",
                 "levels": ["u_1", "u"], "position": "term"}]

    def test_dry_assembly_inserts_blocks_and_applications(self):
        dry = H.dry_assembly(OUTLINE, self.holes(OUTLINE))
        self.assertEqual(len(H.sorry_offsets(dry)), 3)            # two lemmas with sorry, one hole left
        self.assertIn("namespace P2MHole_a\ntheorem solution (n : ℕ) (h : 0 < n) : 0 < Foo.double n := sorry\n"
                      "end P2MHole_a\n\n@[simp]\ntheorem helper", dry)
        self.assertIn("(first | exact P2MHole_a.solution n h | (apply P2MHole_a.solution <;> assumption))", dry)
        self.assertIn("have k : 0 < n := (by first | exact P2MHole_b.solution n h |", dry)
        self.assertIn("namespace P2MHole_b\nuniverse u_1\n", dry)     # u is declared in the file already
        self.assertLess(dry.index("namespace P2MHole_b"), dry.index("open Foo in\ntheorem solution"))

    def test_assembly_replaces_bodies_and_hoists_imports(self):
        dry = H.dry_assembly(OUTLINE, self.holes(OUTLINE))
        proof = ("import Mathlib.Tactic.Ring\nimport Mathlib.Tactic.Linarith\n\nopen Foo\nuniverse u_1\n\n"
                 "theorem solution (n : ℕ) (h : 0 < n) : 0 < Foo.double n := by\n  unfold double; omega\n")
        out = H.assemble(dry, {"P2MHole_a": {"text": proof, "drop": ["open Foo", "universe u_1"]}})
        self.assertIn("import Mathlib.Order.Basic\nimport Mathlib.Tactic.Linarith\n", out)
        self.assertEqual(out.count("import Mathlib.Tactic.Ring"), 1)
        a, b = H.block_spans(out, "P2MHole_a")
        self.assertEqual(out[a:b], "theorem solution (n : ℕ) (h : 0 < n) : 0 < Foo.double n := by\n  unfold double; omega\n")
        self.assertIn("theorem solution {α : Sort u_1} (n : ℕ) (h : 0 < n) : 0 < n := sorry", out)   # b untouched
        with self.assertRaises(ValueError):
            H.assemble(dry, {"P2MHole_zz": {"text": proof}})

    def test_failed_blocks_from_errors(self):
        dry = H.dry_assembly(OUTLINE, self.holes(OUTLINE))
        ln = next(i + 1 for i, l in enumerate(dry.splitlines()) if "exact P2MHole_b.solution" in l)
        out = lake_out("Solutions/Sol_x.lean", [("error", ln, 30, "unknown identifier 'h'"),
                                                 ("warning", 3, 0, "declaration uses `sorry`")])
        self.assertEqual(H.failed_blocks(out, "Solutions/Sol_x.lean", dry, ["P2MHole_a", "P2MHole_b"]), {"P2MHole_b"})


if __name__ == "__main__":
    unittest.main()
