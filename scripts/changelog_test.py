#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for scripts/changelog.py: python3 scripts/changelog_test.py (standard library only)."""

import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import changelog  # noqa: E402

HEAD = """# Changelog

Intro.

## [Unreleased]
"""
LINKS = """[Unreleased]: https://example.org/r/compare/v0.6.0...main
[v0.6.0]: https://example.org/r/compare/v0.5.0...v0.6.0
"""
OLD = """## [v0.6.0] — 2026-11-24

### Added

- **Old.** (node)

"""


def fragment(section, body):
    return f"---\nsection: {section}\n---\n\n{body}\n"


class Repo:
    """A temporary checkout with CHANGELOG.md and changelog.d/."""

    def __init__(self, tc, changelog_text, frags=None):
        self.tmp = tempfile.TemporaryDirectory()
        tc.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "CHANGELOG.md").write_text(changelog_text, encoding="utf-8")
        (self.root / "changelog.d").mkdir()
        (self.root / "changelog.d" / "README.md").write_text("# not a fragment\n", encoding="utf-8")
        for name, text in (frags or {}).items():
            (self.root / "changelog.d" / name).write_text(text, encoding="utf-8")

    def run(self, *args):
        env = dict(os.environ, CHANGELOG_ROOT=str(self.root))
        script = Path(__file__).resolve().parent / "changelog.py"
        return subprocess.run([sys.executable, str(script), *args], env=env, capture_output=True, text=True)

    def git(self, *args):
        subprocess.run(["git", "-c", "user.email=t@example.org", "-c", "user.name=t", *args],
                       cwd=self.root, check=True, capture_output=True)

    def text(self):
        return (self.root / "CHANGELOG.md").read_text(encoding="utf-8")


class FragmentTest(unittest.TestCase):
    def check(self, name, text):
        r = Repo(self, HEAD + "\n" + OLD + LINKS, {name: text})
        return r.run("check")

    def test_good(self):
        res = self.check("a-fix.md", fragment("Fixed", "- **A fix.** Two\n  lines. (node)"))
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("1 changelog fragment(s)", res.stdout)

    def test_bad(self):
        for name, text, want in [
            ("A_Fix.md", fragment("Fixed", "- x"), "name it"),
            ("a.txt", fragment("Fixed", "- x"), "name it"),
            ("a.md", "- x\n", "front matter"),
            ("a.md", fragment("Bugfix", "- x"), "is not one of"),
            ("a.md", "---\nsection: Fixed\nscope: node\n---\n- x\n", "one field"),
            ("a.md", fragment("Fixed", "A sentence."), "bullet"),
            ("a.md", fragment("Fixed", "- x\n\n### Fixed\n- y"), "no headings"),
        ]:
            with self.subTest(name=name, want=want):
                res = self.check(name, text)
                self.assertEqual(res.returncode, 1)
                self.assertIn(want, res.stderr)


