# SPDX-License-Identifier: Apache-2.0
"""integration_queue.main's probe: a task queued while it probes is given back at once, under the probe's
worker name (the gateway's lease holds only id, expires_at and ttl_seconds), and the run refuses to start."""
from __future__ import annotations

import io
import unittest
from contextlib import redirect_stdout
from unittest import mock

from aisathome_harness import queue as Q
from tests import integration_queue as I


class FakeClient:
    def __init__(self, *a, **k):
        self.hold_refused = False
        self.given: list[tuple] = []

    def list(self, **k):
        return Q.Reply(200, {"tasks": []})

    def claim(self, worker, lease_seconds, **k):
        self.worker = worker
        return Q.Reply(200, {"task": {"id": "x1"}, "lease": {"id": "L1", "expires_at": "2026-11-15T00:00:00Z",
                                                              "ttl_seconds": 30}})

    def give_back(self, task, worker, lease, reason=""):
        self.given.append((task, worker, lease))
        return Q.Reply(200, {})


class Probe(unittest.TestCase):
    def test_a_task_queued_while_probing_goes_back_under_the_probes_name(self):
        made = []
        with mock.patch.object(I.Q, "QueueClient", lambda *a, **k: made.append(FakeClient()) or made[-1]), \
                redirect_stdout(io.StringIO()) as out:
            rc = I.main(["--gateway", "http://127.0.0.1:1", "--key-file", "k", "--project", "p"])
        self.assertEqual(rc, 2)
        self.assertIn("a task was queued while probing", out.getvalue())
        self.assertEqual(made[0].given, [("x1", made[0].worker, "L1")])
        self.assertTrue(made[0].worker.endswith("-itest-probe"))


if __name__ == "__main__":
    unittest.main()
