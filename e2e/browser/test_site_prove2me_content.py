"""The prove2me science page (/science/prove2me/), the science overview (/science/) and the home page's science
section in a browser: what they say and show, from the build in site/dist (conftest.py: dist_url, site_pages; CI builds
it from the fixed records site/tests/fixtures/prove2me-records-2026-11-18.json) with the gateway failing or answering
fixed documents. Checked here: the committed data's public fields and its own sums (site/src/data/science/prove2me.json);
the table of every task (grouped, labelled, every result in words, every trial the gateway has, the outline probes);
the list of every leaf (filters, sorting) and its scorecard; the headline numbers and the submissions (reuses marked
and credited); the trials and every run (counts, tokens computed); "Working on now" as built; the network's share
(a fixed /v1/stats); "How to read this"; a phone in both themes; the sidebar; the overview's cards and the home page's
line and chart. Consistency is checked against the page itself and the data, not fixed counts, so a new export of the
data changes nothing here. (The live parts against fixed documents: test_site_prove2me.py.)

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_prove2me_content.py

SPDX-License-Identifier: Apache-2.0
"""
import json
import re

import pytest
from conftest import DIST as DIST_DIR, SRC
from playwright.sync_api import expect

JSON = "/science/prove2me.json"
DATA = SRC / "site/src/data/science/prove2me.json"
TAUCETI = SRC / "site/src/data/science/tauceti.json"
# Nothing internal: no reviewer or tooling names, paths, the platform's user or submission ids.
INTERNAL = ("claude", "anthropic", "codex", "gpt-", "/home/", "raw/", "s3://", "user_id", "submission_id", "file_path",
            "created_by", "proved_by")


@pytest.fixture(scope="module")
def data() -> dict:
    return json.loads(DATA.read_text())


def _tauceti() -> dict:
    return json.loads(TAUCETI.read_text())


def _serve(page, url, what):
    if isinstance(what, int):
        page.route(url, lambda r: r.fulfill(status=what, body="no"))
    else:
        body = json.dumps(what)
        page.route(url, lambda r: r.fulfill(status=200, content_type="application/json", body=body))


def _page(site_pages, stats="fail", width=None, live="fail", records="fail"):
    """A page whose gateway answers as given. `stats`: /v1/stats, a dict or "fail" (HTTP 500). `live`: "Working on
    now"'s sources (/data/prove2me/current.json and /v1/tasks), "fail" (HTTP 500 both, so the page keeps the build's
    copy), a (document, tasks) pair (each a dict or an int status), or None (left to the test). `records`: the
    campaigns and results, "fail" (the numbers, submissions and trials stay the build's)."""
    page = site_pages(width=width or 1400, height=844 if width else 900)
    if live == "fail":
        live = (500, 500)
    if live is not None:
        _serve(page, "**/data/prove2me/current.json", live[0])
        _serve(page, "**/v1/tasks?*", live[1])
    if records == "fail":
        _serve(page, "**/v1/projects/prove2me/campaigns?*", 500)
        _serve(page, "**/v1/projects/prove2me/results?*", 500)
    if stats == "fail":
        _serve(page, "**/v1/stats", 500)
    elif stats is not None:
        _serve(page, "**/v1/stats", stats)
    return page


PAGE = "/science/prove2me/"


