# SPDX-License-Identifier: Apache-2.0
"""The project contract: PROJECT.md's status block and the submit lock."""
from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from aisathome_harness.project import (Project, ProjectStatus, Status, SubmitRefused, Task, Workspace,
                                       parse_status, read_status)

TEMPLATE = Path(__file__).resolve().parent.parent / "template" / "PROJECT.md"


def md(status: str, agreement: str | None = None, since: str | None = "2026-01-31") -> str:
    lines = ["# A project", "", f"status: {status}"]
    if since:
        lines.append(f"since: {since}")
    if agreement is not None:
        lines.append(f"agreement: {agreement}")
    return "\n".join(lines + ["", "Free text: results, learnings.", "status: active"]) + "\n"


class Parse(unittest.TestCase):
    def test_every_status(self):
        for s in Status:
            self.assertIs(parse_status(md(s.value)).status, s)

    def test_fields(self):
        st = parse_status(md("active", "the maintainers' issue #12, 2026-01-30"))
        self.assertEqual(st, ProjectStatus(Status.ACTIVE, "2026-01-31", "the maintainers' issue #12, 2026-01-30"))

    def test_no_agreement(self):
        for a in (None, "none", "None", "-", "n/a", "", "none (withdrawn 2026-01-30: the maintainers asked to stop)",
                  "No, not asked yet"):
            self.assertIsNone(parse_status(md("active", a)).agreement, a)
        self.assertEqual(parse_status(md("active", "Nonetheless agreed in #3")).agreement, "Nonetheless agreed in #3")

    def test_only_the_leading_block_counts(self):
        # the free text after the block says "status: active"; the block says paused
        self.assertIs(parse_status(md("paused", "x")).status, Status.PAUSED)

    def test_missing_status_is_an_error(self):
        with self.assertRaises(ValueError):
            parse_status("# A project\n\nsince: 2026-01-31\n")
        with self.assertRaises(ValueError):
            parse_status("# A project\n\nSome text first.\nstatus: active\n")

    def test_unknown_status_is_an_error(self):
        with self.assertRaises(ValueError):
            parse_status(md("running"))

    def test_the_template_parses(self):
        st = read_status(TEMPLATE)
        self.assertIs(st.status, Status.LOCAL_ONLY)
        self.assertIsNone(st.agreement)


class Demo(Project):
    name = "demo"

    def __init__(self, path):
        super().__init__(path)
        self.submitted = []

    def _submit(self, task, ws):
        self.submitted.append(task.id)
        return "ok"


class SubmitLock(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.path = Path(self.d.name) / "PROJECT.md"
        self.p = Demo(self.path)
        self.task = Task("t1")
        self.ws = Workspace(Path("tree"), Path("home"), Path("raw"), Path("state"))

    def tearDown(self):
        self.d.cleanup()

    def submit(self, text):
        self.path.write_text(text)
        return self.p.submit(self.task, self.ws)

    def test_active_with_agreement_submits(self):
        self.assertEqual(self.submit(md("active", "issue #12")), "ok")
        self.assertEqual(self.p.submitted, ["t1"])

    def test_active_without_agreement_is_refused(self):
        with self.assertRaises(SubmitRefused) as e:
            self.submit(md("active", "none"))
        self.assertIn("agreement", str(e.exception))
        self.assertEqual(self.p.submitted, [])

    def test_other_statuses_are_refused_even_with_an_agreement(self):
        for s in ("paused", "local-only", "ended"):
            with self.assertRaises(SubmitRefused, msg=s) as e:
                self.submit(md(s, "issue #12"))
            self.assertIn(s, str(e.exception))
        self.assertEqual(self.p.submitted, [])

    def test_the_status_is_read_at_submit_time(self):
        self.assertEqual(self.submit(md("active", "issue #12")), "ok")
        with self.assertRaises(SubmitRefused):
            self.submit(md("paused", "issue #12"))
        self.assertEqual(self.p.submitted, ["t1"])

    def test_no_project_md_refuses(self):
        with self.assertRaises(FileNotFoundError):
            self.p.submit(self.task, self.ws)
        self.assertEqual(self.p.submitted, [])


if __name__ == "__main__":
    unittest.main()
