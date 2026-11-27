#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Backfill the gateway's `reuse` record on prove2me's results recorded before the field existed.

A result's `reuse` (docs/gateway.md, "Campaigns and external results") says an accepted submission
closed its leaf by reusing a theorem another prove2me user had proved, not with a proof by the model.
New results carry it from the writer; the three accepted reuses below were recorded before it, one of
them (N18, p2m-faf1e3) even as `kind: proof`. This sets each one's `reuse`, and the labels the writer
uses for a reuse (kind reuse, reuses, relation), by the result's public id with the project's key.

    python3 scripts/prove2me_reuse_backfill.py                        # dry run: what would change
    python3 scripts/prove2me_reuse_backfill.py --apply --key-file F   # PATCH the results
    python3 scripts/prove2me_reuse_backfill.py --records FILE         # the same change to a records file

--gateway picks the gateway (default https://api.aisathome.org; staging:
https://api.staging.aisathome.org). The key is read from --key-file and sent only in the
Authorization header; nothing here prints it. A run changes only what differs, so a second run
changes nothing. --records rewrites a file of the gateway's public records (the site's committed copy,
site/src/data/science/prove2me-records.json) as the gateway answers after the backfill: every
result gets `reuse` (null when it has none). Standard library only.
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

PROJECT = "prove2me"
SOURCE = "prove2me"
DEFAULT_GATEWAY = "https://api.aisathome.org"

# The accepted submissions that closed their leaf by reuse, as the trials recorded them: the leaf, the
# trial (campaign) of the submission, the trials' alias, and the proved prove2me theorem it reused.
REUSES = [
    {
        "item": "p2m-b1d59e",
        "trial": "t6p",
        "alias": "N02",
        "reuse": {
            "name": "AvramDividend.Classical.avram_chat24_occupation_bind_sections_integrable_public",
            "url": "https://prove2.me/theorems/92887d5d-0a99-4615-aac3-82860fb3c907",
            "author": "WillR",
            "relation": "same",
        },
    },
    {
        "item": "p2m-f181df",
        "trial": "t6",
        "alias": "N05",
        "reuse": {
            "name": "NestedSeatAlloc.IntPolicy.eq27_er1_clbi",
            "url": "https://prove2.me/theorems/21543cdb-2cef-4c0d-8b7e-2981bdb2cf31",
            "author": "mikedeng1",
            "relation": "part",
        },
    },
    {
        "item": "p2m-faf1e3",
        "trial": "t9",
        "alias": "N18",
        "reuse": {
            "name": "WilliamsonShmoys.WilliamsonShmoys_maximal_planar_supergraph_edge_count_upper__"
            "p2m_126bf562_f451_4e0b_b87e_ef5c21746e7b_a4",
            "url": "https://prove2.me/theorems/896d696c-1e18-4de1-b5bd-ab3d52a97871",
            "author": "Eyal1990",
            "relation": "import",
        },
    },
]


def find(results: list[dict], fix: dict) -> dict | None:
    """The accepted result of the fix's leaf in its trial, or None (none, or more than one)."""
    hits = [
        r for r in results
        if r.get("item") == fix["item"] and r.get("source", SOURCE) == SOURCE and r.get("state") == "accepted"
        and (r.get("campaign") or (r.get("labels") or {}).get("trial")) == fix["trial"]
    ]
    return hits[0] if len(hits) == 1 else None


def patch_for(result: dict, fix: dict) -> dict:
    """What the PATCH must change on the result ({} when it already is so)."""
    out: dict = {}
    if result.get("reuse") != fix["reuse"]:
        out["reuse"] = fix["reuse"]
    labels = dict(result.get("labels") or {})
    want = {**labels, "kind": "reuse", "reuses": fix["reuse"]["name"], "relation": fix["reuse"]["relation"]}
    want.setdefault("alias", fix["alias"])
    want.setdefault("trial", fix["trial"])
    if want != labels:
        out["labels"] = want
    return out