THEOREM = re.compile(r"^https://prove2\.me/theorems/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


TOP = {"project", "project_url", "model", "generated_at", "as_of", "lean", "mathlib", "totals", "submissions", "current", "trials",
       "leaves", "by_role", "tasks", "probes"}


# Outline probe rounds (2026-11-15): t<N>p chose trial N's leaves, one outline task per leaf; "finished" with its tasks'
# sums (its tasks in "tasks", trial label "<N>p"), "running" with its leaves only.
PROBE_ROUND = {"trial", "for_trial", "state", "start", "end", "scaffold", "caps", "leaves", "tasks", "model_minutes",
               "prompt_tokens", "cached_tokens", "output_tokens"}


PROBE_LEAF = {"id", "alias", "role", "theorem", "title", "url"}


# The task table's result phrases never show a raw outcome word (2026-11-15: check_error showed raw).
RAW_OUTCOMES = ("rest_done", "check_error", "compile_error", "no_solution", "no-progress", "assembly_failed", "no_manifest")


RUN = {"leaf", "group", "role", "theorem", "title", "url", "outcome", "proved_in_round", "rounds", "scored", "not_scored_why",
       "model_minutes", "prompt_tokens", "cached_tokens", "output_tokens", "earlier",
       "proved_in_outline", "tasks", "outlines"}     # the last three: a v5 trial's run (a leaf is many tasks)


SUBMISSION = {"leaf", "trial", "kind", "theorem", "title", "url", "submitted_at", "verdict", "verdict_at", "reuses", "found"}


# A reuse (2026-11-15): "same" statement as the proved theorem, or a "part" of its conclusion (N05); found by the
# trial's outline probes or during the trial.
REUSES = {"theorem", "url", "author", "relation"}


# The table of every leaf: one row per theorem over every run, plus a leaf closed by reuse.
LEAF = {"id", "aliases", "theorem", "title", "url", "roles", "trials", "runs", "proved_runs", "outcome", "first_tried",
        "model_minutes", "prompt_tokens", "cached_tokens", "output_tokens"}


ROLES = ("calibration", "open", "comparison")


# The task table: generic field names any project's tasks could use, plus prove2me's labels.
TASK_ROW = {"project", "id", "kind", "state", "outcome", "try", "parent", "needs", "retry_of", "replaces", "steps", "model_min",
            "tokens", "started_at", "finished_at", "labels"}


# Since 2026-11-15 also the task's role and its kind on the gateway (scaffold v7: p2m-v7), and a v7 task's closed_by and
# hole id when it has them.
TASK_ROW_MORE = {"role", "gateway_kind", "closed_by", "hole_id"}


TASK_ID = re.compile(r"^(t[0-9]+p?)-([A-Za-z0-9]+)(?:-s([0-9]+))?(?:-(?:h([0-9]+)|(rest|asm)))?(?:-a([0-9]+))?$")


# v7's task ids: <trial>-<leaf id>-s<outline>[-h<NN>|-rest|-asm][-a<try>], -s1 always written.
V7_TASK_ID = re.compile(r"^(t[0-9]+p?)-(p2m-[0-9a-f]{6}(?:[0-9a-f]{2})?)-s([0-9]+)(?:-(?:h([0-9]+)|(rest|asm)))?(?:-a([0-9]+))?$")


OUTCOMES = ("accepted", "reused", "proved", "not_proved", "stopped", "not_scored")   # best first


OUTCOME_LABEL = {"accepted": "Accepted by prove2me", "reused": "Closed by reuse", "proved": "Proved", "not_proved": "Not proved",
                 "stopped": "Stopped", "not_scored": "Not scored"}   # a reuse: the proved theorem it imports, its page, its author as that page shows


CURRENT_LEAF = {"leaf", "group", "theorem", "title", "url", "state", "outline", "earlier", "holes", "hole_tasks", "outlines"}


CURRENT = {"trial", "state", "start", "scaffold", "caps", "summary", "holes_as_of", "leaves", "tasks"}


TASK = {"id", "leaf", "role", "gen", "hole", "attempt", "state", "outcome", "steps", "model_minutes", "prompt_tokens",
        "cached_tokens", "output_tokens"}


OUTLINE = {"gen", "holes", "lemmas", "inplace", "prepass", "prepass_holes"}


TOK = ("prompt_tokens", "cached_tokens", "output_tokens")


O04 = "https://prove2.me/theorems/7f145aaf-e97f-4d4f-bd5a-1b6d57c5d1fa"


STATS = {"updated": "2026-11-13T16:10:23Z", "nodes": {"online": 2, "slots": 2, "busy": 2, "paused": 0, "held": 0},
         "today": {"date": "2026-11-13", "requests": 400, "completed": 390, "prompt_tokens": 1, "completion_tokens": 1,
                   "projects": {"prove2me": {"requests": 100, "prompt_tokens": 3_700_000, "completion_tokens": 190_000,
                                             "node_seconds": 7949.2}}},
         "total": {"requests": 12_000, "completed": 11_000, "tokens": 5, "since": "2026-10-06",
                   "projects": {"prove2me": {"requests": 1234, "prompt_tokens": 14_000_000, "completion_tokens": 700_000,
                                             "cached_tokens": 13_000_000, "uncached_prompt_tokens": 1_000_000,
                                             "node_seconds": 36_000}}}}


def _open_all(page, sel="details[data-p2m-trial], [data-p2m-now] details[data-leaf-detail], details[data-p2m-list], details[data-p2m-alltasks]"):
    """Open the page's closed sections (each trial's list, "Working on now"'s rows, every leaf and every task are drawn
    in the browser when they open) and wait until each is drawn."""
    page.locator(sel).evaluate_all("ds => ds.forEach((d) => (d.open = true))")
    waiting = ":is(details[open] > .p2m-lazy, details[open] > .now-more > .p2m-lazy):not([data-lazy-state=done])"
    expect(page.locator(waiting)).to_have_count(0, timeout=20_000)


def _doc(data, trial=None, leaves=None, submissions=(), description="Scaffold v7. The outline is `sorry` holes.\n\nSecond *part*.",
         status="running", start="2026-11-15", scaffold="v7", budget="one round of 110 steps / 110 min", schema=2):
    """A data document (schema prove2me-current/2, or /1, which the page still reads); by default for the build's
    current trial and its leaves."""
    cur = data["current"] or {}
    n = trial or cur.get("trial") or 6
    ids = {l["theorem"]: l["id"] for l in data["leaves"]}
    if leaves is None:
        leaves = [{"id": ids.get(l["theorem"]) or "p2m-" + l["url"][-36:].replace("-", "")[:6], "alias": l["leaf"],
                   "role": l["group"], "theorem": l["theorem"], "url": l["url"]} for l in cur.get("leaves") or []]
    return {"schema": f"prove2me-current/{schema}", "project": "prove2me", "updated_at": "2026-11-15T10:20:00Z",
            "trial": {"id": f"t{n}", "number": n, "scaffold": scaffold, "start": start, "status": status,
                      "description": description, "budget": budget},
            "leaves": leaves, "submissions": list(submissions)}


def _ptask(id_, trial, alias, leaf, role, state, outline="1", hole=None, try_=1, **kw):
    """One task of GET /v1/tasks (tasks_public.go's schema)."""
    t = {"project": "prove2me", "id": id_, "kind": "p2m-v7", "role": role, "state": state, "outcome": None, "try": try_,
         "parent": None, "needs": [], "retry_of": None, "replaces": None, "closed_by": None, "hole_id": None,
         "labels": {"trial": trial, "leaf": leaf, "alias": alias, "group": "open", "scaffold": "v7", "outline": outline,
                    **({"hole": str(hole)} if hole else {})},
         "steps": None, "model_min": None, "tokens": None, "prompt_tokens": None, "cached_tokens": None,
         "completion_tokens": None, "created_at": "2026-11-15T09:07:31Z", "started_at": None, "finished_at": None,
         "updated_at": "2026-11-15T10:00:00Z", "node_classes": []}
    t.update(kw)
    return t


def _tasks(tasks):
    return {"updated": "2026-11-15T10:21:00Z", "projects": ["prove2me"], "tasks": tasks, "next": None}


def _counts(rows):
    def of(g=None):
        rs = [r for r in rows if g is None or r["group"] == g]
        return sum(r["outcome"] == "proved" for r in rs), len(rs)
    return of(), of("calibration"), of("open")


def test_the_data_is_public_and_consistent(data):
    """The committed data has the public fields only, links only to prove2me's theorem pages, and no internal word;
    its totals are its rows'. (That /science/prove2me.json is it: site/scripts/check-dist.mjs.)"""
    assert set(data) <= TOP, set(data) - TOP
    for tr in data["trials"]:
        assert tr["leaves"], f"trial {tr['trial']} has no leaves"
        for r in tr["leaves"]:
            assert set(r) <= RUN, set(r) - RUN
            assert THEOREM.match(r["url"]), r["url"]
            assert r["scored"] or r.get("not_scored_why"), f"trial {tr['trial']} {r['leaf']}: unscored without a reason"
            for tk in r.get("tasks") or []:     # a finished v5 trial: every task ended, its tokens known unless never run
                assert set(tk) == TASK and tk["leaf"] == r["leaf"], tk
                assert tk["state"] in ("finished", "failed", "cancelled"), tk
                assert tk["prompt_tokens"] is not None or (tk["state"] == "cancelled" and not tk["steps"]), tk
            if r.get("tasks"):
                for k in TOK:
                    assert r[k] == sum(tk[k] or 0 for tk in r["tasks"]), f"trial {tr['trial']} {r['leaf']}: {k} is not its tasks' sum"
                assert r["model_minutes"] == round(sum(tk["model_minutes"] or 0 for tk in r["tasks"]), 1), r["leaf"]
            for o in r.get("outlines") or []:
                assert set(o) == OUTLINE, o
    runs = [r for tr in data["trials"] for r in tr["leaves"]]
    for s in data["submissions"]:
        assert set(s) <= SUBMISSION and THEOREM.match(s["url"]), s
        assert s["kind"] in ("proof", "reuse"), s
        if s["kind"] == "reuse":
            # closed by reuse (N02, 2026-11-14): no run proved it, it names the theorem it imports and its author
            assert set(s["reuses"]) == REUSES and THEOREM.match(s["reuses"]["url"]) and s["reuses"]["author"], s
            assert s["reuses"]["relation"] in ("same", "part") and s["found"] in ("probes", "trial"), s
            assert s["reuses"]["url"] != s["url"] and s["reuses"]["theorem"] != s["theorem"], s
            assert not any(r["theorem"] == s["theorem"] and r["outcome"] == "proved" for r in runs), s
            continue
        assert "reuses" not in s, s
        cur = data["current"] or {}
        if s["trial"] == cur.get("trial"):
            # a proof of the running trial (2026-11-15: N03 in trial 6): its leaf's assembly or outline task proved it
            assert any(l["leaf"] == s["leaf"] and l["theorem"] == s["theorem"] for l in cur["leaves"]), s
            assert any(tk["leaf"] == s["leaf"] and tk["outcome"] == "proved" and tk["role"] in ("asm", "skeleton")
                       for tk in cur.get("tasks") or []), s
            continue
        # the leaf and trial of the run that proved it (v5's assembly record names itself "assembly")
        assert any(r["leaf"] == s["leaf"] and r["theorem"] == s["theorem"] and r["outcome"] == "proved"
                   for tr in data["trials"] if tr["trial"] == s["trial"] for r in tr["leaves"]), s
    for l in (data["current"] or {}).get("leaves", []):
        assert set(l) <= CURRENT_LEAF and THEOREM.match(l["url"]), l
        for o in l.get("outlines") or []:
            assert set(o) == OUTLINE, o
    if data["current"]:
        assert set(data["current"]) <= CURRENT, set(data["current"]) - CURRENT
        for tk in data["current"].get("tasks") or []:
            assert set(tk) == TASK, tk
            assert TASK_ID.match(tk["id"]) or V7_TASK_ID.match(tk["id"]), tk["id"]
            if tk["prompt_tokens"] is not None:
                assert 0 <= tk["cached_tokens"] <= tk["prompt_tokens"] and tk["output_tokens"] >= 0, tk
            if tk["state"] == "finished" and tk["role"] != "asm":
                assert tk["prompt_tokens"], f"{tk['id']}: finished with no tokens from its records"
    # since 12 November 2026 every proof of an open leaf is submitted (the page says so): each proved open run of
    # trial 3 on is listed, under its own leaf (F01's assembly record sits one folder deeper than the others)
    subs = {(s["trial"], s["leaf"]) for s in data["submissions"]}
    for tr in data["trials"]:
        for r in tr["leaves"]:
            if tr["trial"] >= 3 and r["group"] == "open" and r["outcome"] == "proved":
                assert (tr["trial"], r["leaf"]) in subs, f"trial {tr['trial']} {r['leaf']}: proved, not in the submissions"
    # the outline probe rounds: public fields, their leaves' ids as the leaf table's, a finished round its tasks' sums
    probe_rows = [t for t in data["tasks"] if t["labels"]["trial"].endswith("p")]
    for pr in data.get("probes") or []:
        assert set(pr) == PROBE_ROUND and re.fullmatch(r"t[0-9]+p", pr["trial"]) and pr["for_trial"] == int(pr["trial"][1:-1]), pr
        assert pr["state"] in ("finished", "running") and pr["leaves"], pr
        for l in pr["leaves"]:
            assert set(l) == PROBE_LEAF and THEOREM.match(l["url"]) and l["id"] == "p2m-" + l["url"][-36:].replace("-", "")[:len(l["id"]) - 4], l
        mine = [t for t in probe_rows if t["labels"]["trial"] == pr["trial"][1:]]
        if pr["state"] == "running":
            assert not mine and pr["tasks"] is None and pr["prompt_tokens"] is None, pr["trial"]
            continue
        assert pr["tasks"] == len(mine) > 0, pr["trial"]
        assert pr["prompt_tokens"] + pr["output_tokens"] == sum(t["tokens"] or 0 for t in mine), pr["trial"]
        assert pr["model_minutes"] == round(sum(t["model_min"] or 0 for t in mine), 1), pr["trial"]
        assert {t["labels"]["leaf"] for t in mine} <= {l["id"] for l in pr["leaves"]}, pr["trial"]
        assert all(t["kind"] == "outline" and t["state"] in ("done", "failed", "cancelled") for t in mine), pr["trial"]
    done = [pr for pr in data.get("probes") or [] if pr["state"] == "finished"]
    if done:
        pt = data["totals"]["probes"]
        assert pt["rounds"] == len(done) and pt["tasks"] == sum(pr["tasks"] for pr in done)
        for k in TOK:
            assert pt[k] == sum(pr[k] for pr in done), k
    text = json.dumps(data).lower()
    for bad in INTERNAL:
        assert bad not in text, f"the data mentions {bad!r}"
    # the totals are the rows' own; a leaf closed by reuse counts as accepted, not as an open leaf proved
    t = data["totals"]
    assert t["runs"] == len(runs) and t["proved"] == sum(r["outcome"] == "proved" for r in runs)
    assert t["open_leaves"] == len({r["theorem"] for r in runs if r["group"] == "open"})
    assert t["open_leaves_proved"] == len({r["theorem"] for r in runs if r["group"] == "open" and r["outcome"] == "proved"})
    assert t["accepted"] == sum(s["verdict"] == "accepted" for s in data["submissions"])
    assert t["reused"] == sum(s["kind"] == "reuse" and s["verdict"] == "accepted" for s in data["submissions"])
    for k in TOK:
        assert t[k] == sum(r[k] for r in runs), f"totals.{k} is not the runs' sum"
    for r in runs:
        assert r["prompt_tokens"] > 0 and r["output_tokens"] > 0 and 0 <= r["cached_tokens"] <= r["prompt_tokens"], r["leaf"]


def _leaves(data):
    """The leaf table recomputed from the runs and submissions: theorem -> the expected row's fields."""
    out = {}
    for tr in data["trials"]:
        for r in tr["leaves"]:
            x = out.setdefault(r["theorem"], {"aliases": [], "roles": set(), "trials": [], "runs": [], "first_tried": tr["start"]})
            x["aliases"] += [r["leaf"]] if r["leaf"] not in x["aliases"] else []
            x["roles"].add(r["role"])
            x["trials"] += [tr["trial"]] if tr["trial"] not in x["trials"] else []
            x["runs"].append(r)
    for s in data["submissions"]:
        if s["kind"] == "reuse":
            out.setdefault(s["theorem"], {"aliases": [s["leaf"]], "roles": {"open"}, "trials": [], "runs": [],
                                          "first_tried": s["submitted_at"][:10]})
    acc = {s["theorem"]: s["kind"] for s in data["submissions"] if s["verdict"] == "accepted"}
    for th, x in out.items():
        runs = x["runs"]
        x["outcome"] = ("accepted" if acc.get(th) == "proof" else "reused" if acc.get(th) == "reuse" else
                        "proved" if any(r["outcome"] == "proved" for r in runs) else
                        "not_proved" if any(r["scored"] for r in runs) else
                        "stopped" if any(r["outcome"] == "cancelled" for r in runs) else "not_scored")
        x["tokens"] = sum(r["prompt_tokens"] + r["output_tokens"] for r in runs) if runs else None
        x["minutes"] = round(sum(r["model_minutes"] for r in runs), 1) if runs else None
    return out


def test_the_leaf_table_data_is_the_runs_own(data):
    """Every leaf ever tried is one row: its id is p2m- and the first 6 hex characters of prove2me's theorem id (8 when two
    collide), the trials' ids its aliases; its roles, trials, best outcome, minutes and tokens are its runs'; the
    scorecard by role sums the runs of each role. Recomputed here from the trials and submissions."""
    want = _leaves(data)
    rows = data["leaves"]
    assert {r["theorem"] for r in rows} == set(want) and len(rows) == len(want) == data["totals"]["leaves"]
    hexes = [r["url"].rsplit("/", 1)[1].replace("-", "") for r in rows]
    for r, h in zip(rows, hexes):
        assert set(r) == LEAF and THEOREM.match(r["url"]), r
        n = 6 if sum(x[:6] == h[:6] for x in hexes) == 1 else 8
        assert r["id"] == f"p2m-{h[:n]}", r["id"]
        w = want[r["theorem"]]
        assert r["aliases"] == w["aliases"] and r["trials"] == w["trials"], r
        assert r["roles"] == [x for x in ROLES if x in w["roles"]], r
        assert r["outcome"] == w["outcome"] and r["first_tried"] == w["first_tried"], r
        assert r["runs"] == len(w["runs"]) and r["proved_runs"] == sum(x["outcome"] == "proved" for x in w["runs"]), r
        assert r["model_minutes"] == w["minutes"], r
        assert (None if r["prompt_tokens"] is None else r["prompt_tokens"] + r["output_tokens"]) == w["tokens"], r
    assert len({r["id"] for r in rows}) == len(rows), "two leaves share an id"
    runs = [r for tr in data["trials"] for r in tr["leaves"]]
    proving = {(tr["trial"], r["theorem"]): r["role"] for tr in data["trials"] for r in tr["leaves"] if r["outcome"] == "proved"}
    for sc in data["by_role"]:
        mine = [r for r in runs if r["role"] == sc["role"]]
        assert sc["runs"] == len(mine) and sc["proved"] == sum(r["outcome"] == "proved" for r in mine), sc
        assert sc["leaves"] == sum(sc["role"] in r["roles"] for r in rows), sc
        assert sc["accepted"] == sum(s["kind"] == "proof" and s["verdict"] == "accepted" and proving.get((s["trial"], s["theorem"])) == sc["role"]
                                     for s in data["submissions"]), sc
        for k in TOK:
            assert sc[k] == sum(r[k] for r in mine), (sc["role"], k)
    t = data["totals"]
    assert sum(sc["runs"] for sc in data["by_role"]) == t["runs"] and sum(sc["reused"] for sc in data["by_role"]) == t["reused"]
    # a proof of the running trial has no run of a finished trial, so no role yet
    cur_proofs = sum(s["kind"] == "proof" and s["verdict"] == "accepted" and s["trial"] == (data["current"] or {}).get("trial")
                     for s in data["submissions"])
    assert sum(sc["accepted"] for sc in data["by_role"]) == t["accepted"] - t["reused"] - cur_proofs


def _relations(rows):
    """id -> parent, needs, retry_of, replaces from the task ids alone (outline, hole, rest and assembly tasks)."""
    ids = {r["id"] for r in rows}
    out = {}
    for r in rows:
        if r["kind"] == "run":
            out[r["id"]] = (None, [], None, None)
            continue
        lab = r["labels"]
        mine = [x for x in rows if x["kind"] != "run" and x["labels"]["trial"] == lab["trial"] and x["labels"]["alias"] == lab["alias"]]
        def newest(pred):
            c = [x for x in mine if pred(x)]
            return max(c, key=lambda x: x["try"])["id"] if c else None
        outline = lambda g: newest(lambda x: x["kind"] == "outline" and x["labels"]["outline"] == g)
        g = lab["outline"]
        parent = outline(g) if r["kind"] in ("hole", "rest", "assembly") else None
        base = re.sub(r"-a[0-9]+$", "", r["id"])
        prev = base if r["try"] == 2 else f"{base}-a{r['try'] - 1}"
        retry = prev if r["try"] > 1 and prev in ids else None
        replaces = outline(str(int(g) - 1)) if r["kind"] == "outline" and int(g) > 1 and r["try"] == 1 else None
        needs = []
        if r["kind"] == "assembly":
            holes = sorted({int(x["labels"]["hole"]) for x in mine if x["kind"] == "hole" and x["labels"]["outline"] == g})
            needs = [newest(lambda x, h=h: x["kind"] == "hole" and x["labels"]["outline"] == g and x["labels"].get("hole") == str(h)) for h in holes]
            rest = newest(lambda x: x["kind"] == "rest" and x["labels"]["outline"] == g)
            needs += [rest] if rest else []
        out[r["id"]] = (parent, needs, retry, replaces)
    return out


def test_the_task_table_data_is_the_records_own(data):
    """Every task, in the generic fields: one task per run of trials 1 to 4, a v5 trial's own tasks; its tokens, minutes
    and steps are the trials' records; its leaf the leaf table's id; its relations follow from the task ids alone."""
    tasks = data["tasks"]
    assert len({t["id"] for t in tasks}) == len(tasks), "two tasks share an id"
    want = []
    for tr in data["trials"]:
        for r in tr["leaves"]:
            want += [tk["id"] for tk in r["tasks"]] if r.get("tasks") else [(tr["trial"], r["leaf"])]
    probes = [t for t in tasks if t["labels"]["trial"].endswith("p")]
    assert len(tasks) == len(want) + len((data["current"] or {}).get("tasks") or []) + len(probes)
    assert len(probes) == sum(pr["tasks"] or 0 for pr in data.get("probes") or [])
    leaf_id = {l["theorem"]: l["id"] for l in data["leaves"]}
    run_of = {(tr["trial"], r["leaf"]): r for tr in data["trials"] for r in tr["leaves"]}
    static = {tk["id"]: tk for tr in data["trials"] for r in tr["leaves"] for tk in r.get("tasks") or []}
    rel = _relations(tasks)
    for t in tasks:
        assert TASK_ROW <= set(t) <= TASK_ROW | TASK_ROW_MORE and t["project"] == "prove2me", t
        assert t["state"] in ("queued", "running", "done", "failed", "cancelled"), t
        assert t["kind"] in ("outline", "hole", "rest", "assembly", "run"), t
        lab = t["labels"]
        run = None if lab["trial"].endswith("p") else run_of.get((int(lab["trial"]), lab["alias"]))
        if run:
            assert lab["leaf"] == leaf_id[run["theorem"]], t
        if t["kind"] == "run":
            assert t["id"] in (f"t{lab['trial']}-{lab['alias']}",) and run and not run.get("tasks"), t
            assert t["tokens"] == run["prompt_tokens"] + run["output_tokens"] and t["model_min"] == run["model_minutes"], t
        else:
            m = TASK_ID.match(t["id"])
            v7 = V7_TASK_ID.match(t["id"])
            if v7:      # v7 names the leaf by its id; the alias is a label
                m = v7
                # the gateway kind is the scaffold's: p2m-v7 (trial 6), p2m-v8 (trial 7), later ones alike
                assert m[2] == lab["leaf"] and re.fullmatch(r"p2m-v[7-9]|p2m-v[1-9][0-9]+", t["gateway_kind"] or ""), t
            else:
                assert m and m[2] == lab["alias"], t
            assert m[1] == f"t{lab['trial']}" and lab["outline"] == (m[3] or "1"), t
            assert t["try"] == int(m[6] or 1) and lab.get("hole") == (str(int(m[4])) if m[4] else None), t
            tk = static.get(t["id"])
            if tk:
                assert t["model_min"] == tk["model_minutes"] and t["steps"] == tk["steps"], t
                assert t["tokens"] == (None if tk["prompt_tokens"] is None else tk["prompt_tokens"] + (tk["output_tokens"] or 0)), t
        assert (t["parent"], t["needs"], t["retry_of"], t["replaces"]) == rel[t["id"]], t["id"]
        for f in ("started_at", "finished_at"):
            assert t[f] is None or re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", t[f]), t
        if t["started_at"] and t["finished_at"]:
            assert t["started_at"] <= t["finished_at"], t
    # the relations, present in trial 5: a retry, a replaced outline, an assembly's holes
    if any(t["labels"]["trial"] == "5" for t in tasks):
        assert any(t["retry_of"] for t in tasks) and any(t["replaces"] for t in tasks) and any(t["needs"] for t in tasks)


# A trial of the fixtures' own (96: the build never has it, so nothing of the build's trial merges in) and three leaves.
FIX_LEAVES = [{"id": f"p2m-e2e00{i}", "alias": a, "role": r, "theorem": f"E2eFixture.leaf_{a.lower()}",
               "url": f"https://prove2.me/theorems/e2e00{i}00-0000-4000-8000-00000000000{i}"}
              for i, (a, r) in enumerate((("X01", "calibration"), ("X02", "open"), ("X03", "open")), 1)]


def _fix_live():
    """GET /v1/tasks for trial 96: X01's outline done, its hole 1 running with the gateway's live counters, its assembly
    queued; X02's outline running without them; X03 no task; and tasks the page must leave out: an outline probe (t96p)
    and a task of trial 5."""
    x1, x2 = FIX_LEAVES[0]["id"], FIX_LEAVES[1]["id"]
    s1 = f"t96-{x1}-s1"
    return [
        _ptask(s1, "t96", "X01", x1, "outline", "done", outcome="outline", steps=40, model_min=50.0, prompt_tokens=2_000_000,
               cached_tokens=8_000_000, completion_tokens=100_000, tokens=2_100_000, started_at="2026-11-15T09:08:00Z",
               finished_at="2026-11-15T10:00:00Z"),
        _ptask(f"{s1}-h01", "t96", "X01", x1, "hole", "leased", hole=1, parent=s1, started_at="2026-11-15T10:05:00Z",
               live={"requests": 12, "prompt_tokens": 1_500_000, "cached_tokens": 1_200_000, "completion_tokens": 20_000,
                     "model_s": 600, "updated_at": "2026-11-15T10:20:30Z"}),   # live prompt_tokens include the cache reads
        _ptask(f"{s1}-asm", "t96", "X01", x1, "assembly", "queued", parent=s1, needs=[f"{s1}-h01"]),
        _ptask(f"t96-{x2}-s1", "t96", "X02", x2, "outline", "leased", started_at="2026-11-15T10:01:00Z"),
        _ptask("t96p-N09", "t96p", "N09", "N09", "skeleton", "leased", kind="other"),
        _ptask("t5-F01-s2", "t5", "F01", "F01", "skeleton", "done", kind="other", outcome="outline"),
    ]


def _fix_doc(data, **kw):
    return _doc(data, trial=96, leaves=[dict(l) for l in FIX_LEAVES], **kw)


def _jsround(x: float) -> int:
    return int(x + 0.5)


def _jscompact(v: int) -> str:
    """task-table.ts compactNum (JavaScript's rounding): 2_043_914 -> "2.0M", 498_304 -> "498k"."""
    return f"{v / 1e6:.1f}M" if v >= 1e6 else f"{_jsround(v / 1e3)}k" if v >= 1e3 else str(_jsround(v))


def _computed(tok: dict) -> int:
    """Tokens computed: what the model read without the cache, plus what it wrote."""
    return tok["prompt_tokens"] - (tok["cached_tokens"] or 0) + tok["output_tokens"]


def _task_computed(data) -> dict:
    """task id -> tokens computed, as the page counts the data's task rows: from each run's or task's own counts (a
    probe task's are not in the data)."""
    out = {}
    for tr in data["trials"]:
        for r in tr["leaves"]:
            if r.get("tasks"):
                out.update({t["id"]: _computed(t) for t in r["tasks"] if t["prompt_tokens"] is not None and t["output_tokens"] is not None})
            else:
                out[f"t{tr['trial']}-{r['leaf']}"] = _computed(r)
    return out


def _work(steps, minutes, tokens) -> str:
    """The Work cell: "47 steps · 44 min · 2.4M", "–" when there was none."""
    parts = [f"{steps} step{'' if steps == 1 else 's'}" if steps else "",
             (f"{_jsround(minutes)} min" if minutes >= 10 else f"{minutes:.1f} min") if minutes else "",
             _jscompact(tokens) if tokens else ""]
    return " · ".join(p for p in parts if p) or "–"


_MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]