class FoldTest(unittest.TestCase):
    def test_into_empty_unreleased(self):
        r = Repo(self, HEAD + "\n" + OLD + LINKS, {
            "b-new.md": fragment("Added", "- **New.** (site)"),
            "a-fix.md": fragment("Fixed", "- **Fix.**\n  more. (node)"),
            "c-new.md": fragment("Added", "- **Newer.** (gateway)"),
        })
        res = r.run("fold", "v0.7.0", "--date", "2026-12-01")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(r.text(), HEAD + """
## [v0.7.0] — 2026-12-01

### Added

- **New.** (site)
- **Newer.** (gateway)

### Fixed

- **Fix.**
  more. (node)

""" + OLD + """[Unreleased]: https://example.org/r/compare/v0.7.0...main
[v0.7.0]: https://example.org/r/compare/v0.6.0...v0.7.0
[v0.6.0]: https://example.org/r/compare/v0.5.0...v0.6.0
""")
        self.assertEqual(sorted(p.name for p in (r.root / "changelog.d").iterdir()), ["README.md"])

    def test_existing_unreleased_is_kept_and_merged(self):
        unreleased = """
Lead paragraph.

### Fixed

- **Old fix.** (node)

### Added

- **Old add 1.** (site)

### Known issues

- **Unsigned.** (node)

### Added

- **Old add 2.** (site)

"""
        r = Repo(self, HEAD + unreleased + OLD + LINKS, {"z.md": fragment("Added", "- **Frag.** (node)")})
        res = r.run("fold", "v0.7.0", "--date", "2026-12-01")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("""## [Unreleased]

## [v0.7.0] — 2026-12-01

Lead paragraph.

### Added

- **Old add 1.** (site)
- **Old add 2.** (site)
- **Frag.** (node)

### Fixed

- **Old fix.** (node)

### Known issues

- **Unsigned.** (node)

## [v0.6.0]""", r.text())

    def test_no_older_version_and_no_links(self):
        r = Repo(self, HEAD, {"a.md": fragment("Removed", "- **Gone.** (gateway)")})
        res = r.run("fold", "v0.1.0", "--date", "2026-12-01")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(r.text(), HEAD + "\n## [v0.1.0] — 2026-12-01\n\n### Removed\n\n- **Gone.** (gateway)\n\n")

    def test_dry_run_changes_nothing(self):
        before = HEAD + "\n" + OLD + LINKS
        r = Repo(self, before, {"a.md": fragment("Fixed", "- **Fix.** (node)")})
        res = r.run("fold", "v0.7.0", "--dry-run")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("## [v0.7.0] — ", res.stdout)
        self.assertEqual(r.text(), before)
        self.assertTrue((r.root / "changelog.d" / "a.md").exists())

    def test_refusals(self):
        for frags, version, want in [
            ({}, "v0.7.0", "nothing to release"),
            ({"a.md": fragment("Fixed", "- x")}, "v0.6.0", "already has a section"),
            ({"a.md": fragment("Fixed", "- x")}, "0.7.0", "want a tag"),
        ]:
            with self.subTest(want=want):
                r = Repo(self, HEAD + "\n" + OLD + LINKS, frags)
                res = r.run("fold", version)
                self.assertEqual(res.returncode, 1)
                self.assertIn(want, res.stderr)
        r = Repo(self, "# Changelog\n", {"a.md": fragment("Fixed", "- x")})
        self.assertIn("no '## [Unreleased]'", r.run("fold", "v0.7.0").stderr)


class BaseTest(unittest.TestCase):
    def repo_at_base(self):
        r = Repo(self, HEAD + "\n" + OLD + LINKS)
        r.git("init", "-q")
        r.git("add", "-A")
        r.git("commit", "-q", "-m", "base")
        return r

    def test_fragment_only_passes(self):
        r = self.repo_at_base()
        (r.root / "changelog.d" / "a.md").write_text(fragment("Fixed", "- x"), encoding="utf-8")
        res = r.run("check", "--base", "HEAD")
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_older_section_edit_passes(self):
        r = self.repo_at_base()
        (r.root / "CHANGELOG.md").write_text(r.text().replace("**Old.**", "**Older.**"), encoding="utf-8")
        self.assertEqual(r.run("check", "--base", "HEAD").returncode, 0)

    def test_direct_unreleased_edit_fails(self):
        r = self.repo_at_base()
        (r.root / "CHANGELOG.md").write_text(
            r.text().replace("## [Unreleased]\n", "## [Unreleased]\n\n### Fixed\n\n- **Direct.** (node)\n"),
            encoding="utf-8")
        res = r.run("check", "--base", "HEAD")
        self.assertEqual(res.returncode, 1)
        self.assertIn("edits the Unreleased section", res.stderr)

    def test_release_passes(self):
        r = Repo(self, HEAD + "\n### Fixed\n\n- **Pending.** (node)\n\n" + OLD + LINKS)
        r.git("init", "-q")
        r.git("add", "-A")
        r.git("commit", "-q", "-m", "base")
        self.assertEqual(r.run("fold", "v0.7.0").returncode, 0)
        res = r.run("check", "--base", "HEAD")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("a release", res.stdout)


if __name__ == "__main__":
    unittest.main()
