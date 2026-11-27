#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for scripts/prove2me_reuse_backfill.py: python3 scripts/prove2me_reuse_backfill_test.py
(standard library only; a stand-in gateway on localhost)."""

import copy
import json
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

sys.path.insert(0, str(Path(__file__).resolve().parent))
import prove2me_reuse_backfill as B  # noqa: E402

KEY = "test-project-key"


def result(rid, item, campaign, labels, **more):
    return {"project": "prove2me", "id": rid, "source": "prove2me", "campaign": campaign, "item": item, "state": "accepted",
            "labels": labels, "reuse": None, **more}


def records():
    """The results as the gateway held them before the backfill: N02 and N05 marked as reuses by their
    labels, N18 as a proof, and a proof by the model."""
    return [
        result("r-0000000000000002", "p2m-b1d59e", "t6p", {"alias": "N02", "kind": "reuse", "relation": "same", "trial": "t6p",
                                                           "reuses": B.REUSES[0]["reuse"]["name"]}),
        result("r-0000000000000005", "p2m-f181df", "t6", {"alias": "N05", "kind": "reuse", "relation": "part", "trial": "t6",
                                                          "reuses": B.REUSES[1]["reuse"]["name"]}),
        result("r-0000000000000018", "p2m-faf1e3", "t9", {"alias": "N18", "kind": "proof", "trial": "t9"}),
        result("r-0000000000000019", "p2m-a3c1d0", "t9", {"alias": "N19", "kind": "proof", "trial": "t9"}),
    ]


class Stub(BaseHTTPRequestHandler):
    """GET results?item=, PATCH results/{id} (key required), as the gateway answers them."""

    store: list = []
    patches: list = []

    def log_message(self, *a):
        pass

    def send(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path != "/v1/projects/prove2me/results":
            return self.send(404, {"error": "no"})
        item = parse_qs(u.query).get("item", [""])[0]
        self.send(200, {"results": [r for r in self.store if r["item"] == item], "next": None})

    def do_PATCH(self):
        if self.headers.get("Authorization") != f"Bearer {KEY}":
            return self.send(401, {"error": "a project key is needed"})
        rid = self.path.rsplit("/", 1)[-1]
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.patches.append((rid, body))
        r = next((x for x in self.store if x["id"] == rid), None)
        if r is None:
            return self.send(404, {"error": "no such result"})
        before = copy.deepcopy(r)
        r.update(body)
        self.send(200, {"changed": r != before, "result": r})


class BackfillTest(unittest.TestCase):
    def setUp(self):
        Stub.store, Stub.patches = records(), []
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), Stub)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.srv.server_address[1]}"

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()

    def test_dry_run_writes_nothing(self):
        lines = []
        self.assertEqual(B.run(B.Gateway(self.base, None), apply=False, out=lines.append), 0)
        self.assertEqual(Stub.patches, [])
        self.assertEqual(len([x for x in lines if "would PATCH" in x]), 3)

    def test_apply_records_each_reuse_once(self):
        lines = []
        self.assertEqual(B.run(B.Gateway(self.base, KEY), apply=True, out=lines.append), 0)
        self.assertEqual(sorted(rid for rid, _ in Stub.patches), ["r-0000000000000002", "r-0000000000000005", "r-0000000000000018"])
        n18 = next(r for r in Stub.store if r["item"] == "p2m-faf1e3")
        self.assertEqual(n18["reuse"]["author"], "Eyal1990")
        self.assertEqual(n18["reuse"]["relation"], "import")
        self.assertEqual((n18["labels"]["kind"], n18["labels"]["alias"], n18["labels"]["trial"]), ("reuse", "N18", "t9"))
        self.assertTrue(n18["labels"]["reuses"].startswith("WilliamsonShmoys."))
        # the proof by the model is untouched
        self.assertIsNone(next(r for r in Stub.store if r["item"] == "p2m-a3c1d0")["reuse"])
        # N02's labels were right already: only its reuse was sent
        self.assertEqual(sorted(next(b for rid, b in Stub.patches if rid == "r-0000000000000002")), ["reuse"])
        # a second run changes nothing
        Stub.patches = []
        lines = []
        self.assertEqual(B.run(B.Gateway(self.base, KEY), apply=True, out=lines.append), 0)
        self.assertEqual(Stub.patches, [])
        self.assertTrue(all(x.endswith("already recorded") for x in lines), lines)

    def test_a_wrong_key_or_an_old_gateway_fails(self):
        lines = []
        self.assertEqual(B.run(B.Gateway(self.base, "wrong"), apply=True, out=lines.append), 3)
        for r in Stub.store:
            del r["reuse"]
        lines = []
        self.assertEqual(B.run(B.Gateway(self.base, KEY), apply=True, out=lines.append), 3)
        self.assertTrue(all("no reuse field" in x for x in lines), lines)

    def test_records_file(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "records.json"
            rs = records()
            for r in rs:
                del r["reuse"]  # a copy made before the field
            f.write_text(json.dumps({"source": "Public GET", "campaigns": {"campaigns": []}, "results": {"results": rs}, "tasks": {}}))
            self.assertEqual(B.main(["--records", str(f)]), 0)
            out = json.loads(f.read_text())
            got = {r["item"]: r for r in out["results"]["results"]}
            self.assertEqual(got["p2m-faf1e3"]["reuse"]["name"], B.REUSES[2]["reuse"]["name"])
            self.assertEqual(got["p2m-faf1e3"]["labels"]["kind"], "reuse")
            self.assertIsNone(got["p2m-a3c1d0"]["reuse"])
            self.assertIn("reuse backfilled", out["source"])
            again = f.read_text()
            B.main(["--records", str(f)])
            self.assertEqual(f.read_text(), again)


if __name__ == "__main__":
    unittest.main()