def _short(iso: str) -> str:
    """"2026-11-13T16:07:00Z" -> "13 Nov 16:07"."""
    return f"{int(iso[8:10])} {_MON[int(iso[5:7]) - 1]} {iso[11:16]}"


def _label(t: dict) -> str:
    """The Task cell: a readable label, " · try N" after a first try."""
    lab, o = t["labels"], t["labels"].get("outline", "1")
    base = {"run": "Run", "outline": "Probe: outline" if lab["trial"].endswith("p") else f"Outline {o}",
            "hole": f"Hole {lab.get('hole')} (outline {o})", "rest": f"In place (outline {o})",
            "assembly": f"Assembly (outline {o})"}[t["kind"]]
    return f"{base} · try {t['try']}" if t["try"] > 1 else base


def _show(t: dict, rows: list[dict]) -> str:
    """The Show switch's set: running (running or queued), proved (a proof, a closed hole, the holes in place filled in
    when the outline's assembly proved), else not proved."""
    if t["state"] in ("running", "queued"):
        return "running"
    if t["state"] != "done":
        return "not_proved"
    if t["outcome"] in ("proved", "closed"):
        return "proved"
    if t["outcome"] == "rest_done":
        lab = t["labels"]
        return "proved" if any(x["kind"] == "assembly" and x["outcome"] == "proved" and x["labels"]["trial"] == lab["trial"]
                               and x["labels"]["leaf"] == lab["leaf"] and x["labels"].get("outline") == lab.get("outline")
                               for x in rows) else "not_proved"
    return "not_proved"


def _as_row(t: dict) -> dict:
    """A task of GET /v1/tasks as the table's row (prove2me-current.ts taskTableRows): the gateway's live counters for a
    running task (their prompt tokens include the cache reads)."""
    lv = t.get("live") or {}
    if lv:     # tokens computed: the live prompt tokens less their cache reads, plus written
        tok = lv["prompt_tokens"] - lv.get("cached_tokens", 0) + lv["completion_tokens"]
    else:
        tok = None if t["cached_tokens"] is None else (t["prompt_tokens"] or 0) + (t["completion_tokens"] or 0)
    lab = t["labels"]
    return {"id": t["id"], "kind": {"skeleton": "outline", "asm": "assembly"}.get(t["role"], t["role"]),
            "state": "running" if t["state"] == "leased" else t["state"], "outcome": t["outcome"] if t["state"] == "done" else None,
            "try": t["try"] or 1, "parent": t["parent"], "needs": t["needs"], "retry_of": t["retry_of"], "replaces": t["replaces"],
            "labels": {"trial": lab["trial"][1:], "leaf": lab["leaf"] if lab["leaf"].startswith("p2m-") else "",
                       **{k: lab[k] for k in ("outline", "hole") if k in lab}},
            "tokens": tok, "steps": lv.get("requests", t["steps"]) if lv else t["steps"],
            "model_min": _jsround(lv["model_s"] / 6) / 10 if lv else t["model_min"], "started_at": t["started_at"],
            "finished_at": t["finished_at"]}


def _rel_attrs(row, t):
    """A row's relations as its data attributes say (the grouping shows the parent; a retry or a replacement is a note)."""
    for attr, v in (("data-parent", t["parent"]), ("data-retry-of", t["retry_of"]), ("data-replaces", t["replaces"]),
                    ("data-needs", " ".join(t["needs"]) or None)):
        if v:
            expect(row).to_have_attribute(attr, v)
        else:
            assert row.get_attribute(attr) is None, (t["id"], attr)


def test_the_task_table_is_grouped_labelled_and_live(site_pages, dist_url, data):
    """The table of every task: five columns (Leaf, Task, Result, Work, Finished); rows
    grouped by trial and leaf, the leaf shown once per group; a task as a label, its raw id on hover; every task of the
    data and, live from GET /v1/tasks, every task of trial 6 on and of the outline probe rounds (none of the frozen
    trials 1 to 5), the gateway's relations kept; a running task's work from the gateway's live counters; filters by trial
    (probe rounds as "Trial N probes"; by default the newest trial), by leaf, and the Show switch (all, proved, not
    proved, running) with "Showing N of M"."""
    # the frozen trials' tasks (trials 1 to 5): the build's rows of later trials come from the records it fetched
    tasks = [t for t in data["tasks"] if int(re.match(r"\d+", t["labels"]["trial"]).group()) <= 5]
    live_tasks = _fix_live()
    page = _page(site_pages, "fail", live=(_fix_doc(data), _tasks(live_tasks)))
    page.goto(dist_url + PAGE)
    box = page.locator("details[data-p2m-alltasks]")
    expect(box).not_to_have_attribute("open", "")
    box.locator("summary").click()
    mine = [t for t in live_tasks if t["labels"]["trial"] in ("t96", "t96p")]
    expect(box.locator("tr[data-source=live]")).to_have_count(len(mine), timeout=15_000)
    expect(box.locator("tr[data-task-id='t5-F01-s2'][data-source=live]")).to_have_count(0)
    assert [h.inner_text().strip().lower() for h in box.locator("thead th").all()] == ["leaf", "task", "result", "work", "finished"]
    n = box.locator("tbody tr").count()       # the build's rows (the frozen trials' and the records') and the live ones
    assert n >= len(tasks) + len(mine), n
    count = box.locator("[data-tt-count]")
    trial = box.locator("select[data-tt-filter='label:trial']")
    # by default the newest trial (96: the live one, newer than the build's)
    expect(trial).to_have_value("96")
    t96 = [t for t in mine if t["labels"]["trial"] == "t96"]
    expect(count).to_have_text(f"Showing {len(t96)} of {n}")
    s1, x1 = t96[0]["id"], t96[0]["labels"]["leaf"]
    vis = box.locator("tbody tr:not([hidden])")
    assert [r.get_attribute("data-task-id") for r in vis.all()] == [s1, f"{s1}-h01", f"{s1}-asm", t96[3]["id"]]
    # the leaf once per group: on its first row, hidden on the others
    assert [r.get_attribute("data-first") is not None for r in vis.all()] == [True, False, False, True]
    out = box.locator(f"tr[data-task-id='{s1}']")
    expect(out.locator("[data-leaf]")).to_have_text(x1)
    expect(out.locator("[data-leaf]")).to_have_attribute("title", "X01 in trial 96")
    expect(out.locator("[data-round]")).to_have_text("Trial 96")
    expect(box.locator(f"tr[data-task-id='{s1}-h01'] [data-leaf]")).to_be_hidden()
    expect(out.locator(".tt-task")).to_have_text("Outline 1")
    expect(out.locator(".tt-task")).to_have_attribute("title", s1)
    expect(out.locator("[data-cell=result]")).to_have_text("Outline written")
    expect(out.locator("[data-cell=work]")).to_have_text("40 steps · 50 min · 2.1M")       # tokens computed: 2.0M read fresh + 0.1M written
    expect(out.locator("[data-cell=finished] time")).to_have_text("15 Nov 10:00")
    expect(out.locator("[data-cell=finished] time")).to_have_attribute("title", "started 15 Nov 09:08 UTC")
    hole = box.locator(f"tr[data-task-id='{s1}-h01']")
    expect(hole).to_have_attribute("data-state", "running")
    expect(hole.locator(".tt-task")).to_have_text("Hole 1 (outline 1)")
    expect(hole.locator("[data-cell=result]")).to_have_text("Running (12 steps so far)")
    expect(hole.locator("[data-cell=work]")).to_have_text("12 steps · 10 min · 320k")
    expect(hole).to_have_attribute("data-tokens", str(300_000 + 20_000))
    expect(hole.locator("[data-cell=finished]")).to_have_text("running since 15 Nov 10:05")
    expect(hole).to_have_attribute("data-parent", s1)
    asm = box.locator(f"tr[data-task-id='{s1}-asm']")
    expect(asm.locator(".tt-task")).to_have_text("Assembly (outline 1)")
    expect(asm.locator("[data-cell=result]")).to_have_text("Queued")
    expect(asm).to_have_attribute("data-needs", f"{s1}-h01")
    expect(asm.locator("[data-cell=work]")).to_have_text("–")
    expect(box.locator(f"tr[data-task-id='{t96[3]['id']}'] [data-cell=result]")).to_have_text("Running")
    # the probe round's live row
    labels = [o.inner_text() for o in trial.locator("option").all()]
    assert "Trial 96 probes" in labels and labels[0] == "All trials", labels
    trial.select_option("96p")
    expect(count).to_have_text(f"Showing 1 of {n}")
    probe = box.locator("tr[data-task-id='t96p-N09']")
    expect(probe.locator(".tt-task")).to_have_text("Probe: outline")
    expect(probe.locator("[data-leaf]")).to_have_text("N09")
    expect(probe.locator("[data-round]")).to_have_text("Trial 96 probes")
    # every task of the data, its label, its relations, its result never a raw outcome word
    computed = _task_computed(data)
    trial.select_option("")
    expect(count).to_have_text(f"Showing {n} of {n}")
    for t in tasks:
        row = box.locator(f"tr[data-task-id='{t['id']}']")
        expect(row.locator(".tt-task")).to_have_text(_label(t))
        _rel_attrs(row, t)
        if t["retry_of"]:
            expect(row.locator("[data-rel=retry_of] a")).to_have_attribute("href", f"#p2m-task-{t['retry_of']}")
        if t["replaces"]:
            prev = next(x for x in tasks if x["id"] == t["replaces"])["labels"]["outline"]
            expect(row.locator("[data-rel=replaces]")).to_have_text(f"replaces outline {prev}")
        expect(row.locator("[data-cell=work]")).to_have_text(_work(t["steps"], t["model_min"], computed.get(t["id"])))
        if t["finished_at"]:
            expect(row.locator("[data-cell=finished] time")).to_have_text(_short(t["finished_at"]))
    words = [c.inner_text() for c in box.locator("tbody [data-cell=result]").all()]
    for bad in RAW_OUTCOMES:
        assert not any(bad in w for w in words), f"a result says {bad!r}"
    # the outline probes of the data: their rounds in the trial filter, their rows with the leaf's theorem
    for pr in [pr for pr in data.get("probes") or [] if pr["state"] == "finished"]:
        trial.select_option(pr["trial"][1:])
        rows = [t for t in data["tasks"] if t["labels"]["trial"] == pr["trial"][1:]]
        expect(count).to_have_text(re.compile(rf"^Showing \d+ of {n}$"))
        for t in rows:
            row = box.locator(f"tr[data-task-id='{t['id']}']")
            expect(row.locator(".tt-task")).to_have_text("Probe: outline")
            leaf = next(l for l in pr["leaves"] if l["id"] == t["labels"]["leaf"])
            expect(row.locator("a.tt-thm")).to_have_attribute("href", leaf["url"])
            expect(row.locator("a.tt-thm")).to_have_text(leaf["theorem"])
            expect(row.locator("[data-round]")).to_have_text(f"Trial {pr['for_trial']} probes")
    trial.select_option("")
    # the leaf filter and the Show switch
    rows = [t for t in tasks] + [_as_row(t) for t in mine]
    leaf = box.locator("select[data-tt-filter='label:leaf']")
    leaf.select_option(x1)
    expect(count).to_have_text(f"Showing 3 of {n}")
    leaf.select_option("")
    for r in rows:      # the rows known here are in the set the switch shows them in
        expect(box.locator(f"tr[data-task-id='{r['id']}']")).to_have_attribute("data-show", _show(r, rows))
    for v in ("proved", "not_proved", "running", ""):
        btn = box.locator(f"[data-tt-show='{v}']")
        btn.click()
        expect(btn).to_have_attribute("aria-pressed", "true")
        k = box.locator(f"tbody tr[data-show='{v}']").count() if v else n
        expect(count).to_have_text(f"Showing {k} of {n}")
    expect(box.locator("[data-tt-show][aria-pressed=true]")).to_have_count(1)


