# SPDX-License-Identifier: Apache-2.0
"""Lean text that tries to hide code from the text checks (lexer, axioms.scan, sorry_text)."""
from __future__ import annotations

import unittest

from aisathome_harness.lean import axioms, sorry_text
from aisathome_harness.lean.lexer import code_mask


class LeanText(unittest.TestCase):
    """Text that tries to hide code from the scan. For each: what the scan says now, and whether the
    kernel-level check (axioms.check_command: the type and the axioms of `solution`) would catch it anyway."""

    CASES = [
        # (text, what the scan must find, does the kernel check catch it without the scan?)
        ('def banner := "/-"\naxiom bad : False\ntheorem solution : False := bad\n', "declares an axiom", True),
        ('def banner := "--" axiom bad : False\n', "declares an axiom", True),           # two commands, one line
        ("theorem t : True := by run_tac pure ()\n", "meta-level command", False),
        ('def x := "/-"\n#eval Lean.Elab.Command.liftTermElabM (pure ())\n', "meta-level command", False),
        ("def «/-» := 1\nset_option debug.skipKernelTC true in\ntheorem solution : False := sorry\n", "debug.skipKernelTC", False),
        ('def s := s!"{(sorry : Nat)} /- "\n', "sorry", True),
        ("def c := '\"'\nrun_cmd pure ()\n", "meta-level command", False),
        ('def r := r#"/-"#\nelab "x" : term => pure (Lean.mkConst `True)\n', "elaborator extension", False),
        ('def q := "\\"/-"\ntheorem t : 1 = 1 := by native_decide\n', "native_decide", True),
    ]

    def test_the_scan_sees_through_literals(self):
        for text, want, _ in self.CASES:
            self.assertIn(want, axioms.scan(text), text)

    def test_comments_are_not_code_but_strings_count(self):
        """Fail closed: a comment may say anything; a string that says `sorry` is refused (a false positive
        on a string is acceptable, a miss is not)."""
        self.assertEqual(axioms.scan("-- axiom x\n/- #eval -/\ntheorem h' : True := trivial\n"), [])
        self.assertEqual(axioms.scan('def msg := "no sorry here"\n'), ["sorry"])

    INTERPOLATED = [
        # code inside interpolation, written so that a lexer that misses the interpolation blanks it
        'def x := s! "{(by run_tac pure () : Nat)}"\n',                    # whitespace after s!
        'def x := s!\n  "{(by run_tac pure () : Nat)}"\n',                 # a newline after s!
        'def x : Nat := by\n  throwError "{ "/-" } x"\n#eval 1\n-- -/\n',  # a macro that takes it without !
        'def a := "{ /-"\n#eval danger\n-- -/\n',                         # a plain string with a brace
        'def p := panic! "{"\nrun_cmd pure ()\n',
    ]

    def test_braces_in_strings_fail_closed(self):
        for text in self.INTERPOLATED:
            self.assertIn(axioms.BRACED, axioms.scan(text), text)
        self.assertEqual(axioms.scan_lines('def a := 1\ndef b := "x {y}"\n')[axioms.BRACED], [2])
        self.assertEqual(axioms.scan('def r := r#"{}"#\n'), [])      # raw strings cannot interpolate

    def test_interpolation_after_whitespace_is_code_for_the_mask(self):
        m = code_mask('def x := s! "{(by run_tac pure () : Nat)}"\n')
        self.assertIn("run_tac", m)

    def test_comments_only_mode_keeps_strings(self):
        self.assertEqual(axioms.blank_comments('def a := "/-" -- c\n'), 'def a := "/-"     \n')

    def test_the_sorry_check_too(self):
        text = 'def banner := "/-"\ntheorem a : True := by\n  sorry\n'
        diff = "diff --git a/A.lean b/A.lean\n+++ b/A.lean\n@@ -0,0 +1,3 @@\n" + "".join("+" + l + "\n" for l in text.splitlines())
        self.assertEqual([h["line"] for h in sorry_text.hits(diff, lambda p: text)], [3])

    def test_mask_keeps_offsets(self):
        for text, _, _ in self.CASES:
            m = code_mask(text)
            self.assertEqual(len(m), len(text))
            self.assertEqual(m.count("\n"), text.count("\n"))


if __name__ == "__main__":
    unittest.main()