def apply_to_records(records: dict) -> list[str]:
    """Apply the backfill to a records file's results in place; every result gets `reuse`. Returns
    what changed, one line each."""
    results = records["results"]["results"]
    done = []
    for fix in REUSES:
        r = find(results, fix)
        if r is None:
            continue
        p = patch_for(r, fix)
        if p:
            r.update(p)
            done.append(f"{fix['item']} ({fix['alias']}, {fix['trial']}): {', '.join(sorted(p))}")
    for r in results:
        r.setdefault("reuse", None)
    return done


class Gateway:
    def __init__(self, base: str, key: str | None):
        self.base = base.rstrip("/")
        self.key = key

    def call(self, method: str, path: str, body: dict | None = None) -> tuple[int, dict]:
        req = urllib.request.Request(self.base + path, method=method,
                                     headers={"Accept": "application/json", "User-Agent": "aisathome-reuse-backfill"})
        if body is not None:
            req.data = json.dumps(body).encode()
            req.add_header("Content-Type", "application/json")
        if self.key and method != "GET":
            req.add_header("Authorization", f"Bearer {self.key}")
        try:
            with urllib.request.urlopen(req, timeout=30) as res:
                return res.status, json.loads(res.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"{}")
            except ValueError:
                return e.code, {}

    def results(self, item: str) -> list[dict]:
        q = urllib.parse.urlencode({"item": item, "source": SOURCE, "limit": 500})
        code, d = self.call("GET", f"/v1/projects/{PROJECT}/results?{q}")
        if code != 200:
            raise SystemExit(f"GET results of {item}: HTTP {code} {d.get('error', '')}")
        return d.get("results") or []


def run(gw: Gateway, apply: bool, out=print) -> int:
    """Backfill each reuse; returns the number of failures."""
    failed = 0
    for fix in REUSES:
        name = f"{fix['item']} ({fix['alias']}, {fix['trial']})"
        r = find(gw.results(fix["item"]), fix)
        if r is None:
            out(f"{name}: no single accepted result in {fix['trial']}; skipped")
            failed += 1
            continue
        if "reuse" not in r:
            out(f"{name}: this gateway's results have no reuse field (an older gateway); skipped")
            failed += 1
            continue
        p = patch_for(r, fix)
        if not p:
            out(f"{name}: already recorded")
            continue
        if not apply:
            out(f"{name}: would PATCH {r['id']} {json.dumps(p, ensure_ascii=False)}")
            continue
        code, d = gw.call("PATCH", f"/v1/projects/{PROJECT}/results/{r['id']}", p)
        if code != 200:
            out(f"{name}: PATCH {r['id']}: HTTP {code} {d.get('error', '')}")
            failed += 1
            continue
        out(f"{name}: {'recorded' if d.get('changed') else 'already recorded'} ({r['id']})")
    return failed


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--gateway", default=DEFAULT_GATEWAY)
    ap.add_argument("--key-file", type=Path, help="the prove2me project key (needed with --apply)")
    ap.add_argument("--apply", action="store_true", help="PATCH the results (default: print what would change)")
    ap.add_argument("--records", type=Path, help="apply to this records file instead of a gateway")
    a = ap.parse_args(argv)
    if a.records:
        records = json.loads(a.records.read_text())
        for line in apply_to_records(records) or ["nothing to change"]:
            print(f"{a.records}: {line}")
        note = "; reuse backfilled by scripts/prove2me_reuse_backfill.py"
        if note not in records.get("source", ""):
            records["source"] = records.get("source", "") + note
        a.records.write_text(json.dumps(records, ensure_ascii=False, indent="\t") + "\n")
        return 0
    key = None
    if a.apply:
        if not a.key_file:
            ap.error("--apply needs --key-file")
        key = a.key_file.expanduser().read_text().strip()
    return 1 if run(Gateway(a.gateway, key), a.apply) else 0


if __name__ == "__main__":
    sys.exit(main())