# A trial that ended and that the build does not have (95: the build never has it) while the document's trial (96) runs:
# the gateway is the record from trial 6 on (2026-11-17: trial 7 vanished from the page when trial 8
# started), so its tasks and its row in the trials table come live, from every page of GET /v1/tasks.
T95 = [("p2m-e2e951", "Y01", "calibration"), ("p2m-e2e952", "Y02", "open")]


def _ended_trial():
    """GET /v1/tasks of trial 95: Y01's outline, its hole proved and its assembly proving the leaf; Y02's outline that did
    not compile and its second outline cancelled."""
    (y1, a1, g1), (y2, a2, g2) = T95
    s1 = f"t95-{y1}-s1"
    lab = lambda alias, group, **kw: {"alias": alias, "group": group, "scaffold": "v8", "outline": "1", **kw}
    fin = {"started_at": "2026-11-14T09:00:00Z", "finished_at": "2026-11-14T10:00:00Z"}

    def mk(id_, leaf, role, state, labels, **kw):
        return _ptask(id_, "t95", "", leaf, role, state, created_at="2026-11-14T08:00:00Z",
                      labels={"trial": "t95", "leaf": leaf, **labels}, **kw)
    return [
        mk(s1, y1, "outline", "done", outcome="outline", steps=20, model_min=12.0, prompt_tokens=100_000, cached_tokens=900_000,
           completion_tokens=10_000, **fin, labels=lab(a1, g1)),
        mk(f"{s1}-h01", y1, "hole", "done", outcome="proved", parent=s1, steps=5, model_min=3.0, prompt_tokens=20_000,
           cached_tokens=80_000, completion_tokens=2_000, **fin, labels=lab(a1, g1, hole="1")),
        mk(f"{s1}-asm", y1, "assembly", "done", outcome="proved", parent=s1, needs=[f"{s1}-h01"], steps=0, model_min=0,
           prompt_tokens=0, cached_tokens=0, completion_tokens=0, **fin, labels=lab(a1, g1)),
        mk(f"t95-{y2}-s1", y2, "outline", "done", outcome="compile_error", steps=30, model_min=45.0, prompt_tokens=300_000,
           cached_tokens=1_700_000, completion_tokens=30_000, started_at="2026-11-14T09:00:00Z",
           finished_at="2026-11-15T01:00:00Z", labels=lab(a2, g2)),
        mk(f"t95-{y2}-s2", y2, "outline", "cancelled", replaces=f"t95-{y2}-s1", labels=lab(a2, g2, outline="2")),
    ]


def _serve_pages(page, doc, tasks, per_page=3):
    """The document (a dict or an int status) and GET /v1/tasks in pages of `per_page`, newest first, each page's `next`
    the gateway's cursor (sent back as `after`), so the page must follow it to see the oldest tasks."""
    _serve(page, "**/data/prove2me/current.json", doc)
    seen = []

    def handle(route):
        q = route.request.url.split("?", 1)[1]
        after = re.search(r"(?:^|&)after=([^&]+)", q)
        i = int(after.group(1).split("~", 1)[0]) if after else 0
        seen.append(i)
        chunk = tasks[i * per_page:(i + 1) * per_page]
        more = (i + 1) * per_page < len(tasks)
        body = {"updated": "2026-11-15T10:21:00Z", "projects": ["prove2me"], "tasks": chunk,
                "next": f"{i + 1}~prove2me~{chunk[-1]['id']}" if more else None}
        route.fulfill(status=200, content_type="application/json", body=json.dumps(body))
    page.route("**/v1/tasks?*", handle)
    return seen


def test_every_trial_the_gateway_has_is_on_the_page(site_pages, dist_url, data):
    """Every trial from trial 6 on comes live from the gateway, not only the document's (2026-11-17: trial 7's 40 tasks
    vanished when trial 8 started): an ended trial the build lacks (95) shows its tasks in the task table, under its own
    trial in the filter, its leaves by id (no theorem known). The task list is followed page by page (`next`). Tasks of
    the frozen trials 1 to 5 and labels that are no trial stay out. Without the document, the table still goes live;
    from a used browser that first saw the gateway fail, the live rows come in once, not twice. (Its row in the trials
    table comes from the gateway's campaigns since step D6: the code repo's e2e/browser/test_site_prove2me.py.)"""
    t95 = _ended_trial()
    live = _fix_live()[:4]                           # trial 96 runs (the document's)
    extra = [_ptask("t5-F01-s2", "t5", "F01", "F01", "skeleton", "done", kind="other", outcome="outline"),
             _ptask("e2e1-C07", "e2e1", "C07", "C07", "outline", "done", outcome="proved")]
    tasks = live + extra + t95                       # newest first: trial 95's tasks on the last pages
    # a used browser: the page first loads with the gateway failing (the build's rows only) ...
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    box = page.locator("details[data-p2m-alltasks]")
    page.wait_for_timeout(1500)
    expect(box.locator("tr[data-source=live]")).to_have_count(0)
    # the trials table is the build's (from the records the build fetched)
    expect(page.locator("[data-p2m-trials]")).to_have_attribute("data-source", "build")
    # ... then the gateway answers, in pages of three
    page.unroute("**/data/prove2me/current.json")
    page.unroute("**/v1/tasks?*")
    seen = _serve_pages(page, _fix_doc(data), tasks)
    page.reload()
    box.locator("summary").click()
    expect(box.locator("tr[data-source=live]")).to_have_count(len(live) + len(t95), timeout=15_000)
    assert sorted(set(seen)) == [0, 1, 2, 3], f"pages read: {seen}"
    for t in t95 + live:
        expect(box.locator(f"tr[data-task-id='{t['id']}'][data-source=live]")).to_have_count(1)
    for t in extra:
        expect(box.locator(f"tr[data-task-id='{t['id']}'][data-source=live]")).to_have_count(0)
    expect(box.locator("tr[data-task-id='e2e1-C07']")).to_have_count(0)
    trial = box.locator("select[data-tt-filter='label:trial']")
    expect(trial).to_have_value("96")
    assert "Trial 95" in [o.inner_text() for o in trial.locator("option").all()]
    trial.select_option("95")
    vis = box.locator("tbody tr:not([hidden])")
    expect(vis).to_have_count(len(t95))
    assert [r.get_attribute("data-task-id") for r in vis.all()] == [t["id"] for t in t95]
    (y1, a1, _), (y2, a2, _) = T95
    first = box.locator(f"tr[data-task-id='t95-{y1}-s1']")
    expect(first.locator("[data-leaf]")).to_have_text(y1)
    expect(first.locator("[data-leaf]")).to_have_attribute("title", f"{a1} in trial 95")
    expect(first.locator("[data-round]")).to_have_text("Trial 95")
    expect(first.locator(".tt-thm")).to_have_count(0)      # a leaf the build does not know: its id only
    expect(first.locator("[data-cell=result]")).to_have_text("Outline written")
    expect(first.locator("[data-cell=work]")).to_have_text("20 steps · 12 min · 110k")
    expect(box.locator(f"tr[data-task-id='t95-{y1}-s1-asm'] [data-cell=result]")).to_have_text("Proved: our full check passed")
    expect(box.locator(f"tr[data-task-id='t95-{y2}-s2'] [data-cell=result]")).to_have_text("Cancelled")
    # the document's leaves name their theorems in the task table too
    trial.select_option("96")
    x1 = FIX_LEAVES[0]
    expect(box.locator(f"tr[data-task-id='t96-{x1['id']}-s1'] a.tt-thm")).to_have_attribute("href", x1["url"])
    # without campaigns the trials table stays the build's
    expect(page.locator("[data-p2m-trials]")).to_have_attribute("data-source", "build")
    # reloaded in the same browser: the live rows come once
    page.reload()
    box.locator("summary").click()
    expect(box.locator("tr[data-source=live]")).to_have_count(len(live) + len(t95), timeout=15_000)
    # without the document: the task table still goes live; "Working on now" keeps the build's copy
    p2 = _page(site_pages, "fail", live=None)
    _serve_pages(p2, 500, tasks)
    p2.goto(dist_url + PAGE)
    box2 = p2.locator("details[data-p2m-alltasks]")
    box2.locator("summary").click()     # drawn when it opens
    expect(box2.locator("tr[data-source=live]")).to_have_count(len(live) + len(t95), timeout=15_000)
    expect(p2.locator("[data-p2m-now]")).to_have_attribute("data-source", "build")


def _words_tasks():
    """Trial 96's X01 with every kind of result: outline 1 written (3 holes, hole 1 found false by the document), a hole
    our check failed on, one proved, one stopped by a fault and its second try cancelled, the holes in place cancelled;
    outline 2 replacing it (2 holes), the holes in place filled in, the assembly proved. X02's outline did not compile."""
    x1, x2 = FIX_LEAVES[0]["id"], FIX_LEAVES[1]["id"]
    s1, s2 = f"t96-{x1}-s1", f"t96-{x1}-s2"
    end = {"started_at": "2026-11-15T09:10:00Z", "finished_at": "2026-11-15T09:40:00Z", "steps": 10, "model_min": 5.0,
           "prompt_tokens": 100_000, "cached_tokens": 400_000, "completion_tokens": 10_000}
    done = lambda id_, role, outcome, **kw: _ptask(id_, "t96", "X01", x1, role, "done", outcome=outcome, **{**end, **kw})
    return [
        done(s1, "outline", "outline"),
        done(f"{s1}-h01", "hole", "check_error", hole=1, parent=s1),
        done(f"{s1}-h02", "hole", "proved", hole=2, parent=s1),
        done(f"{s1}-h03", "hole", "infra", hole=3, parent=s1),
        _ptask(f"{s1}-h03-a2", "t96", "X01", x1, "hole", "cancelled", hole=3, try_=2, parent=s1, retry_of=f"{s1}-h03"),
        _ptask(f"{s1}-rest", "t96", "X01", x1, "rest", "cancelled", parent=s1),
        done(s2, "outline", "outline", outline="2", replaces=s1),
        done(f"{s2}-rest", "rest", "rest_done", outline="2", parent=s2),
        done(f"{s2}-asm", "assembly", "proved", outline="2", parent=s2, needs=[f"{s2}-rest"], steps=None, model_min=None,
             prompt_tokens=0, cached_tokens=0, completion_tokens=0),
        _ptask(f"t96-{x2}-s1", "t96", "X02", x2, "outline", "done", outcome="compile_error", **end),
    ]


def test_the_task_table_says_every_result_in_words(site_pages, dist_url, data):
    """Bug, 2026-11-15: the table showed raw outcome words ("check_error", "rest_done"). Every result is one plain
    phrase: "Our check failed", "Filled in, checked by the assembly" (proved: its assembly proved), "Written, 3 holes"
    (the document's hole count), "Stopped by a fault", "Did not compile", "Cancelled (outline refuted)" (the document
    found a hole of it false), "Proved", "Proved: our full check passed"; a retry and a replacement say so."""
    x1 = FIX_LEAVES[0]["id"]
    doc = _fix_doc(data)
    doc["leaves"][0]["outlines"] = [
        {"outline": 1, "holes": [{"hole": 1, "kind": "lemma", "state": "false", "by": "model"},
                                 {"hole": 2, "kind": "lemma", "state": "open"}, {"hole": 3, "kind": "inplace", "state": "open"}]},
        {"outline": 2, "holes": [{"hole": 1, "kind": "inplace", "state": "open"}, {"hole": 2, "kind": "inplace", "state": "open"}]}]
    page = _page(site_pages, "fail", live=(doc, _tasks(_words_tasks())))
    page.goto(dist_url + PAGE)
    box = page.locator("details[data-p2m-alltasks]")
    box.locator("summary").click()
    expect(box.locator("tr[data-source=live]")).to_have_count(10, timeout=15_000)
    s1, s2 = f"t96-{x1}-s1", f"t96-{x1}-s2"
    want = {
        s1: ("Outline 1", "Written, 3 holes", "not_proved"),
        f"{s1}-h01": ("Hole 1 (outline 1)", "Our check failed", "not_proved"),
        f"{s1}-h02": ("Hole 2 (outline 1)", "Proved", "proved"),
        f"{s1}-h03": ("Hole 3 (outline 1)", "Stopped by a fault", "not_proved"),
        f"{s1}-h03-a2": ("Hole 3 (outline 1) · try 2", "Cancelled (outline refuted)", "not_proved"),
        f"{s1}-rest": ("In place (outline 1)", "Cancelled (outline refuted)", "not_proved"),
        s2: ("Outline 2", "Written, 2 holes", "not_proved"),
        f"{s2}-rest": ("In place (outline 2)", "Filled in, checked by the assembly", "proved"),
        f"{s2}-asm": ("Assembly (outline 2)", "Proved: our full check passed", "proved"),
        f"t96-{FIX_LEAVES[1]['id']}-s1": ("Outline 1", "Did not compile", "not_proved"),
    }
    for id_, (label, result, show) in want.items():
        row = box.locator(f"tr[data-task-id='{id_}']")
        expect(row.locator(".tt-task")).to_have_text(label)
        expect(row.locator("[data-cell=result]")).to_have_text(result)
        expect(row).to_have_attribute("data-show", show)
    expect(box.locator(f"tr[data-task-id='{s1}-h03-a2'] [data-rel=retry_of]")).to_have_text("retry of try 1")
    expect(box.locator(f"tr[data-task-id='{s2}'] [data-rel=replaces]")).to_have_text("replaces outline 1")
    text = box.locator("tbody").inner_text()
    for bad in RAW_OUTCOMES:
        assert bad not in text, f"the table says {bad!r}"
    # the same outline without the document's verdict: cancelled because outline 2 replaced it
    page2 = _page(site_pages, "fail", live=(_fix_doc(data), _tasks(_words_tasks())))
    page2.goto(dist_url + PAGE)
    box2 = page2.locator("details[data-p2m-alltasks]")
    box2.locator("summary").click()
    row = box2.locator(f"tr[data-task-id='{s1}-rest'][data-source=live]")
    expect(row.locator("[data-cell=result]")).to_have_text("Cancelled (outline replaced)", timeout=15_000)
    expect(box2.locator(f"tr[data-task-id='{s1}'] [data-cell=result]")).to_have_text("Outline written")


def _probe_tasks():
    """Round t97p (trial 97's leaves) after trial 96 ended: one probe running with the gateway's live counters, one
    queued, one that wrote its outline, one that did not compile."""
    ids = [f"p2m-e2e1{i:02d}" for i in range(1, 5)]
    mk = lambda i, state, **kw: _ptask(f"t97p-{ids[i]}-s1", "t97p", f"Q0{i + 1}", ids[i], "outline", state,
                                       created_at=f"2026-11-15T19:08:1{i}Z", **kw)
    return ids, [
        mk(0, "leased", started_at="2026-11-15T19:12:00Z",
           live={"requests": 33, "prompt_tokens": 1_234_358, "cached_tokens": 1_167_692, "completion_tokens": 81_442,
                 "model_s": 1776, "updated_at": "2026-11-15T19:57:16Z"}),
        mk(1, "queued"),
        mk(2, "done", outcome="outline", steps=41, model_min=38.6, prompt_tokens=62_521, cached_tokens=1_934_589,
           completion_tokens=46_804, started_at="2026-11-15T19:13:00Z", finished_at="2026-11-15T19:52:00Z"),
        mk(3, "done", outcome="compile_error", steps=16, model_min=45.0, prompt_tokens=67_690, cached_tokens=450_315,
           completion_tokens=71_906, started_at="2026-11-15T19:13:00Z", finished_at="2026-11-15T19:59:00Z"),
    ]


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_working_on_now_shows_the_next_trials_outline_probes(site_pages, dist_url, data, theme):
    """When the document's trial has ended and the next trial's outline probes (round t<N+1>p) are queued or running,
    "Working on now" says "Choosing trial N+1's leaves: K outline probes" and lists each probe: its leaf, its state and
    steps or its outline's result; the same probes are rows of the task table under "Trial N+1 probes". It fits a
    390 px phone in both themes. While the document's trial runs, or with no probe open, the probes are not shown."""
    ids, probes = _probe_tasks()
    x1 = FIX_LEAVES[0]["id"]
    ended = [_ptask(f"t96-{x1}-s1", "t96", "X01", x1, "outline", "done", outcome="proved", steps=12, model_min=8.0)]
    page = _page(site_pages, "fail", width=390, live=(_fix_doc(data, status="finished"), _tasks(ended + probes)))
    page.clock.install()     # time runs as usual; the end moves it on to the page's minute refresh
    page.goto(dist_url + PAGE)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    now = page.locator("[data-p2m-now][data-source=live]")
    box = now.locator("[data-now-probes]")
    expect(box).to_be_visible(timeout=15_000)
    expect(now).to_have_attribute("data-probes", "t97p")
    expect(box.locator("[data-probes-head]")).to_have_text("Choosing trial 97’s leaves: 4 outline probes")
    items = box.locator("li[data-probe]")
    expect(items).to_have_count(4)
    assert [i.get_attribute("data-probe") for i in items.all()] == [t["id"] for t in probes]
    want = ["Running (33 steps so far)", "Queued", "Outline written", "Did not compile"]
    for i, (t, w) in enumerate(zip(probes, want)):
        li = items.nth(i)
        expect(li.locator(".leaf")).to_have_text(ids[i])
        expect(li.locator(".leaf")).to_have_attribute("title", f"Q0{i + 1}")
        expect(li.locator("[data-probe-result]")).to_have_text(w)
    expect(items.nth(2).locator(".meta")).to_have_text("41 steps · 39 min · 109k")     # tokens computed: 62,521 read fresh + 46,804 written
    expect(now.locator("[data-now-head]")).to_have_text("Trial 96 has ended · scaffold v7")
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    for li in items.all():
        b = li.bounding_box()
        assert b and b["x"] >= 0 and b["x"] + b["width"] <= 390, b
    # the probes in the task table, under their round
    tbl = page.locator("details[data-p2m-alltasks]")
    tbl.locator("summary").click()
    tbl.locator("select[data-tt-filter='label:trial']").select_option("97p")
    expect(tbl.locator("[data-tt-count]")).to_contain_text("Showing 4 of")
    expect(tbl.locator(f"tr[data-task-id='{probes[0]['id']}'] [data-cell=result]")).to_have_text("Running (33 steps so far)")
    for r in tbl.locator("tbody tr:not([hidden])").all():
        b = r.bounding_box()
        assert b and b["x"] >= 0 and b["x"] + b["width"] <= 390, b
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the open task table scrolls sideways"
    # While trial 96 runs, the probes are not "Working on now"; nor when none is open. Checked on the same page through
    # its minute's refresh (the clock moved on), not on fresh pages: a fresh page loads its live data once and retries
    # only a minute later, so each extra load was one more chance to miss the 15 s (2026-11-19). The head
    # changes with each document, so it shows the refresh happened before the probes are checked.
    for doc, ts, head in ((_fix_doc(data, status="running"), ended + probes, "Trial 96, since 15 November 2026 · scaffold v7"),
                          (_fix_doc(data, status="finished"), ended + [dict(t, state="done", live=None) for t in probes],
                           "Trial 96 has ended · scaffold v7")):
        page.unroute("**/data/prove2me/current.json")
        page.unroute("**/v1/tasks?*")
        _serve(page, "**/data/prove2me/current.json", doc)
        _serve(page, "**/v1/tasks?*", _tasks(ts))
        page.clock.run_for(60_000)
        expect(now.locator("[data-now-head]")).to_have_text(head, timeout=15_000)
        expect(now.locator("[data-now-probes]")).to_be_hidden()
        expect(now).to_have_attribute("data-probes", "")


def _leaf_rows(page):
    return page.locator("[data-p2m-leaf-table] tbody tr")


def _visible_ids(page):
    return [r.get_attribute("data-leaf-id") for r in _leaf_rows(page).all() if r.is_visible()]


def _cell_int(loc) -> int:
    return int(loc.inner_text().strip())


def _leaf_table(page) -> list[dict]:
    """The list of every leaf as the page has it: each row's id, roles, outcome, trials and sort values, in its order."""
    return page.evaluate("""() => [...document.querySelectorAll('[data-p2m-leaf-table] tbody tr')].map((r) => ({
        id: r.dataset.leafId, roles: (r.dataset.roles || '').split(' ').filter(Boolean), outcome: r.dataset.outcome,
        order: Number(r.dataset.order), trials: r.querySelector('[data-cell=trials]').textContent.trim(),
        theorem: r.querySelector('[data-sort-theorem]')?.textContent ?? '',
        first: r.dataset.sortFirst === undefined ? null : Number(r.dataset.sortFirst),
        tokens: r.dataset.sortTokens === undefined ? null : Number(r.dataset.sortTokens),
        minutes: r.dataset.sortMinutes === undefined ? null : Number(r.dataset.sortMinutes),
        sortOutcome: Number(r.dataset.sortOutcome) }))""")


def test_the_leaf_table_filters_and_sorts(site_pages, dist_url, data):
    """"Every leaf" (built from the records the site's build fetched, trials 1 to 5 from the frozen data): the scorecard
    adds up (each role's runs, runs proved, proofs accepted and reuses sum to the All row; its leaves are the list's
    rows); every leaf of the frozen data is in the list with its p2m- id, its old id under it and its theorem's link, a
    leaf run only in trials 1 to 5 with the frozen outcome; the role and outcome filters show the matching rows
    ("Showing N of M"); a column's sort puts ▲/▼ and aria-sort on that column only, nulls last."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    score = page.locator("[data-p2m-scorecard]")
    foot = score.locator("tfoot tr[data-role=all]")
    roles = [r.get_attribute("data-role") for r in score.locator("tbody tr[data-role]").all() if r.get_attribute("data-role") != "probes"]
    assert roles and set(roles) <= set(ROLES), roles
    for cell in ("runs", "proved", "accepted", "reused"):
        total = sum(_cell_int(score.locator(f"tbody tr[data-role={r}] [data-cell={cell}]")) for r in roles)
        expect(foot.locator(f"[data-cell={cell}]")).to_have_text(str(total))
    n = _cell_int(foot.locator("[data-cell=leaves]"))

    lst = page.locator("details[data-p2m-list]")
    expect(lst).not_to_have_attribute("open", "")
    expect(lst.locator("summary")).to_have_text(f"All {n} leaves")
    lst.locator("summary").click()
    count = lst.locator("[data-p2m-list-count]")
    expect(_leaf_rows(page)).to_have_count(n)
    expect(count).to_have_text(f"Showing {n} of {n}")
    rows = _leaf_table(page)
    assert [r["order"] for r in rows] == list(range(n)), "the build's order is the rows' data-order"
    for role in roles:
        expect(score.locator(f"tbody tr[data-role={role}] [data-cell=leaves]")).to_have_text(str(sum(role in r["roles"] for r in rows)))
    by_id = {r["id"]: r for r in rows}
    for l in data["leaves"]:
        assert l["id"] in by_id, f"{l['id']} of the frozen data is not in the list"
        row = lst.locator(f"tr[data-leaf-id='{l['id']}']")
        expect(row.locator("th")).to_contain_text(l["id"])
        expect(row.locator("[data-alias]")).to_contain_text(l["aliases"][0])
        expect(row.locator("a[data-sort-theorem]")).to_have_attribute("href", l["url"])
        shown = [int(x) for x in by_id[l["id"]]["trials"].split(", ") if x.isdigit()]
        if l["trials"] and shown and max(shown) <= 5:      # run in the frozen trials only: the frozen data's row
            assert shown == l["trials"], (l["id"], shown)
            expect(row.locator("[data-cell=outcome]")).to_contain_text(OUTCOME_LABEL[l["outcome"]])
    f01 = by_id.get("p2m-a146ce")
    assert f01 and f01["outcome"] == "accepted", f01

    def by(**f):
        return [r["id"] for r in rows if all((v in r["roles"]) if k == "role" else r[k] == v for k, v in f.items())]
    for role in ROLES:
        if not by(role=role):
            continue
        lst.locator(f"[data-filter=role][data-value={role}]").click()
        expect(lst.locator(f"[data-filter=role][data-value={role}]")).to_have_attribute("aria-pressed", "true")
        expect(count).to_have_text(f"Showing {len(by(role=role))} of {n}")
        assert _visible_ids(page) == by(role=role), role
    lst.locator("[data-filter=role][data-value='']").click()
    for o in OUTCOMES:
        btn = lst.locator(f"[data-filter=outcome][data-value={o}]")
        if not by(outcome=o):
            expect(btn).to_have_count(0)
            continue
        btn.click()
        expect(count).to_have_text(f"Showing {len(by(outcome=o))} of {n}")
        assert _visible_ids(page) == by(outcome=o), o
    lst.locator("[data-filter=role][data-value=open]").click()
    lst.locator("[data-filter=outcome][data-value=not_proved]").click()
    expect(count).to_have_text(f"Showing {len(by(role='open', outcome='not_proved'))} of {n}")
    lst.locator("[data-filter=role][data-value='']").click()
    lst.locator("[data-filter=outcome][data-value='']").click()
    expect(count).to_have_text(f"Showing {n} of {n}")

    def expected(key, desc):
        """The order the page should show, from the rows' own values: numbers here, text with the browser's own
        localeCompare; ties and nulls in the build's order, nulls last."""
        k = {"leaf": "id", "theorem": "theorem", "first": "first", "outcome": "sortOutcome", "tokens": "tokens", "minutes": "minutes"}[key]
        vals = [[r["id"], r[k], r["order"]] for r in rows]
        have, none = [v for v in vals if v[1] is not None], [v[0] for v in vals if v[1] is None]
        if have and isinstance(have[0][1], str):
            return page.evaluate("([rs, d]) => rs.slice().sort((a, b) => (d ? -1 : 1) * a[1].localeCompare(b[1]) || a[2] - b[2])"
                                 ".map((r) => r[0])", [have, desc]) + none
        have.sort(key=lambda v: ((-v[1] if desc else v[1]), v[2]))
        return [v[0] for v in have] + none

    heads = lst.locator("th[data-sort-col]")
    for key, first in (("tokens", "desc"), ("minutes", "desc"), ("outcome", "asc"), ("first", "asc"), ("leaf", "asc"), ("theorem", "asc")):
        btn = lst.locator(f"button[data-leaf-sort={key}]")
        for d in (first, "asc" if first == "desc" else "desc"):
            btn.click()
            th = lst.locator(f"th[data-sort-col={key}]")
            expect(th).to_have_attribute("aria-sort", "ascending" if d == "asc" else "descending")
            expect(th.locator(".sort-mark")).to_have_text("▲" if d == "asc" else "▼")
            expect(lst.locator("th[data-sort-col][aria-sort]")).to_have_count(1)
            assert _visible_ids(page) == expected(key, d == "desc"), (key, d)
    assert heads.count() == 6


def _leaf_ids(data) -> dict:
    """theorem -> leaf id: the leaf table's, else an outline probe round's, else the running trial's from its page."""
    ids = {l["theorem"]: "p2m-" + l["url"][-36:].replace("-", "")[:6] for l in (data["current"] or {}).get("leaves") or []}
    ids.update({l["theorem"]: l["id"] for pr in data.get("probes") or [] for l in pr["leaves"]})
    ids.update({l["theorem"]: l["id"] for l in data["leaves"]})
    return ids


def _trial_of(text: str) -> int:
    m = re.search(r"[Tt]rial (\d+)", text)
    assert m, text
    return int(m.group(1))


def test_headline_numbers_and_the_accepted_proof(site_pages, dist_url, data):
    """The numbers on top agree with the rest of the page (built from the records the site's build fetched): the proofs
    accepted are the submissions table's accepted rows that are not reuses (one
    count of proofs, a reuse is never one) and submitted its rows, closed by reuse its accepted reuses, the runs proved
    and run the scorecard's, "trials 1 to N" the trials table's newest; "Small numbers" says the open leaves with an
    accepted proof. The table lists the newest
    trial first in a scroll area; every submission of the frozen data is in it (O04 linked to its theorem page)."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    expect(page.locator("h1")).to_have_text("Science: prove2me")
    rows = page.locator("[data-p2m-submissions] tbody tr")
    n = rows.count()
    assert n >= len(data["submissions"]), n
    accepted = page.locator("[data-p2m-submissions] tbody tr[data-verdict=accepted]").count()
    reused = page.locator("[data-p2m-submissions] tbody tr[data-verdict=accepted][data-kind=reuse]").count()
    expect(page.locator("[data-kpi=accepted] dd")).to_have_text(str(accepted - reused))
    expect(page.locator("[data-kpi=accepted] dt")).to_contain_text(f"of {n} submitted")
    if reused:
        expect(page.locator("[data-kpi=accepted] dt")).to_contain_text(f"({reused} of them reuse")
        expect(page.locator("[data-kpi=reused] dd")).to_have_text(str(reused))
        expect(page.locator("[data-kpi=reused] dt")).to_contain_text("closed by reuse")
    else:
        expect(page.locator("[data-kpi=reused]")).to_have_count(0)
    foot = page.locator("[data-p2m-scorecard] tfoot tr[data-role=all]")
    expect(page.locator("[data-kpi=runs] dd")).to_have_text(foot.locator("[data-cell=proved]").inner_text())
    trials = [int(r.get_attribute("data-trial")) for r in page.locator(f"{TRIAL_ROWS}:not([data-probes])").all()]
    expect(page.locator("[data-kpi=runs] dt")).to_contain_text(f"of {foot.locator('[data-cell=runs]').inner_text()} in trials 1 to {max(trials)}")
    open_ = page.locator("[data-kpi=open] dd").inner_text()
    expect(page.locator("[data-p2m-small]")).to_contain_text(f"Small numbers: {open_} open leaves with an accepted proof")
    # the open leaves with an accepted proof are never more than the proofs
    assert int(open_) <= accepted - reused, (open_, accepted, reused)
    # newest trial first, in a scroll area the keyboard can reach
    order = [_trial_of(r.locator("td[data-label=From]").inner_text()) for r in rows.all()]
    assert order == sorted(order, reverse=True), order
    expect(page.locator("[data-p2m-subs-scroll]")).to_have_attribute("tabindex", "0")
    o04 = page.locator("tr[data-submission=O04]")
    expect(o04.locator("a[data-theorem]")).to_have_attribute("href", O04)
    expect(o04.locator("a[data-theorem]")).to_have_text("SeqPostedPricing.MatroidSPM.accept_prob_eq_quantile")
    expect(o04.locator(".verdict")).to_contain_text("Accepted")
    expect(o04).to_contain_text("12 November 2026, 06:49 UTC")
    ids = _leaf_ids(data)
    for s in data["submissions"]:
        row = page.locator(f"tr[data-submission='{s['leaf']}']")
        # the leaf id leads, the trials' alias small under it (as in the leaf and task tables)
        expect(row.locator("th")).to_have_text(f"{ids[s['theorem']]}{s['leaf']}")
        expect(row.locator("th [data-alias]")).to_have_text(s["leaf"])
        expect(row.locator("a[data-theorem]")).to_have_attribute("href", s["url"])
        expect(row).to_have_attribute("data-verdict", s["verdict"])
        expect(row).to_have_attribute("data-kind", s["kind"])
        if s["kind"] == "reuse":
            continue
        expect(row.locator("td[data-label=From]")).to_have_text(f"Trial {s['trial']}")
        expect(row.locator("[data-reuses]")).to_have_count(0)
    expect(page.locator("main a[data-p2m-link=home]")).to_have_attribute("href", "https://prove2.me/")


def test_the_chart_of_accepted_proofs_under_the_numbers(site_pages, dist_url):
    """The home page's chart of accepted proofs is on the page too (code prove2me-chart), right under the headline
    numbers and before the submissions: as built (the gateway's records refused), one point and one table row per
    accepted proof, the total at the end the headline number; inside the screen at 390 px."""
    k = _built_kpis(site_pages, dist_url)
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    fig = page.locator("figure[data-p2m-chart]")
    expect(fig).to_have_count(1)
    expect(fig).to_have_attribute("data-source", "build")
    expect(fig.locator("svg[role=img] circle")).to_have_count(int(k["accepted"]))
    expect(fig.locator("svg text.end-n")).to_have_text(k["accepted"])
    assert fig.locator("svg[role=img]").get_attribute("aria-label").startswith(f"{k['accepted']} proof")
    expect(fig.locator("tbody tr")).to_have_count(int(k["accepted"]))
    order = page.evaluate("""() => [...document.querySelectorAll('[data-p2m-kpis], figure[data-p2m-chart], [data-p2m-submissions]')]
        .map((e) => e.matches('[data-p2m-kpis]') ? 'kpis' : e.matches('figure') ? 'chart' : 'submissions')""")
    assert order == ["kpis", "chart", "submissions"], order
    page.set_viewport_size({"width": 390, "height": 844})
    page.wait_for_timeout(300)
    b = fig.locator("svg").bounding_box()
    assert b["x"] >= 0 and b["x"] + b["width"] <= 390, f"the chart on a phone: {b}"
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"


def test_a_leaf_closed_by_reuse_is_marked_and_credited(site_pages, dist_url, data):
    """A submission closed by reuse says so in its row (how it was found), links the theorem it uses and names its
    author; one line under the table says these are not counted as proved by the model. The frozen data's (N02, the same
    statement as a theorem another prove2me user had proved, found while trial 6 was prepared; N05, a part of a proved
    theorem's conclusion, found during trial 6) are as recorded, closed by reuse wherever a trial's list names them, and
    in the list of every leaf."""
    reuses = [s for s in data["submissions"] if s["kind"] == "reuse"]
    if not reuses:
        pytest.skip("no submission closed by reuse in the data")
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    _open_all(page, "details[data-p2m-trial], details[data-p2m-list]")
    caption = page.locator("h2#submissions ~ p.caps").first
    expect(caption).to_contain_text("Closed by reuse: the proof imports a theorem someone else had already proved; not counted as the model’s proof.")
    # the proved theorems page, from the section's lead line
    expect(page.locator("a[data-p2m-theorems-link=submissions]")).to_have_attribute("href", "/science/prove2me/theorems/")
    for row in page.locator("[data-p2m-submissions] tbody tr[data-kind=reuse]").all():
        expect(row.locator("td[data-label=From]")).to_contain_text("Closed by reuse")
        expect(row.locator("[data-reuses] a")).to_have_attribute("href", THEOREM)
    for s in reuses:
        row = page.locator(f"tr[data-submission='{s['leaf']}']")
        found = f"found during trial {s['trial']}" if s["found"] == "trial" else f"found while preparing trial {s['trial']}"
        expect(row.locator("td[data-label=From]")).to_contain_text(found)
        expect(row.locator("td[data-label=From]")).not_to_contain_text(f"Trial {s['trial']}")
        link = row.locator("[data-reuses] a")
        expect(link).to_have_attribute("href", s["reuses"]["url"])
        expect(link).to_have_text(s["reuses"]["theorem"])
        expect(row.locator("[data-reuses]")).to_contain_text(f"by {s['reuses']['author']}")
        expect(row.locator("[data-reuses]")).to_contain_text("part of" if s["reuses"]["relation"] == "part" else "same statement as")
        lid = next(l["id"] for l in data["leaves"] if l["theorem"] == s["theorem"])
        expect(page.locator(f"[data-p2m-leaf-table] tr[data-leaf-id='{lid}']")).to_have_attribute("data-outcome", "reused")
        # no run of any trial: a trial's list that names the leaf (the records' trial 6 does) says closed by reuse
        assert not any(r["leaf"] == s["leaf"] for tr in data["trials"] for r in tr["leaves"]), s["leaf"]
        for tr in page.locator(f"details[data-p2m-trial] tr[data-leaf='{s['leaf']}']").all():
            expect(tr).to_have_attribute("data-outcome", "reused")


# The trials: one row per trial, newest first, each a <details> whose summary
# is the row (dates, scaffold, leaves, accepted proofs, GPU-h per proof) and whose list has the counts the row leaves out
# ([data-trial-numbers]: our own check's by role, model hours, tokens); probe rounds are plain rows; the All line closes it.
TRIAL_ROWS = "[data-p2m-trials] [data-p2m-trial-lists] > [data-trial]"


TRIALS_ALL = "[data-p2m-trials] [data-p2m-trials-all]"


def _trial(page, n):
    return page.locator(f"{TRIAL_ROWS}[data-trial='{n}']")


def _open_trial(page, n):
    """Open trial n's row (a click on its summary) and return its details."""
    d = page.locator(f"details[data-p2m-trial='{n}']")
    if d.get_attribute("open") is None:
        d.locator(":scope > summary").click()
    expect(d).to_have_attribute("open", "")
    return d


def _of(text: str) -> tuple[int, int]:
    m = re.fullmatch(r"(\d+) of (\d+)", text.strip())
    return (int(m.group(1)), int(m.group(2))) if m else (0, 0)


def test_every_trial_and_every_run(site_pages, dist_url, data):
    """The trials: one row per trial, newest first. Trials 1 to 5 with their counts recomputed from the frozen runs
    (our own check's: proved, calibration and open, under the row), each trial's list (opened from its row) with every
    run, its theorem link and result, unscored runs saying so and why; every later trial (from the records) with a row
    and its own list; the All line's counts the sum of the trials'. A probe round's row says it is one."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    rows = page.locator(f"{TRIAL_ROWS}:not([data-probes])")
    numbers = [int(r.get_attribute("data-trial")) for r in rows.all()]
    assert numbers == list(range(max(numbers), 0, -1)), numbers
    for pr in page.locator(f"{TRIAL_ROWS}[data-probes]").all():
        expect(pr.locator(".tr-name")).to_have_text(re.compile(r"^Trial \d+ probes$"))
        expect(pr).to_contain_text("Outline probes to choose its leaves")
    for tr in [t for t in data["trials"] if t["trial"] <= 5]:
        (p, n), (cp, cn), (op, on) = _counts(tr["leaves"])
        row = _trial(page, tr["trial"])
        expect(row).to_have_attribute("data-source", "build")
        expect(row.locator(":scope > summary [data-cell=leaves]")).to_have_text(f"{n} leaves")
        nums = row.locator("[data-trial-numbers]")
        expect(nums.locator("[data-cell=proved]")).to_have_text(f"{p} of {n}")
        if cn:
            expect(nums.locator("[data-cell=calibration]")).to_have_text(f"{cp} of {cn}")
        expect(nums.locator("[data-cell=open]")).to_have_text(f"{op} of {on}")
        expect(row).not_to_have_attribute("open", "")
        details = _open_trial(page, tr["trial"])
        runs = details.locator("table.p2m-leaves > tbody > tr")
        expect(runs).to_have_count(len(tr["leaves"]))
        for r in tr["leaves"]:
            cell = details.locator(f"tr[data-leaf='{r['leaf']}']")
            expect(cell.get_by_role("link", name=r["theorem"], exact=True)).to_have_attribute("href", r["url"])
            expect(cell).to_have_attribute("data-outcome", r["outcome"])
            result = cell.locator(".result")
            if not r["scored"]:
                expect(result).to_contain_text("Not scored")
                expect(result).to_contain_text(r["not_scored_why"])
            elif r["outcome"] == "proved":
                expect(result).to_contain_text("Proved")
            else:
                expect(result).to_contain_text("Not proved")
    for n in numbers:
        if n > 5:
            expect(page.locator(f"details[data-p2m-trial='{n}'] [data-trial-facts]")).to_contain_text(f"Trial {n} ")
    sums = {c: [0, 0] for c in ("proved", "calibration", "open")}
    for r in rows.all():
        for c in sums:
            cell = r.locator(f"[data-trial-numbers] [data-cell={c}]")
            p, n = _of(cell.text_content()) if cell.count() else (0, 0)
            sums[c][0] += p
            sums[c][1] += n
    foot = page.locator(TRIALS_ALL)
    for c, (p, n) in sums.items():
        expect(foot.locator(f"[data-cell={c}]")).to_have_text(f"{p} of {n}")


def _compact(v: int) -> str:
    """The Tau Ceti page's format: 16968593 -> "17.0M", 405114 -> "405k"."""
    return f"{v / 1e6:.1f}M" if v >= 1e6 else f"{round(v / 1e3)}k" if v >= 1e3 else str(v)


def _tok_cell(cell, rows):
    """Tokens computed in the cell, the count with the cache re-reads beside it (attribute, hover)."""
    want = {k: sum(r[k] for r in rows) for k in TOK}
    computed = _computed(want)
    with_cache = want["prompt_tokens"] + want["output_tokens"]
    expect(cell).to_have_attribute("data-tokens", str(computed))
    expect(cell).to_have_attribute("data-with-cache", str(with_cache))
    expect(cell).to_have_text(_compact(computed))
    title = cell.get_attribute("title")
    assert f"{computed:,} tokens computed" in title, title
    assert f"{with_cache:,} with" in title, title


def test_tokens_per_run_and_per_trial(site_pages, dist_url, data):
    """Every run's tokens computed (read without the cache, plus written) in compact form, with the count
    with the cache re-reads in its hover text: trials 1 to 5 and their runs recomputed here from the frozen data; the All
    row the sum of every row (the outline probes' too), the scorecard's All row the same, and the share of cache re-reads
    in "How to read this" from those sums."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    _open_all(page, "details[data-p2m-trial]")
    for tr in [t for t in data["trials"] if t["trial"] <= 5]:
        _tok_cell(_trial(page, tr["trial"]).locator("[data-trial-numbers] .tok"), tr["leaves"])
        for r in tr["leaves"]:
            _tok_cell(page.locator(f"details[data-p2m-trial='{tr['trial']}'] tr[data-leaf='{r['leaf']}'] .tok"), [r])
    cells = page.locator(f"{TRIAL_ROWS} [data-trial-numbers] .tok, {TRIAL_ROWS}[data-probes] .tok").all()
    computed = sum(int(c.get_attribute("data-tokens")) for c in cells)
    with_cache = sum(int(c.get_attribute("data-with-cache")) for c in cells)
    foot = page.locator(f"{TRIALS_ALL} .tok")
    expect(foot).to_have_attribute("data-tokens", str(computed))
    expect(foot).to_have_attribute("data-with-cache", str(with_cache))
    expect(page.locator("details[data-p2m-method]")).to_contain_text("Tokens computed are the work the GPU did")
    expect(page.locator("[data-p2m-cache-share]")).to_have_text(f"{round(100 * (with_cache - computed) / with_cache)}%")
    expect(page.locator("[data-p2m-cache-times]")).to_have_text(str(round(with_cache / computed)))
    score = page.locator("[data-p2m-scorecard]")
    expect(score.locator("tfoot [data-cell=tokens]")).to_have_attribute("data-tokens", str(computed))
    expect(score.locator("tfoot [data-cell=hours]")).to_have_text(page.locator(f"{TRIALS_ALL} [data-cell=hours]").inner_text())
    probes = page.locator(f"{TRIAL_ROWS}[data-probes] .tok").all()
    if probes:
        row = score.locator("tbody tr[data-role=probes]")
        expect(row.locator("th")).to_have_text("Outline probes")
        expect(row.locator("[data-cell=tokens]")).to_have_attribute("data-tokens", str(sum(int(c.get_attribute("data-tokens")) for c in probes)))
        expect(row.locator("[data-cell=runs]")).to_have_text("–")
    else:
        expect(score.locator("tbody tr[data-role=probes]")).to_have_count(0)


def test_working_on_now_names_the_current_trial(site_pages, dist_url, data):
    """The build's copy (the records the site's build fetched): a block per trial not finished and per bench with open
    tasks, each named with its start and scaffold, with no description and no budget; its leaves with their state, their
    tokens computed and with the cache, their outlines and a task table whose count is its rows; and the line that says
    it is the build's copy."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    box = page.locator("[data-p2m-now]")
    expect(box).to_have_attribute("data-source", "build")
    blocks = box.locator("[data-now-trial]")
    if blocks.count() == 0:
        expect(box).to_contain_text("No trial is running.")
        return
    assert box.get_attribute("data-trial").split() == [b.get_attribute("data-now-trial") for b in blocks.all()]
    head = re.compile(r"^(Trial \d+(, since \d+ \w+ \d{4})? · scaffold \S+|Trial \d+ (is being prepared|has ended) · scaffold \S*"
                      r"|Bench \S+(, since \d+ \w+ \d{4})? · scaffold \S*)$")
    for b in blocks.all():
        expect(b.locator("[data-now-head]")).to_have_text(head)
    expect(box.locator("[data-now-about], [data-now-budget]")).to_have_count(0)
    assert "Budget:" not in box.inner_text()
    # one row per leaf, closed: its id and its line; opened, the rest
    for li in box.locator("li[data-current-leaf]").all():
        expect(li.locator(":scope > details[data-leaf-detail] > summary .leaf")).to_have_text(re.compile(r"^p2m-[0-9a-f]{6}"))
    page.locator("[data-p2m-now] details[data-leaf-detail]").evaluate_all("ds => ds.forEach((d) => (d.open = true))")
    for li in box.locator("li[data-current-leaf]").all():
        assert li.get_attribute("data-state") in ("running", "queued", "finished", "failed", "cancelled", "planned"), li.get_attribute("data-state")
        tok = li.locator(".meta [data-tokens]")
        if tok.count():
            assert int(tok.get_attribute("data-computed")) <= int(tok.get_attribute("data-tokens"))
            expect(tok).to_contain_text("tokens computed")
        table = li.locator("details[data-leaf-tasks]")
        if table.count():
            m = re.search(r"\((\d+)\)", table.locator("summary").inner_text())
            assert m, table.locator("summary").inner_text()
            expect(table.locator("tbody tr[data-task]:not([data-task=all])")).to_have_count(int(m.group(1)))
    expect(box.locator("[data-now-asof]")).to_contain_text("copy from the site’s last build")


def test_the_networks_share_shows_real_numbers_only(site_pages, dist_url):
    """With /v1/stats data the box shows project prove2me's numbers (today's share of requests too); with a failing
    endpoint it stays hidden, never dashes or zeros."""
    page = _page(site_pages, STATS)
    page.goto(dist_url + PAGE)
    box = page.locator("[data-p2m-live]")
    expect(box).to_be_visible()
    expect(box.locator("[data-live=today-requests]")).to_have_text("100")
    expect(box.locator("[data-live=today-share]")).to_have_text("25%")
    expect(box.locator("[data-live=requests]")).to_have_text("1,234")
    expect(box.locator("[data-live=tokens]")).to_have_text("1.7M")      # tokens computed: 1.0M read fresh + 0.7M written
    expect(box).to_contain_text("tokens computed in all")
    expect(box.locator("[data-live=hours]")).to_have_text("10.0")
    expect(box.locator("time[data-live=updated]")).to_have_text("16:10")
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    page.wait_for_load_state("networkidle")
    expect(page.locator("[data-p2m-live]")).to_be_hidden()
    for bad in ("undefined", "NaN"):
        expect(page.locator("main")).not_to_contain_text(bad)


def test_how_to_read_and_nothing_internal(site_pages, dist_url, data):
    """"How to read this" says who judges and links the JSON; the page names no reviewer model, tool or path."""
    page = _page(site_pages, STATS)
    page.goto(dist_url + PAGE)
    main = page.locator("main")
    expect(page.locator("h2#how-to-read-this")).to_have_text("How to read this")
    expect(main).to_contain_text("The Lean kernel is the judge")
    expect(main).to_contain_text("our own measurements")
    expect(main).to_contain_text("AIs@home is not part of prove2me")
    expect(page.locator("a[data-p2m-json]")).to_have_attribute("href", JSON)
    # the JSON's totals stop at its export: the page dates it and links the live records beside it (code site-live-numbers)
    if page.locator("[data-p2m-json-asof]").count():
        expect(page.locator("[data-p2m-json-asof]")).to_have_text(re.compile(r"^\d{1,2} [A-Z][a-z]+ 20\d\d$"))
        for what, href in (("campaigns", "/v1/projects/prove2me/campaigns"), ("results", "/v1/projects/prove2me/results"),
                           ("tasks", "/v1/tasks?project=prove2me")):
            expect(page.locator(f"a[data-p2m-records-link={what}]")).to_have_attribute("href", href)
    text = main.inner_text().lower()
    for bad in INTERNAL:
        assert bad not in text, f"the page says {bad!r}"
    for bad in ("undefined", "NaN"):
        expect(main).not_to_contain_text(bad)


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_the_page_fits_a_phone_in_both_themes(site_pages, dist_url, theme):
    """At 390 px, with the live box shown and every trial and every leaf's task table open, nothing scrolls sideways and
    the rows fit."""
    page = _page(site_pages, STATS, width=390)
    page.goto(dist_url + PAGE)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    expect(page.locator("[data-p2m-live]")).to_be_visible()
    _open_all(page, "details[data-p2m-trial], details[data-leaf-detail], details[data-p2m-method]")
    page.locator("details[data-leaf-tasks]").evaluate_all("ds => ds.forEach((d) => (d.open = true))")
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    _open_all(page, "details[data-p2m-list], details[data-p2m-alltasks]")
    # every trial's tasks: the default (the newest trial) can have none yet, so no row would be measured
    trial = page.locator("select[data-tt-filter='label:trial']")
    if trial.count():
        trial.first.select_option("")
    sels = [TRIAL_ROWS, "details[data-p2m-trial] tbody tr", "[data-p2m-submissions] tbody tr", "[data-p2m-now] .now-row", "[data-p2m-headline]",
            "[data-p2m-scorecard] tbody tr", "[data-p2m-leaf-table] tbody tr", "[data-tt-table] tbody tr"]
    if page.locator("details[data-leaf-tasks] tbody tr").count():
        sels.append("details[data-leaf-tasks] tbody tr")
    for sel in sels:
        box = page.locator(f"{sel} >> visible=true").first.bounding_box()
        assert box and box["x"] >= 0 and box["x"] + box["width"] <= 390, f"{sel} on a phone: {box}"


def test_sidebar_links_the_page(site_pages, dist_url):
    """The sidebar's Science group links prove2me and marks it current on the page."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + "/science/tauceti/")
    page.locator("nav.sidebar").get_by_role("link", name="prove2me", exact=True).click()
    page.wait_for_url(dist_url + PAGE)
    expect(page.locator("nav.sidebar").get_by_role("link", name="prove2me", exact=True)).to_have_attribute("aria-current", "page")


def test_a_finished_v5_trial_keeps_its_tasks(site_pages, dist_url, data):
    """A finished v5 trial (trial 5 on, a leaf is many tasks) is in the Trials history, not in "Working on now": its
    leaves table counts each leaf's tasks and outlines, a proved leaf names the outline that proved it, and below it
    each leaf's task table, collapsed, has one row per task and the leaf's total. Its accepted proof (F01) is listed
    under the submissions with its trial. Recomputed here from the JSON."""
    v5 = [tr for tr in data["trials"] if any(r.get("tasks") for r in tr["leaves"])]
    if not v5:
        pytest.skip("no finished v5 trial in the data")
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    now = page.locator("[data-p2m-now]")
    for tr in v5:
        expect(now).not_to_have_attribute("data-trial", f"t{tr['trial']}")
        details = _open_trial(page, tr["trial"])
        expect(details.locator("table.p2m-leaves thead")).to_contain_text("Tasks")
        box = details.locator(f"[data-p2m-trial-tasks='{tr['trial']}']")
        for r in tr["leaves"]:
            row = details.locator(f"table.p2m-leaves tr[data-leaf='{r['leaf']}']")
            ngen = len({tk["gen"] for tk in r["tasks"]})
            expect(row.locator("td[data-label=Tasks]")).to_contain_text(str(len(r["tasks"])))
            expect(row.locator("td[data-label=Tasks]")).to_contain_text(f"{ngen} outline{'s' if ngen != 1 else ''}")
            if r["outcome"] == "proved" and ngen > 1:
                expect(row.locator(".result")).to_contain_text(f"Proved with outline {r['proved_in_outline']}")
            table = box.locator(f"details[data-leaf-tasks='{r['leaf']}']")
            expect(table).not_to_have_attribute("open", "")
            lid = next(l["id"] for l in data["leaves"] if l["theorem"] == r["theorem"])
            expect(table.locator("summary")).to_have_text(f"{lid} ({r['leaf']}): every task ({len(r['tasks'])})")
            # above the table, the leaf's outlines newest first and its line
            plan = box.locator(f"[data-trial-leaf='{r['leaf']}'] [data-leaf-plan='{r['leaf']}']")
            gens = sorted({tk["gen"] for tk in r["tasks"]}, reverse=True)
            assert [int(o.get_attribute("data-outline")) for o in plan.locator(".p2m-outline").all()] == gens, r["leaf"]
            asm = [tk for tk in r["tasks"] if tk["role"] == "asm" and tk["outcome"] == "proved"]
            if asm:
                expect(plan.locator("[data-leaf-summary]")).to_have_text(f"Proved: outline {asm[0]['gen']} assembled")
                expect(plan.locator(f".p2m-outline[data-outline='{asm[0]['gen']}']")).to_have_attribute("data-outline-state", "proved")
            table.locator("summary").click()
            expect(table.locator("tbody tr")).to_have_count(len(r["tasks"]))
            for tk in r["tasks"]:
                trow = table.locator(f"tr[data-task='{tk['id']}']")
                expect(trow).to_have_attribute("data-state", tk["state"])
                expect(trow).to_have_attribute("data-gen", str(tk["gen"]))
            total = sum((tk["prompt_tokens"] or 0) + (tk["output_tokens"] or 0) for tk in r["tasks"])
            expect(table.locator("tr[data-task=all] td[data-total]")).to_have_attribute("data-total", str(total))
            assert total == r["prompt_tokens"] + r["output_tokens"], r["leaf"]
    for s in data["submissions"]:
        if s["kind"] == "proof" and any(s["trial"] == tr["trial"] for tr in v5):
            row = page.locator(f"tr[data-submission='{s['leaf']}']")
            expect(row.locator("td[data-label=From]")).to_have_text(f"Trial {s['trial']}")
            if s["verdict"] == "accepted":
                expect(row.locator(".verdict")).to_contain_text("Accepted")


def _built_kpis(site_pages, dist_url) -> dict:
    """The prove2me page's own headline numbers as the build made them (the gateway's records refused)."""
    page = _page(site_pages, "fail")
    page.goto(dist_url + PAGE)
    k = page.locator("[data-p2m-kpis]")
    out = {"accepted": k.locator("[data-kpi=accepted] dd").inner_text(),
           "submitted": re.search(r"of (\d+) submitted", k.locator("[data-kpi=accepted] dt").inner_text()).group(1),
           "open": k.locator("[data-kpi=open] dd").inner_text(),
           "tried": re.search(r"of (\d+) tried", k.locator("[data-kpi=open] dt").inner_text()).group(1),
           "reused": k.locator("[data-kpi=reused] dd").inner_text() if k.locator("[data-kpi=reused]").count() else "",
           "running": [r.get_attribute("data-trial") for r in page.locator(f"{TRIAL_ROWS}[data-status]:not([data-probes])").all()]}
    page.close()
    return out


def test_overview_lists_prove2me_first_then_tau_ceti(site_pages, dist_url, data):
    """One card per project, prove2me (active) first, then Tau Ceti (finished); prove2me's numbers are its own page's
    (the build's records), Tau Ceti's its JSON's; each card's link opens its page; the sidebar lists Overview, prove2me,
    Proved theorems, Tau Ceti in that order."""
    k = _built_kpis(site_pages, dist_url)
    page = _page(site_pages, "fail")
    page.goto(dist_url + "/science/")
    expect(page.locator("h1")).to_have_text("Science")
    cards = page.locator("[data-science-overview] > li")
    expect(cards).to_have_count(2)
    assert [c.get_attribute("data-project") for c in cards.all()] == ["prove2me", "tauceti"]
    # built as the Tau Ceti card: a status line, one line, two numbers, the date and links
    p2m = page.locator("li[data-project=prove2me]")
    status = p2m.locator("[data-project-status]").inner_text()
    m = re.fullmatch(r"(?i)Active(?:: trial (\d+) (?:running since \d+ \w+ \d{4}|running|being prepared))?", status.strip())
    assert m, status
    if m.group(1):          # the newest trial while it runs or is prepared: a running row of the trials
        assert m.group(1) in k["running"], (status, k["running"])
    expect(p2m.locator("[data-number]")).to_have_count(2)
    expect(p2m.locator("[data-number=accepted] dd")).to_have_text(k["accepted"])
    expect(p2m.locator("[data-number=accepted] dt")).to_have_text("Lean proofs by the model accepted by prove2me")
    expect(p2m.locator("[data-number=open] dd")).to_have_text(f"{k['open']} of {k['tried']}")
    expect(p2m.locator("a[data-card-link=theorems]")).to_have_attribute("href", "/science/prove2me/theorems/")
    tc = _tauceti()
    merged = sum(p["state"] == "merged" for p in tc["prs"])
    reviewed = [p for p in tc["prs"] if p["first_review_approved"] is not None]
    tau = page.locator("li[data-project=tauceti]")
    expect(tau.locator("[data-project-status]")).to_contain_text("Finished: stopped at the maintainers’ request on 7 November 2026")
    expect(tau.locator("[data-number=merged] dd")).to_have_text(f"{merged} of {len(tc['prs'])}")
    expect(tau.locator("[data-number=first-review] dd")).to_have_text(
        f"{sum(p['first_review_approved'] is True for p in reviewed)} of {len(reviewed)}")
    links = page.locator("nav.sidebar ul.top-level a").all()
    sidebar = [(a.get_attribute("href"), a.inner_text().strip()) for a in links]
    i = [h for h, _ in sidebar].index("/science/")
    want = [("/science/", "Overview"), (PAGE, "prove2me"), ("/science/prove2me/theorems/", "Proved theorems"), ("/science/tauceti/", "Tau Ceti")]
    if "/science/prove2me/theorems/" not in [h for h, _ in sidebar]:
        want = [w for w in want if w[0] != "/science/prove2me/theorems/"]
    assert sidebar[i:i + len(want)] == want, sidebar
    expect(page.locator("nav.sidebar ul.top-level a[href='/science/']")).to_have_attribute("aria-current", "page")
    for project, path in (("prove2me", PAGE), ("tauceti", "/science/tauceti/")):
        page.goto(dist_url + "/science/")
        page.locator(f"li[data-project={project}] a[data-project-link]").click()
        page.wait_for_url(dist_url + path)
    text = page.locator("main").inner_text().lower()
    for bad in INTERNAL:
        assert bad not in text, f"the overview says {bad!r}"


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_overview_fits_a_phone_in_both_themes(site_pages, dist_url, theme):
    page = _page(site_pages, "fail", width=390)
    page.goto(dist_url + "/science/")
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    expect(page.locator("[data-science-overview] > li")).to_have_count(2)
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the overview scrolls sideways"
    for c in page.locator("[data-science-overview] > li").all():
        box = c.bounding_box()
        assert box["x"] >= 0 and box["x"] + box["width"] <= 390, f"a card on a phone: {box}"


def test_home_links_to_the_science_and_prove2me(site_pages, dist_url, data):
    """The home page's science section names prove2me first with its page's accepted and open-leaf counts (the build's
    records), links its page and the overview, and keeps Tau Ceti's line; the volunteer pitch and the live strip stay."""
    k = _built_kpis(site_pages, dist_url)
    page = _page(site_pages, "fail")
    page.route("**/v1/projects/tauceti/science", lambda r: r.abort())
    page.goto(dist_url + "/")
    section = page.locator("[data-home-science]")
    line = section.locator("[data-home-prove2me]")
    expect(line.locator("[data-home-p2m-accepted]")).to_have_text(k["accepted"])
    # one number and what it is from; the ways in are links of their own
    expect(line.locator("[data-home-p2m-text]")).to_have_text(re.compile(rf"^Lean proofs? by .+ accepted by prove2me so far, from {k['tried']} open (?:leaf|leaves) tried\.$"))
    expect(section.locator("a[data-home-theorems-link]")).to_have_attribute("href", "/science/prove2me/theorems/")
    expect(line).to_have_attribute("data-source", "build")
    # the chart beside it (home-science-first): a point per accepted proof, the total at the end, the same rows in its table
    chart = section.locator("[data-home-p2m-chart]")
    expect(chart.locator("svg[role=img] circle")).to_have_count(int(k["accepted"]))
    expect(chart.locator("svg text.end-n")).to_have_text(k["accepted"])
    assert chart.locator("svg[role=img]").get_attribute("aria-label").startswith(f"{k['accepted']} proof")
    expect(chart.locator("tbody tr")).to_have_count(int(k["accepted"]))
    box = line.bounding_box()
    tau = section.locator("[data-home-tauceti]")
    if tau.count():
        assert box["y"] < tau.bounding_box()["y"], "prove2me is not first"
    expect(page.locator("[data-live-stats]")).to_have_count(1)
    section.locator("a[data-home-p2m-link]").click()
    page.wait_for_url(dist_url + PAGE)
    page.goto(dist_url + "/")
    page.locator("[data-home-science] a[data-home-science-all]").click()
    page.wait_for_url(dist_url + "/science/")
    page.set_viewport_size({"width": 390, "height": 844})
    page.goto(dist_url + "/")
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the home page scrolls sideways"
    chart_box = page.locator("[data-home-p2m-chart] svg").bounding_box()
    assert chart_box["x"] >= 0 and chart_box["x"] + chart_box["width"] <= 390, f"the chart on a phone: {chart_box}"


# ---- the proved theorems (/science/prove2me/theorems/ and a proof's own page), as built

THEOREMS = "/science/prove2me/theorems/"
PROOF_PAGE = re.compile(r"^/science/prove2me/theorems/(p2m-[0-9a-f]{6}(?:[0-9a-f]{2})?)/$")


def test_the_theorems_page_says_what_the_count_is(site_pages, dist_url):
    """The list leads with the count of the model's accepted proofs and says what it is out of, in words: the scope
    line (open leaves with a proof of those tried, the submissions and their accepted proofs), that we chose only leaves
    we judged easy or moderate, the link to what the count means; the KPIs say the same; the call to action names the
    node page and the 24 GB cards; both charts are there. (That every number agrees, as built and live:
    test_site_prove2me.py.)"""
    page = _page(site_pages)
    page.goto(dist_url + THEOREMS)
    expect(page.locator("h1")).to_have_text("Proved theorems")
    hero = page.locator("[data-th-headline]")
    n = int(hero.locator("[data-th-headline-n]").inner_text())
    expect(hero).to_contain_text("accepted by prove2me")
    scope = hero.locator("[data-th-scope]")
    expect(scope).to_contain_text(re.compile(r"Proofs of \d+ of the \d+ open leaves tried\. \d+ submissions?: \d+ accepted proofs?"))
    expect(scope).to_contain_text("we chose only leaves we judged easy or moderate")
    expect(scope.locator("a[href='#counts']")).to_have_text("What the count means →")
    assert page.locator("[data-th-rows] tr[data-kind=proof]").count() == n
    m = re.search(r"Proofs of (\d+) of the (\d+) open leaves tried\. (\d+) submissions?: (\d+) accepted proofs?", scope.inner_text())
    assert m and int(m.group(4)) == n, scope.inner_text()
    kpis = page.locator("[data-th-kpis]")
    expect(kpis.locator("[data-kpi=open] dt")).to_have_text(f"open leaves with an accepted proof, of {m.group(2)} tried")
    expect(kpis.locator("[data-kpi=submitted] dt")).to_contain_text(f"submissions: {n} accepted proof")
    cta = page.locator("[data-th-cta]")
    expect(cta).to_contain_text("Your GPU could help prove the next lemma.")
    expect(cta.locator("[data-th-contribute]")).to_have_attribute("href", "/node/")
    expect(cta).to_contain_text("24 GB")
    expect(page.locator("[data-p2m-chart] svg")).to_have_count(1)
    expect(page.locator("[data-th-bars] svg")).to_have_count(1)
    expect(page.locator("a[data-th-work-link]")).to_have_attribute("href", PAGE)


def test_a_proof_page_says_what_it_took(site_pages, dist_url):
    """Every accepted proof in the list links a page of its own; the newest renders: its title, "Accepted by
    prove2me", its leaf, the GPU-hours in its call to action the same as its facts', its statement on prove2me, its own
    share card (built), and its tasks under "The tasks" with a "Served by" column."""
    page = _page(site_pages)
    page.goto(dist_url + THEOREMS)
    links = page.locator("[data-th-rows] tr[data-kind=proof] th a").evaluate_all("as => as.map(a => a.getAttribute('href'))")
    assert links and all(PROOF_PAGE.match(h) for h in links), links
    href = links[0]
    leaf = PROOF_PAGE.match(href).group(1)
    proof = _page(site_pages)
    proof.goto(dist_url + href)
    expect(proof.locator("h1")).not_to_be_empty()
    expect(proof.locator(".th-kicker")).to_contain_text("Accepted by prove2me")
    expect(proof.locator("[data-th-facts] [data-fact=leaf]")).to_contain_text(leaf)
    hours = proof.locator("[data-th-facts] [data-fact=hours] dd").inner_text()
    expect(proof.locator("[data-th-cta] [data-th-cta-hours]")).to_have_text(f"This proof took {hours} GPU-hours of inference.")
    expect(proof.locator("[data-statement]").first).to_have_attribute("href", THEOREM)
    og = proof.locator("meta[property='og:image']").get_attribute("content")
    assert og.endswith(f"{href}og.png"), og
    assert (DIST_DIR / href.strip("/") / "og.png").exists(), f"{href}og.png is not built"
    expect(proof.locator("h2#tasks")).to_have_text("The tasks")
    expect(proof.locator("[data-th-tasks] thead th").last).to_have_text("Served by")


def test_the_sidebar_names_the_proved_theorems(site_pages, dist_url):
    """The sidebar's Science group names the list "Proved theorems"; the prove2me page's headline and its
    submissions' lead line link it."""
    page = _page(site_pages)
    page.goto(dist_url + PAGE)
    expect(page.locator("nav.sidebar a[href='/science/prove2me/theorems/']")).to_have_text("Proved theorems")
    expect(page.locator("a[data-p2m-theorems-link=headline]")).to_have_attribute("href", THEOREMS)
    expect(page.locator("a[data-p2m-theorems-link=submissions]")).to_have_attribute("href", THEOREMS)
