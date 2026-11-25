"""The prove2me science page (/science/prove2me/), the science overview (/science/) and the home page's science line,
in a browser, against fixed documents: no staging and no gateway needed.

The site is the build in site/dist, built from the same fixed records (the build fetches the gateway's records itself
otherwise: site/src/data/prove2me-build.ts), so the page without JavaScript is theirs:

  cd site && npm ci && P2M_RECORDS=tests/fixtures/prove2me-records-2026-11-18.json npm run build

CI builds it so first. It is served here by a small static server. Every request the pages make to the gateway (/v1/...) or for the project's data document
(/data/prove2me/current.json) is answered by the test: a fixed document, an HTTP error, or the gateway's public records
as they were on 2026-11-18 (site/tests/fixtures). Checked here: "Working on now" live from the data document or the
trial campaigns not finished (a block each) and the tasks; each leaf's outlines and holes in words; the headline numbers, the submissions,
the trials table, each trial's list from trial 6 on and every leaf live from the records, with
tokens computed leading; a bench in the status block and in "Working on now", shown as a trial; the home page's line and the
/science/ card; and the build's copy (the records at the build) when a source fails.

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_prove2me.py

SPDX-License-Identifier: Apache-2.0
"""
import functools
import json
import os
import re
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from playwright.sync_api import expect

SRC = Path(__file__).resolve().parents[2]
DIST = Path(os.environ.get("SITE_DIST", SRC / "site" / "dist"))
DATA = SRC / "site/src/data/science/prove2me.json"
RECORDS = SRC / "site/tests/fixtures/prove2me-records-2026-11-18.json"
PAGE = "/science/prove2me/"


class _Quiet(SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


@pytest.fixture(scope="module")
def site():
    if not (DIST / "science/prove2me/index.html").exists():
        if os.environ.get("CI"):
            pytest.fail(f"no site build in {DIST}")
        pytest.skip(f"no site build in {DIST} (cd site && npm ci && npm run build)")
    srv = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(_Quiet, directory=str(DIST)))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


@pytest.fixture
def pages(browser):
    opened = []

    def make(js=True):
        ctx = browser.new_context(viewport={"width": 1400, "height": 900}, java_script_enabled=js)
        ctx.set_default_timeout(15_000)
        opened.append(ctx)
        return ctx.new_page()

    yield make
    for c in opened:
        c.close()


@pytest.fixture(scope="module")
def data() -> dict:
    return json.loads(DATA.read_text())


@pytest.fixture(scope="module")
def records() -> dict:
    return json.loads(RECORDS.read_text())


def _serve(page, url, what):
    if isinstance(what, int):
        page.route(url, lambda r: r.fulfill(status=what, body="no"))
    else:
        body = json.dumps(what)
        page.route(url, lambda r: r.fulfill(status=200, content_type="application/json", body=body))


def _page(pages, width=None, live="fail", records=None, js=True):
    """A page whose gateway answers as given. `live`: "Working on now"'s sources (/data/prove2me/current.json and
    /v1/tasks): "fail" (both HTTP 500), or a (document, tasks) pair (each a dict, or an int status). `records`: the
    campaigns and results (a dict with "campaigns" and "results", each a dict or an int status), else HTTP 500. Any other
    request to the gateway fails (/v1/stats among them)."""
    page = pages(js)
    if width:
        page.set_viewport_size({"width": width, "height": 844})
    page.route("**/v1/**", lambda r: r.fulfill(status=500, body="no"))
    page.route("**/data/**", lambda r: r.fulfill(status=404, body="no"))
    if live == "fail":
        live = (500, 500)
    _serve(page, "**/data/prove2me/current.json", live[0])
    _serve(page, "**/v1/tasks?*", live[1])
    rec = records or {}
    _serve(page, "**/v1/projects/prove2me/campaigns?*", rec.get("campaigns", 500))
    _serve(page, "**/v1/projects/prove2me/results?*", rec.get("results", 500))
    return page


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
    """One task of GET /v1/tasks (the gateway's tasks_public.go schema)."""
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


# ---- "Working on now" (moved here from the staging tests, 2026-11-18: they need no staging)

def test_working_on_now_is_live_from_the_document_and_the_tasks(pages, site, data):
    """With the data document and GET /v1/tasks, "Working on now" is theirs (data-source=live): the document's trial,
    start and scaffold in the heading, its Markdown-light text (paragraphs, code, emphasis; nothing as raw HTML), its
    budget, its leaves in its order with their ids, aliases, roles and theorem links; each leaf's state from the trial's
    tasks (another trial's or a probe's never count), a running task's steps and tokens from the gateway's live
    counters; a leaf with no task yet "Not started yet"; a submission of a listed leaf on its line, one of another leaf
    under the list; #leaf-<id> and #leaf-<alias> both find the leaf."""
    doc = _fix_doc(data, schema=1, description="Scaffold v7. Holes are `sorry`; <b>no html</b>.\n\nSee [prove2me](https://prove2.me/) and *more*.")
    ids = {l["alias"]: l["id"] for l in doc["leaves"]}
    live = _fix_live()     # X03: no task yet
    doc["submissions"] = [
        {"leaf": ids["X02"], "alias": "X02", "kind": "proof", "theorem": doc["leaves"][1]["theorem"], "url": doc["leaves"][1]["url"],
         "trial": 96, "submitted_at": "2026-11-15T10:10:00Z", "verdict": "accepted", "verdict_at": "2026-11-15T10:12:00Z"},
        {"leaf": "p2m-b1d59e", "alias": "N02", "kind": "reuse", "theorem": "occupation_bind_sections_integrable_local",
         "url": "https://prove2.me/theorems/b1d59eba-4373-4046-8d79-4f717dd511ca", "trial": 96,
         "submitted_at": "2026-11-14T21:54:16Z", "verdict": "accepted", "verdict_at": "2026-11-14T21:55:33Z"}]
    page = _page(pages, live=(doc, _tasks(live)))
    page.goto(f"{site}{PAGE}#leaf-{ids['X01']}")
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t96]")
    expect(box).to_be_visible()
    expect(box.locator("[data-now-head]")).to_have_text("Trial 96, since 15 November 2026 · scaffold v7")
    # the block states what is worked on: no description of the trial, no budget
    expect(box.locator("[data-now-about], [data-now-budget]")).to_have_count(0)
    assert "no html" not in box.inner_text() and "Budget" not in box.inner_text()
    items = box.locator("li[data-current-leaf]")
    expect(items).to_have_count(3)
    assert [i.get_attribute("data-current-leaf") for i in items.all()] == ["X01", "X02", "X03"]
    for l in doc["leaves"]:
        li = box.locator(f"li[data-current-leaf={l['alias']}]")
        expect(li).to_have_attribute("data-leaf-id", l["id"])
        expect(li).to_have_attribute("id", f"leaf-{l['id']}")
        expect(li.get_by_role("link", name=l["theorem"], exact=True)).to_have_attribute("href", l["url"])
        expect(li.locator(".meta")).to_contain_text(f"{'Calibration' if l['role'] == 'calibration' else 'Open'} leaf")
        expect(li.locator(".leaf")).to_have_text(l["id"])                 # the id leads, the alias small
        expect(li.locator("[data-alias]")).to_have_text(l["alias"])
    x1 = box.locator("li[data-current-leaf=X01]")
    expect(x1).to_have_class(re.compile(r"\btarget\b"))
    expect(x1).to_have_attribute("data-state", "running")
    expect(x1.locator(".state")).to_have_text("Running: outline 1, hole 1")
    o1 = x1.locator("[data-leaf-plan=X01] .p2m-outline[data-outline='1']")
    expect(o1.locator("[data-outline-text]")).to_have_text("in progress")
    expect(o1.locator("[data-outline-cost]")).to_have_text("written in 40 steps · 50.0 min")
    expect(o1.locator("li[data-hole='1'] .p2m-htext")).to_have_text("Working (try 1, 12 steps so far)")   # the live counters
    expect(o1.locator("li[data-hole=asm] .p2m-htext")).to_have_text("queued")
    tasks = x1.locator("details[data-leaf-tasks=X01]")
    tasks.locator("summary").click()
    hole = tasks.locator(f"tr[data-task='t96-{ids['X01']}-s1-h01']")
    expect(hole.locator("td[data-label=Steps]")).to_have_text("12")
    expect(hole.locator("td[data-label='Model min']")).to_have_text("10.0")
    expect(hole.locator("td[data-total]")).to_have_attribute("data-total", str(300_000 + 1_200_000 + 20_000))
    expect(hole.locator("td[data-computed]")).to_have_attribute("data-computed", str(300_000 + 20_000))
    total = (2_000_000 + 8_000_000 + 100_000) + (300_000 + 1_200_000 + 20_000)
    expect(x1.locator(".meta [data-tokens]")).to_have_attribute("data-tokens", str(total))
    # tokens computed lead; with the cache re-reads beside them
    expect(x1.locator(".meta [data-tokens]")).to_have_text("2.4M tokens computed so far (11.6M with cache re-reads)")
    x2 = box.locator("li[data-current-leaf=X02]")
    expect(x2).to_have_attribute("data-state", "running")
    expect(x2.locator("[data-submission=accepted]")).to_have_text("submitted, accepted by prove2me (15 November)")
    x3 = box.locator("li[data-current-leaf=X03]")
    expect(x3).to_have_attribute("data-state", "planned")
    expect(x3.locator(".state")).to_have_text("Not started yet")
    expect(x3.locator("details")).to_have_count(0)
    expect(box.locator("[data-now-subs]")).to_have_text(
        "Also from this trial: p2m-b1d59e (N02), closed by reuse, accepted by prove2me on 14 November.")
    expect(box.locator("[data-now-asof]")).to_have_text(
        "Live: the trial’s data as published 15 November, 10:20 UTC; its tasks from the gateway, updated 10:21 UTC. "
        "It refreshes every minute.")
    assert "N09" not in box.inner_text() and "F01" not in box.inner_text(), "a probe's or another trial's task counts"
    page.goto(f"{site}{PAGE}#leaf-X02")
    expect(page.locator("[data-p2m-now][data-source=live] li[data-current-leaf=X02]")).to_have_class(re.compile(r"\btarget\b"))


def test_working_on_now_follows_a_newer_document(pages, site, data):
    """A document for a trial the build does not know yet: its heading and text, being prepared with no leaf and no task;
    then running with a leaf the build never saw (its theorem linked from the document) and the gateway's first task."""
    doc = _doc(data, trial=97, leaves=[], status="preparing", start=None, scaffold="v9", description="Scaffold v9: next.",
               budget="")
    page = _page(pages, live=(doc, _tasks([])))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t97]")
    expect(box).to_be_visible()
    expect(box.locator("[data-now-head]")).to_have_text("Trial 97 is being prepared · scaffold v9")
    expect(box.locator("li[data-current-leaf]")).to_have_count(0)
    url = "https://prove2.me/theorems/0123abcd-0000-4000-8000-000000000001"
    leaf = {"id": "p2m-0123ab", "alias": "Z01", "role": "open", "theorem": "E2e.newer_leaf", "url": url}
    doc = _doc(data, trial=97, leaves=[leaf], status="running", start="2026-12-01", scaffold="v9", description="Scaffold v9.")
    task = _ptask("t97-p2m-0123ab-s1", "t97", "Z01", "p2m-0123ab", "outline", "leased", live={"requests": 3})
    page = _page(pages, live=(doc, _tasks([task])))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t97]")
    expect(box.locator("[data-now-head]")).to_have_text("Trial 97, since 1 December 2026 · scaffold v9")
    li = box.locator("li[data-current-leaf=Z01]")
    expect(li).to_have_attribute("data-state", "running")
    expect(li.get_by_role("link", name="E2e.newer_leaf")).to_have_attribute("href", url)
    expect(li.locator(".state")).to_have_text("Running: outline 1 (being written)")
    expect(li.locator(".p2m-outline[data-outline='1'] [data-outline-text]")).to_have_text("being written")


# ---- a leaf's outlines in words (2026-11-15: the task table and its one-line count were hard to follow)

# Trial 6's three leaves as they stood on 15 November 2026 (GET /v1/tasks and the data document of that day), moved to
# the fixtures' trial 98 so that nothing of the build's trial merges in: N03 (p2m-dbd69f), whose outline 1 was refuted
# by a false hole that our check first failed on and a re-judgment then found disproved, and whose outline 2 runs;
# C07 (p2m-3ad60f), proved by assembly after the index and the pre-pass closed two of its three holes; N05
# (p2m-f181df), closed by reuse (its outline was one line using a proved theorem; a second outline was cancelled).
OUT_LEAVES = [
    {"id": "p2m-dbd69f", "alias": "N03", "role": "open", "theorem": "PerturbedWF.eq_2_3",
     "url": "https://prove2.me/theorems/dbd69f70-c8c7-4b16-b27d-a8ea3d7ac435"},
    {"id": "p2m-3ad60f", "alias": "C07", "role": "calibration",
     "theorem": "WilliamsonShmoys.planar_drawing_maximal_supergraph__p2m_97829890_428d_4657_a899_f7e200a68a7c_a4",
     "url": "https://prove2.me/theorems/3ad60fc3-1bed-4725-aaf6-264d650d407f"},
    {"id": "p2m-f181df", "alias": "N05", "role": "open", "theorem": "NestedSeatAlloc.IntPolicy.expRevenue_one_right_derivative",
     "url": "https://prove2.me/theorems/f181dfe1-72dd-485a-90a4-331ac8998977"},
]
REJUDGED = "try 1: our check failed first (fixed), re-judged: disproved"


def _out_doc(data, schema=2):
    leaves = [dict(l) for l in OUT_LEAVES]
    if schema == 2:
        leaves[0]["outlines"] = [
            {"outline": 1, "holes": [{"hole": 1, "kind": "inplace", "state": "open"},
                                     {"hole": 2, "kind": "lemma", "state": "false", "by": "model", "note": REJUDGED}]},
            {"outline": 2, "holes": [{"hole": 1, "kind": "lemma", "state": "open"},
                                     {"hole": 2, "kind": "lemma", "state": "proved", "by": "prepass"},
                                     {"hole": 3, "kind": "lemma", "state": "open"}, {"hole": 4, "kind": "lemma", "state": "open"}]}]
        leaves[1]["outlines"] = [
            {"outline": 1, "holes": [{"hole": 1, "kind": "lemma", "state": "proved", "by": "index"},
                                     {"hole": 2, "kind": "lemma", "state": "open"},
                                     {"hole": 3, "kind": "lemma", "state": "proved", "by": "prepass"}]}]
    reuse = {"leaf": "p2m-f181df", "alias": "N05", "kind": "reuse", "theorem": OUT_LEAVES[2]["theorem"], "url": OUT_LEAVES[2]["url"],
             "trial": 98, "submitted_at": "2026-11-15T10:50:29Z", "verdict": "accepted", "verdict_at": "2026-11-15T10:51:45Z"}
    if schema == 2:
        reuse["reuses"] = {"theorem": "NestedSeatAlloc.IntPolicy.eq27_er1_clbi", "relation": "part",
                           "url": "https://prove2.me/theorems/21543cdb-2cef-4c0d-8b7e-2981bdb2cf31"}
    return _doc(data, trial=98, leaves=leaves, submissions=[reuse], schema=schema)


def _out_tasks():
    """The three leaves' tasks with their states, outcomes, counts and relations as GET /v1/tasks gave them."""
    t = lambda id_, alias, leaf, role, state, outline="1", **kw: _ptask(f"t98-{leaf}-{id_}", "t98", alias, leaf, role, state, outline, **kw)
    n, c, f = "p2m-dbd69f", "p2m-3ad60f", "p2m-f181df"
    s1, s2 = f"t98-{n}-s1", f"t98-{n}-s2"
    return [
        t("s1", "N03", n, "outline", "done", outcome="outline", steps=59, model_min=27.0, prompt_tokens=49574,
          cached_tokens=2476007, completion_tokens=56678, tokens=106252, started_at="2026-11-15T09:57:00Z", finished_at="2026-11-15T10:36:37Z"),
        t("s1-h02", "N03", n, "hole", "done", hole=2, parent=s1, outcome="check_error", steps=19, model_min=7.0,
          prompt_tokens=24950, cached_tokens=346971, completion_tokens=14304, tokens=39254, hole_id="h-3ad4181432a9",
          started_at="2026-11-15T10:45:06Z", finished_at="2026-11-15T10:57:33Z"),
        t("s1-h02-a2", "N03", n, "hole", "cancelled", hole=2, try_=2, parent=s1, retry_of=f"{s1}-h02", hole_id="h-3ad4181432a9",
          finished_at="2026-11-15T11:00:43Z"),
        t("s1-rest", "N03", n, "rest", "cancelled", parent=s1, started_at="2026-11-15T10:57:44Z", finished_at="2026-11-15T11:15:41Z"),
        t("s2", "N03", n, "outline", "done", "2", outcome="outline", replaces=s1, steps=28, model_min=12.8, prompt_tokens=20614,
          cached_tokens=1017441, completion_tokens=29579, tokens=50193, started_at="2026-11-15T11:26:26Z", finished_at="2026-11-15T12:02:35Z"),
        t("s2-h01", "N03", n, "hole", "leased", "2", hole=1, parent=s2, hole_id="h-64bc841c37be", started_at="2026-11-15T12:02:53Z",
          live={"requests": 14, "prompt_tokens": 323843, "cached_tokens": 305523, "completion_tokens": 5681, "model_s": 106.2,
                "updated_at": "2026-11-15T12:11:06Z"}),
        t("s2-h03", "N03", n, "hole", "queued", "2", hole=3, parent=s2, hole_id="h-cac3ab8ef3b6"),
        t("s2-h04", "N03", n, "hole", "queued", "2", hole=4, parent=s2, hole_id="h-b308af8d430c"),
        t("s2-rest", "N03", n, "rest", "queued", "2", parent=s2),
        t("s1", "C07", c, "outline", "done", outcome="outline", steps=19, model_min=6.6, prompt_tokens=32651, cached_tokens=385762,
          completion_tokens=21944, tokens=54595, started_at="2026-11-15T09:12:22Z", finished_at="2026-11-15T09:49:47Z"),
        t("s1-h02", "C07", c, "hole", "done", hole=2, parent=f"t98-{c}-s1", outcome="proved", steps=6, model_min=1.7,
          prompt_tokens=11460, cached_tokens=71893, completion_tokens=2754, tokens=14214, hole_id="h-08fd942211ba",
          started_at="2026-11-15T09:50:00Z", finished_at="2026-11-15T09:55:38Z"),
        t("s1-asm", "C07", c, "assembly", "done", parent=f"t98-{c}-s1", needs=[f"t98-{c}-s1-h02"], outcome="proved", steps=0,
          model_min=0, prompt_tokens=0, cached_tokens=0, completion_tokens=0, tokens=0, started_at="2026-11-15T09:55:50Z",
          finished_at="2026-11-15T09:56:57Z"),
        t("s1", "N05", f, "outline", "done", outcome="proved", steps=11, model_min=2.9, prompt_tokens=22119, cached_tokens=212163,
          completion_tokens=6854, tokens=28973, started_at="2026-11-15T10:36:56Z", finished_at="2026-11-15T10:44:59Z"),
        t("s2", "N05", f, "outline", "cancelled", "2", replaces=f"t98-{f}-s1", finished_at="2026-11-15T10:49:45Z"),
    ]


def _outline(li, gen):
    return li.locator(f"[data-leaf-plan] .p2m-outline[data-outline='{gen}']")


def _holes(o) -> list[tuple[str, str, str, str]]:
    """Each hole's name, state (waiting, working, solved; the assembly's own), text and tooltip."""
    return [(h.locator(".p2m-hname").inner_text(), h.get_attribute("data-hole-state"), h.locator(".p2m-htext").inner_text(),
             h.get_attribute("title") or "") for h in o.locator("li[data-hole]").all()]


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_working_on_now_shows_each_outline_and_hole_in_words(pages, site, data, theme):
    """Per leaf: one line in words, then its outlines newest first, each with its state and why and the outline task's
    steps and minutes, and one row per hole with its state, a running try's live steps inside its row; the task table
    stays below. N03: "Running: outline 2, hole 1"; outline 2 in progress (hole 2 closed by the pre-pass, holes 3 and 4
    and the holes in place queued); outline 1 refuted because hole 2 is false: the re-judgment from the data document,
    never our check's failure as the last word; its hole in place cancelled with the outline. C07 proved by assembly,
    holes closed by the index and the pre-pass named. N05 closed by reuse, the theorem it imports named. At 390 px in both
    themes nothing scrolls sideways and every row fits."""
    page = _page(pages, width=390, live=(_out_doc(data), _tasks(_out_tasks())))
    page.goto(site + PAGE)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t98]")
    expect(box).to_be_visible()

    n03 = box.locator("li[data-current-leaf=N03]")
    expect(n03.locator(".state")).to_have_text("Running: outline 2, hole 1")
    assert [o.get_attribute("data-outline") for o in n03.locator(".p2m-outline").all()] == ["2", "1"], "newest outline first"
    o2, o1 = _outline(n03, 2), _outline(n03, 1)
    expect(o2).to_have_attribute("data-outline-state", "in_progress")
    expect(o2.locator("[data-outline-text]")).to_have_text("in progress")
    expect(o2.locator("[data-outline-cost]")).to_have_text("written in 28 steps · 12.8 min")
    assert _holes(o2) == [("Hole 1", "working", "Working (try 1, 14 steps so far)", ""),
                          ("Hole 2", "solved", "Solved", "solved automatically"),
                          ("Hole 3", "working", "Working (queued)", ""), ("Hole 4", "working", "Working (queued)", ""),
                          ("In place", "working", "Working (queued)", "")], _holes(o2)
    expect(o1).to_have_attribute("data-outline-state", "refuted")
    expect(o1.locator("[data-outline-text]")).to_have_text("refuted: hole 2 is false")
    expect(o1.locator("[data-outline-cost]")).to_have_text("written in 59 steps · 27.0 min")
    assert _holes(o1) == [("Hole 1", "waiting", "Waiting", "in place, cancelled (outline refuted); a new outline is being made"),
                          ("Hole 2", "waiting", "Waiting", f"false, disproved by the model; {REJUDGED}; a new outline is being made")], _holes(o1)
    assert "check_error" not in n03.inner_text()
    # the task table stays, collapsed, with every task
    table = n03.locator("details[data-leaf-tasks=N03]")
    expect(table).not_to_have_attribute("open", "")
    expect(table.locator("summary")).to_have_text("Every task (9)")

    c07 = box.locator("li[data-current-leaf=C07]")
    expect(c07.locator(".state")).to_have_text("Proved: outline 1 assembled")
    o = _outline(c07, 1)
    expect(o).to_have_attribute("data-outline-state", "proved")
    expect(o.locator("[data-outline-text]")).to_have_text("proved (assembled)")
    assert _holes(o) == [("Hole 1", "solved", "Solved", "already known"), ("Hole 2", "solved", "Solved", "own task"),
                         ("Hole 3", "solved", "Solved", "solved automatically"),
                         ("Assembly", "proved", "proved: our full check passed", "")], _holes(o)

    n05 = box.locator("li[data-current-leaf=N05]")
    expect(n05.locator(".state")).to_have_text("Closed by reuse: imports NestedSeatAlloc.IntPolicy.eq27_er1_clbi")
    expect(_outline(n05, 2).locator("[data-outline-text]")).to_have_text("abandoned: cancelled before it started")
    expect(_outline(n05, 1).locator("[data-outline-text]")).to_have_text("proved without holes")

    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    for sel in ("[data-p2m-now] .p2m-ohead", "[data-p2m-now] .p2m-holes li"):
        for el in page.locator(sel).all():
            b = el.bounding_box()
            assert b and b["x"] >= 0 and b["x"] + b["width"] <= 390, f"{sel} on a phone: {b}"


def test_without_the_documents_holes_the_outlines_come_from_the_tasks(pages, site, data):
    """A document of schema /1 (no holes, no reuse details): the outlines still come from the tasks, a hole number
    without a task of its own waits, "not known yet" in its tooltip (nothing is guessed), the false hole is not claimed (the task list only
    knows our check's failure), and the reuse leaf's imported theorem comes from the build when it knows the leaf, else
    the line says a proved theorem."""
    page = _page(pages, live=(_out_doc(data, schema=1), _tasks(_out_tasks())))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t98]")
    expect(box).to_be_visible()
    n03 = box.locator("li[data-current-leaf=N03]")
    expect(n03.locator(".state")).to_have_text("Running: outline 2, hole 1")
    o1 = _outline(n03, 1)
    expect(o1).to_have_attribute("data-outline-state", "abandoned")
    expect(o1.locator("li[data-hole='2'] .p2m-htext")).to_have_text("Waiting")
    expect(o1.locator("li[data-hole='2']")).to_have_attribute(
        "title", "try 1: our check failed; try 2 not tried (outline abandoned); a new outline is being made")
    expect(_outline(n03, 2).locator("li[data-hole='2'] .p2m-htext")).to_have_text("Waiting")
    expect(_outline(n03, 2).locator("li[data-hole='2']")).to_have_attribute("title", "not known yet")
    reused = next((s["reuses"]["theorem"] for s in data["submissions"] if s["theorem"] == OUT_LEAVES[2]["theorem"] and s.get("reuses")),
                  "a theorem already proved on prove2me")
    expect(box.locator("li[data-current-leaf=N05] .state")).to_have_text(f"Closed by reuse: imports {reused}")


def test_every_hole_waits_works_or_is_solved_the_detail_in_a_tooltip(pages, site, data):
    """From 2026-11-25 every task of an outline carries the label `hole_states` (set by the prove2me runner): every
    hole shows, the ones after the last hole task too, each Waiting, Working or Solved; why it waits and how it was
    solved only in its tooltip (trial 11's p2m-dabfa9: holes held back behind the riskiest one had been shown as
    "closed before the hole tasks, or left in place", hole 4 not at all)."""
    states = "1=task 2=prepass:simp 3=task 4=task 5=inplace 6=wait:1"
    tasks = [{**x, "labels": {**x["labels"], "hole_states": states}} if x["id"].startswith("t98-p2m-dbd69f-s2-") else x
             for x in _out_tasks()]
    page = _page(pages, width=390, live=(_out_doc(data, schema=1), _tasks(tasks)))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t98]")
    expect(box).to_be_visible()
    o2 = _outline(box.locator("li[data-current-leaf=N03]"), 2)
    expect(o2).to_have_attribute("data-outline-state", "in_progress")
    expect(o2.locator("li[data-hole='6']")).to_be_visible()
    assert _holes(o2) == [("Hole 1", "working", "Working (try 1, 14 steps so far)", ""),
                          ("Hole 2", "solved", "Solved", "solved automatically: simp"),
                          ("Hole 3", "working", "Working (queued)", ""), ("Hole 4", "working", "Working (queued)", ""),
                          ("Hole 5", "working", "Working (queued)", "in place"),
                          ("Hole 6", "waiting", "Waiting", "starts when hole 1 is done")], _holes(o2)
    assert "starts when" not in o2.inner_text(), "the reason is in the tooltip only"
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"


def _n03_finished(asm_state="done", asm_outcome="proved"):
    """N03 as trial 6 ended it: outline 2's holes 1, 3 and 4 proved as lemmas, hole 2 left in place and filled in by
    the in-place task (`rest_done`: that task never says proved), then the assembly; the document lists hole 2 as in
    place."""
    doc_holes = [{"hole": 1, "kind": "lemma", "state": "open"}, {"hole": 2, "kind": "inplace", "state": "open"},
                 {"hole": 3, "kind": "lemma", "state": "open"}, {"hole": 4, "kind": "lemma", "state": "open"}]
    t = lambda id_, role, state, **kw: _ptask(f"t98-p2m-dbd69f-{id_}", "t98", "N03", "p2m-dbd69f", role, state, "2", **kw)
    s2 = "t98-p2m-dbd69f-s2"
    keep = [x for x in _out_tasks() if not x["id"].startswith(s2)]
    done = dict(started_at="2026-11-15T12:02:53Z", finished_at="2026-11-15T13:00:00Z", steps=20, model_min=5.0)
    tasks = keep + [
        t("s2", "outline", "done", outcome="outline", replaces="t98-p2m-dbd69f-s1", steps=28, model_min=12.8,
          started_at="2026-11-15T11:26:26Z", finished_at="2026-11-15T12:02:35Z"),
        *[t(f"s2-h0{h}", "hole", "done", hole=h, parent=s2, outcome="proved", **done) for h in (1, 3, 4)],
        t("s2-rest", "rest", "done", parent=s2, outcome="rest_done", **{**done, "steps": 118, "model_min": 62.2}),
        t("s2-asm", "assembly", asm_state, parent=s2, outcome=asm_outcome if asm_state == "done" else None,
          needs=[f"{s2}-h01", f"{s2}-h03", f"{s2}-h04", f"{s2}-rest"]),
    ]
    return doc_holes, tasks


@pytest.mark.parametrize("asm_state,asm_outcome,outline_state,want", (
    ("done", "proved", "proved", ("solved", "Solved", "in place, with the outline")),
    ("queued", None, "in_progress", ("working", "Working (the assembly checks it)", "in place")),
    ("done", "compile_error", "ended", ("waiting", "Waiting", "in place, filled in, but the assembly did not check")),
))
def test_a_hole_in_place_is_decided_by_the_assembly(pages, site, data, asm_state, asm_outcome, outline_state, want):
    """Trial 6's N03: the in-place task ends `rest_done` (it fills the holes in, the assembly's full check decides), so
    its hole is proved when the assembly is, waits for the assembly before, and is not proved when the assembly fails;
    never "not proved: rest done" on a proved leaf."""
    doc_holes, tasks = _n03_finished(asm_state, asm_outcome)
    doc = _out_doc(data)
    doc["leaves"][0]["outlines"][1]["holes"] = doc_holes
    page = _page(pages, live=(doc, _tasks(tasks)))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t98]")
    expect(box).to_be_visible()
    o2 = _outline(box.locator("li[data-current-leaf=N03]"), 2)
    expect(o2).to_have_attribute("data-outline-state", outline_state)
    holes = _holes(o2)
    assert ("Hole 2", *want) in holes, holes
    assert all("rest done" not in " ".join(h) for h in holes), holes


def test_a_hole_in_place_keeps_the_documents_verdict(pages, site, data):
    """A verdict the data document holds for a hole in place (false, found outside a task, no in-place task yet) is
    shown, not "not tried"."""
    doc_holes, tasks = _n03_finished()
    doc_holes[1] = {"hole": 2, "kind": "inplace", "state": "false", "by": "check", "note": "re-judged"}
    tasks = [x for x in tasks if not x["id"].endswith(("-s2-rest", "-s2-asm"))]
    doc = _out_doc(data)
    doc["leaves"][0]["outlines"][1]["holes"] = doc_holes
    page = _page(pages, live=(doc, _tasks(tasks)))
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now][data-source=live][data-trial=t98]")
    expect(box).to_be_visible()
    holes = _holes(_outline(box.locator("li[data-current-leaf=N03]"), 2))
    assert ("Hole 2", "waiting", "Waiting", "in place, false, disproved; re-judged") in holes, holes


@pytest.mark.parametrize("broken", ["document 500", "tasks 500", "document not one", "tasks not a list", "document 404"])
def test_working_on_now_keeps_the_build_copy_when_a_source_fails(pages, site, data, broken):
    """When the data document or the task list fails or is not what it should be (and there are no campaigns), the
    build's copy stays as it was."""
    doc, tasks = _doc(data), _tasks([])
    live = {"document 500": (500, tasks), "tasks 500": (doc, 500), "document not one": ({**doc, "schema": "other/1"}, tasks),
            "tasks not a list": (doc, {"updated": "2026-11-15T10:21:00Z", "tasks": {}}), "document 404": (404, tasks)}[broken]
    page = _page(pages, live=live)
    page.goto(site + PAGE)
    box = page.locator("[data-p2m-now]")
    built = box.locator("[data-now-head]").inner_text()
    page.wait_for_timeout(1500)       # every fetch has answered
    expect(box).to_have_attribute("data-source", "build")
    expect(box.locator("[data-now-head]")).to_have_text(built)
    cur = data["current"]
    if cur and cur.get("state") != "preparing":
        expect(box.locator("li[data-current-leaf]")).to_have_count(len(cur["leaves"]))
        expect(box.locator("[data-now-asof]")).to_contain_text("copy from the site’s last build")


@pytest.mark.parametrize("width", (1280, 390))
def test_working_on_now_shows_every_trial_not_finished(pages, site, records, width):
    """Bug: trial 10 was posted while trial 9 still ran, and "Working on now" showed trial 10 only.
    Every trial campaign not finished has its own block, oldest first, each with its head, its own text and budget and
    its leaves with their tasks; a leaf both trials list keeps its #leaf- anchor on its first row only. When trial 8
    ends, the next minute's refresh shows trial 9 alone. At 390 px nothing scrolls sideways."""
    rec = _two_open(records)
    t9 = rec["campaigns"]["campaigns"][0]
    t9["text"] = "Trial nine's own text."
    t9["labels"]["budget"] = "one round of 120 steps / 120 min"
    page = _page(pages, width=width, live=(404, rec["tasks"]), records={"campaigns": rec["campaigns"], "results": rec["results"]})
    page.clock.install()     # time runs as usual; the end moves it on to the page's minute refresh
    page.goto(site + PAGE)
    now = page.locator("[data-p2m-now][data-source=live][data-trial='t8 t9']")
    expect(now).to_be_visible()
    blocks = now.locator("[data-now-trial]")
    expect(blocks).to_have_count(2)
    assert [b.get_attribute("data-now-trial") for b in blocks.all()] == ["t8", "t9"]
    b8, b9 = now.locator("[data-now-trial=t8]"), now.locator("[data-now-trial=t9]")
    expect(b8.locator("[data-now-head]")).to_have_text("Trial 8, since 16 November 2026 · scaffold v9")
    expect(b9.locator("[data-now-head]")).to_have_text("Trial 9, since 18 November 2026 · scaffold v10")
    assert "Trial nine's own text." not in now.inner_text()
    expect(b8.locator("li[data-current-leaf]")).to_have_count(6)
    expect(b9.locator("li[data-current-leaf]")).to_have_count(6)
    leaf = b8.locator("li[data-current-leaf]").first.get_attribute("data-leaf-id")
    expect(page.locator(f"#leaf-{leaf}")).to_have_count(1)
    expect(b8.locator(f"li#leaf-{leaf}")).to_have_count(1)
    expect(now.locator("[data-now-asof]")).to_have_text("Live from the gateway’s records of the trials and their tasks, updated 14:58 UTC. It refreshes every minute.")
    assert page.evaluate("document.documentElement.scrollWidth") <= width, "the page scrolls sideways"
    # trial 8 ends: the next refresh shows trial 9 alone
    for c in rec["campaigns"]["campaigns"]:
        if c["id"] == "t8":
            c["status"] = "finished"
    page.unroute("**/v1/projects/prove2me/campaigns?*")
    _serve(page, "**/v1/projects/prove2me/campaigns?*", rec["campaigns"])
    page.clock.run_for(60_000)
    one = page.locator("[data-p2m-now][data-source=live][data-trial=t9]")
    expect(one.locator("[data-now-trial]")).to_have_count(1, timeout=15_000)
    expect(one.locator("[data-now-head]")).to_have_text("Trial 9, since 18 November 2026 · scaffold v10")
    expect(page.locator(f"[data-now-trial=t9] li#leaf-{leaf}")).to_have_count(1)


# ---- the gateway's records: numbers, submissions, trials

def _records_page(pages, records, doc=404, campaigns=None, results=None, width=None, path=PAGE, site=""):
    page = _page(pages, width=width, live=(doc, records["tasks"]),
                 records={"campaigns": records["campaigns"] if campaigns is None else campaigns,
                          "results": records["results"] if results is None else results})
    page.goto(site + path)
    return page


def test_the_numbers_submissions_and_trials_come_live_from_the_records(pages, site, data, records):
    """With the gateway's campaigns, results and tasks (as on 2026-11-18): the headline numbers count trials 1 to 5 from
    the build and trial 6 on from the records (8 of 8 accepted, 6 of 26 open leaves, 17 of 54 runs); the submissions
    list gains trial 8's; the trials table's rows from trial 6 on are live, trial 8 running, tokens computed leading,
    the All row counting them; each trial's list from trial 6 on has its generated facts, the campaign's own text as
    plain text and every leaf; "Working on now" is the newest trial campaign (no data document)."""
    page = _records_page(pages, records, site=site)
    kpis = page.locator("[data-p2m-kpis][data-source=live]")
    expect(kpis).to_be_visible()
    expect(kpis.locator("[data-kpi=accepted] dd")).to_have_text("8")
    expect(kpis.locator("[data-kpi=accepted] dt")).to_have_text("proofs accepted by prove2me, of 8 submitted")
    expect(kpis.locator("[data-kpi=open] dd")).to_have_text("6")
    expect(kpis.locator("[data-kpi=open] dt")).to_have_text("open leaves proved, of 26 tried")
    expect(kpis.locator("[data-kpi=reused] dd")).to_have_text("2")
    expect(kpis.locator("[data-kpi=runs] dt")).to_have_text("runs proved, of 54 in trials 1 to 8")
    expect(kpis.locator("[data-kpi=runs] dd")).to_have_text("17")
    expect(page.locator("[data-p2m-small]")).to_contain_text("Small numbers: 6 open leaves proved so far.")
    expect(page.locator("[data-p2m-updated]")).to_have_text(re.compile(r"^Live from the gateway’s records, updated 18 November 2026, 14:58 UTC$"))

    subs = page.locator("[data-p2m-submissions][data-source=live] tbody tr")
    expect(subs).to_have_count(8)
    # newest trial first
    assert [s.get_attribute("data-submission") for s in subs.all()] == ["N12", "N07", "N10", "N03", "N05", "N02", "F01", "O04"]
    n12 = page.locator("[data-p2m-submissions] tr[data-submission=N12]")
    expect(n12.locator("th")).to_contain_text("p2m-527634")
    expect(n12.get_by_role("link", name="DistLTLSynth.Char.lemma_2_3")).to_have_attribute(
        "href", "https://prove2.me/theorems/527634e2-60a9-4cb8-8ef0-24b370af85f5")
    expect(n12.locator("td[data-label=From]")).to_have_text("Trial 8")
    expect(n12.locator("td[data-label=Verdict]")).to_have_text("Accepted07:22 UTC")

    table = page.locator("[data-p2m-trials][data-source=live]")
    rows = table.locator("tbody tr")
    assert [r.get_attribute("data-trial") for r in rows.all()] == ["1", "2", "3", "4", "5", "6p", "6", "7p", "7", "8"]
    assert [r.get_attribute("data-source") for r in rows.all()][4:] == ["build", "live", "live", "live", "live", "live"]
    t7 = table.locator("tr[data-trial='7']")
    expect(t7.locator("[data-cell=proved]")).to_have_text("3 of 4")
    expect(t7.locator("td[data-label='Tokens computed']")).to_have_attribute("data-tokens", str(201793 + 138170 + 270465 + 5776280))
    expect(t7.locator("td[data-label='Tokens computed']")).to_have_text("6.4M")
    t8 = table.locator("tr[data-trial='8']")
    expect(t8).to_have_attribute("data-status", "running")
    expect(t8.locator("th")).to_have_text("Trial 8running")
    expect(t8.locator("[data-cell=proved]")).to_have_text("2 of 6")
    expect(table.locator("tfoot [data-cell=proved]")).to_have_text("17 of 54")
    expect(page.locator("[data-p2m-trials-live]")).to_have_text("Trials 1 to 5 as recorded; from trial 6 on live from the gateway’s records, updated 14:58 UTC.")
    assert "Qwen tokens" not in page.locator("[data-p2m-trials]").inner_text()

    lists = page.locator("[data-p2m-trial-lists] > details[data-p2m-trial]")
    assert [d.get_attribute("data-p2m-trial") for d in lists.all()] == ["1", "2", "3", "4", "5", "6", "7", "8"]
    d7 = page.locator("details#trial-7[data-source=live]")
    t7.get_by_role("link", name="Trial 7").click()
    expect(d7).to_have_attribute("open", "")
    expect(d7.locator("[data-trial-facts]")).to_have_text(
        "Trial 7 ran from 15 to 17 November 2026 on scaffold v8: 4 leaves (1 calibration, 3 open), 3 of 4 runs proved; prove2me accepted 2 proofs from it.")
    expect(d7.locator("[data-trial-text] p").first).to_contain_text("Scaffold v8: v7 with what trial 6 taught.")
    expect(d7.locator("tr[data-leaf=N10]")).to_have_attribute("data-outcome", "proved")
    d8 = page.locator("details#trial-8[data-source=live]")
    expect(d8.locator(":scope > summary")).to_have_text("Trial 8 · since 16 November 2026 · 2 of 6 proved")
    # the wording fix (2026-11-18): trial 8's p2m-03d4b4, outline 1's second tries cancelled when the outline was abandoned
    n13 = d8.locator("[data-trial-leaf=N13] .p2m-outline[data-outline='1']")
    expect(n13.locator("li[data-hole='2'] .p2m-htext")).to_have_text("Waiting")
    expect(n13.locator("li[data-hole='2']")).to_have_attribute(
        "title", re.compile(r"^try 1: a sorry left; try 2 cancelled: outline abandoned after hole 1 failed"))
    assert "closed by another task" not in d8.inner_text()

    now = page.locator("[data-p2m-now][data-source=live][data-trial=t8]")
    expect(now.locator("[data-now-head]")).to_have_text("Trial 8, since 16 November 2026 · scaffold v9")
    expect(now.locator("li[data-current-leaf]")).to_have_count(6)
    expect(now.locator("[data-now-asof]")).to_have_text("Live from the gateway’s records of the trial and its tasks, updated 14:58 UTC. It refreshes every minute.")
    # every task: trial 8's cancelled try says why
    page.locator("[data-p2m-alltasks] > summary").click()
    row = page.locator("[data-p2m-alltasks] tr[data-task-id='t8-p2m-03d4b4-s1-h02-a2']")
    expect(row.locator("[data-cell=result]")).to_have_text("Cancelled: outline abandoned after hole 1 failed")


def test_a_campaigns_text_is_plain_text(pages, site, records):
    """The campaign's closing text (the project's words) is plain text: paragraphs, nothing as markup or a link."""
    camps = json.loads(json.dumps(records["campaigns"]))
    for c in camps["campaigns"]:
        if c["id"] == "t7":
            c["text"] = "First paragraph.\n\n<img src=x onerror=alert(1)> and [a link](https://example.org) *not em* `not code`"
    page = _records_page(pages, records, campaigns=camps, site=site)
    text = page.locator("details#trial-7[data-source=live] [data-trial-text]")
    expect(text.locator("p")).to_have_count(2)
    expect(text.locator("p").nth(1)).to_have_text("<img src=x onerror=alert(1)> and [a link](https://example.org) *not em* `not code`")
    expect(text.locator("img, a, em, code")).to_have_count(0)


@pytest.mark.parametrize("broken", ["campaigns", "results", "campaigns not a list"])
def test_the_numbers_stay_the_builds_when_the_records_fail(pages, site, data, records, broken):
    """When the campaigns or the results fail (the tasks answer), the headline numbers, the submissions, the trials
    table and every leaf stay the build's (the records at the build: 8 of 8 accepted, 17 runs proved); the task table
    still takes the tasks."""
    kw = {"campaigns": 500} if broken == "campaigns" else {"results": 500} if broken == "results" else {"campaigns": {"campaigns": {}}}
    page = _records_page(pages, records, site=site, **kw)
    page.wait_for_timeout(1500)
    kpis = page.locator("[data-p2m-kpis]")
    expect(kpis).to_have_attribute("data-source", "build")
    expect(kpis.locator("[data-kpi=accepted] dd")).to_have_text("8")
    expect(kpis.locator("[data-kpi=runs] dd")).to_have_text("17")
    expect(page.locator("[data-p2m-trials]")).to_have_attribute("data-source", "build")
    expect(page.locator("[data-p2m-scorecard]")).to_have_attribute("data-source", "build")
    expect(page.locator("[data-p2m-submissions] tbody tr")).to_have_count(len(records["results"]["results"]))
    expect(page.locator("[data-p2m-trials-live]")).to_contain_text("at the site’s last build, 18 November 2026, 14:58 UTC")
    page.locator("[data-p2m-alltasks] > summary").click()
    expect(page.locator("[data-p2m-alltasks] tr[data-task-id='t8-p2m-03d4b4-s1-h02-a2']")).to_have_attribute("data-source", "live")


def test_without_javascript_the_page_is_the_builds(pages, site, data, records):
    """No JavaScript: the numbers, submissions, trials, leaves and "Working on now" as the records were at the build
    (trials 1 to 5 the frozen data's), with tokens computed in the tables; nothing to push by hand."""
    page = pages(js=False)
    page.goto(site + PAGE)
    expect(page.locator("[data-p2m-kpis] [data-kpi=accepted] dd")).to_have_text("8")
    expect(page.locator("[data-p2m-kpis] [data-kpi=open] dt")).to_have_text("open leaves proved, of 26 tried")
    rows = page.locator("[data-p2m-trials] tbody tr")
    assert [r.get_attribute("data-trial") for r in rows.all()] == ["1", "2", "3", "4", "5", "6p", "6", "7p", "7", "8"]
    expect(page.locator("[data-p2m-trials] thead")).to_contain_text("Tokens computed")
    for n in range(1, 9):
        expect(page.locator(f"details#trial-{n}")).to_have_count(1)
    expect(page.locator("[data-p2m-submissions] tbody tr")).to_have_count(len(records["results"]["results"]))
    assert [s.get_attribute("data-submission") for s in page.locator("[data-p2m-submissions] tbody tr").all()][:2] == ["N12", "N07"]
    now = page.locator("[data-p2m-now][data-source=build][data-trial=t8]")
    expect(now.locator("[data-now-head]")).to_have_text("Trial 8, since 16 November 2026 · scaffold v9")
    expect(now.locator("li[data-current-leaf]")).to_have_count(6)
    expect(page.locator("[data-p2m-leaf-table] tr[data-leaf-id=p2m-527634]")).to_have_attribute("data-outcome", "accepted")


def test_home_and_science_overview_read_the_records(pages, site, records):
    """The home page's prove2me line and the /science/ card: the records' numbers (8 accepted, 6 of 26 open leaves,
    trial 8 running); the build's when the gateway fails."""
    page = _records_page(pages, records, path="/", site=site)
    line = page.locator("[data-home-prove2me][data-source=live]")
    expect(line.locator("[data-home-p2m-accepted]")).to_have_text("8")
    expect(line.locator("[data-home-p2m-text]")).to_have_text(
        "proofs by Qwen3.8 27B accepted on prove2me so far: it proved 6 of the 26 open lemmas it tried and closed 2 more by "
        "reusing a proof already there. prove2me states theorems from research papers in Lean 4 and checks every proof by machine. "
        "Trial 8 is running.")
    page = _records_page(pages, records, path="/science/", site=site)
    card = page.locator("[data-project=prove2me][data-source=live]")
    expect(card.locator("[data-number=accepted] dd")).to_have_text("8")
    expect(card.locator("[data-number=open] dd")).to_have_text("6 of 26")
    expect(card.locator("[data-number=trial] dt")).to_have_text("running since 16 November 2026")
    page = _page(pages)
    page.goto(site + "/")
    page.wait_for_timeout(1500)
    expect(page.locator("[data-home-prove2me]")).to_have_attribute("data-source", "build")


def test_home_chart_of_accepted_proofs(pages, site, records):
    """The home page's science section comes before the live strip and the node block, and carries a chart of the
    proofs prove2me accepted: one point per accepted proof (8 with the records of 2026-11-18), its total at the end, a
    text alternative, and the same proofs in its table, newest first. Without JavaScript the build's chart is there."""
    page = _records_page(pages, records, path="/", site=site)
    expect(page.locator("[data-home-prove2me][data-source=live]")).to_be_attached()
    fig = page.locator("[data-home-p2m-chart]")
    expect(fig).to_have_attribute("data-source", "live")
    svg = fig.locator("svg[role=img]")
    assert svg.get_attribute("aria-label").startswith("8 proofs accepted by prove2me from ")
    expect(svg.locator("circle")).to_have_count(8)
    expect(svg.locator("text.end-n")).to_have_text("8")
    fig.locator("summary").click()
    rows = fig.locator("tbody tr")
    expect(rows).to_have_count(8)
    expect(rows.first.locator("td").last).to_have_text("8")
    order = page.evaluate("""() => [...document.querySelectorAll('[data-home-science], [data-live-stats], [data-home-node], [data-home-devlog]')]
        .map((e) => e.matches('[data-home-science]') ? 'science' : e.matches('[data-live-stats]') ? 'live' : e.matches('[data-home-node]') ? 'node' : 'devlog')""")
    assert order == ["science", "live", "node", "devlog"], order
    page.set_viewport_size({"width": 360, "height": 800})
    page.wait_for_timeout(200)
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth")
    box = svg.bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 360
    still = pages(False)
    still.goto(site + "/")
    expect(still.locator("[data-home-p2m-chart] svg[role=img] circle").first).to_be_attached()


# ---- the status block at the top: the whole queue as one bar in parts per trial, a line per open trial, the numbers

STATS = {"updated": "2026-11-18T14:58:00Z", "nodes": {"online": 2, "busy": 1, "slots": 2, "held": 0, "paused": 0},
         "today": {"requests": 10, "projects": {"prove2me": {"requests": 4}}},
         "total": {"requests": 100, "projects": {"prove2me": {"requests": 40, "prompt_tokens": 1000, "cached_tokens": 100,
                                                             "completion_tokens": 50, "node_seconds": 3600}}}}


def _two_open(records):
    """The records with trial 9 started beside trial 8 (as on 2026-11-19): 2 of its tasks done, 1 running, 4 waiting."""
    rec = json.loads(json.dumps(records))
    t8 = next(c for c in rec["campaigns"]["campaigns"] if c["id"] == "t8")
    rec["campaigns"]["campaigns"].insert(0, dict(t8, id="t9", title="Trial 9", started_at="2026-11-18T20:46:49Z",
                                                  labels={"kind": "trial", "scaffold": "v10"}))
    base = next(t for t in rec["tasks"]["tasks"] if t["labels"].get("trial") == "t8")
    for i, (state, outcome) in enumerate([("done", "outline"), ("done", "refused"), ("leased", None)] + [("queued", None)] * 4):
        rec["tasks"]["tasks"].append(dict(base, id=f"t9-n{i}", state=state, outcome=outcome, labels=dict(base["labels"], trial="t9")))
    return rec


def _status_page(pages, site, records, width=None):
    page = _page(pages, width=width, live=(404, records["tasks"]), records={"campaigns": records["campaigns"], "results": records["results"]})
    _serve(page, "**/v1/stats", STATS)
    page.clock.set_fixed_time("2026-11-18T15:00:00Z")  # the fixtures' time: their data is fresh
    page.goto(site + PAGE)
    return page


@pytest.mark.parametrize("width,theme", ((1280, "dark"), (390, "light"), (390, "dark")))
def test_the_status_block_shows_the_whole_queue(pages, site, records, width, theme):
    """Trials 8 and 9 open: the head names both with their scaffolds; the bar is the whole queue, a part per trial named
    under it (Trial 8 · 11, Trial 9 · 5), each part's running and waiting tasks; the sentence above says it in words; a
    line per trial with every state and its leaves; 2 nodes online, 1 of 2 slots busy, 8 of 8 proofs accepted. No
    percentage; at 390 px nothing scrolls sideways."""
    page = _status_page(pages, site, _two_open(records), width)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    box = page.locator("[data-p2m-status][data-source=live][data-trials='t8 t9']")
    expect(box).to_be_visible()
    expect(box.locator("[data-st=head]")).to_have_text("Trials 8 (scaffold v9) and 9 (scaffold v10) are running.")
    expect(box.locator("[data-st=queue]")).to_have_text(
        "16 tasks in the queue: 3 running, 13 waiting (Trial 8: 2 running, 9 waiting; Trial 9: 1 running, 4 waiting).")
    q = box.locator("[data-st-queue]")
    expect(q).to_be_visible()
    expect(q).to_have_attribute("aria-hidden", "true")
    expect(q.locator("[data-st-bar] > .grp")).to_have_count(2)
    assert [" ".join(x.text_content().split()) for x in q.locator("[data-st-qlabels] > span").all()] == ["Trial 8 · 11", "Trial 9 · 5"]
    w8 = q.locator(".grp[data-trial=t8]").bounding_box()["width"]
    w9 = q.locator(".grp[data-trial=t9]").bounding_box()["width"]
    assert w8 > w9, (w8, w9)
    expect(q.locator(".grp[data-trial=t9] [data-part]")).to_have_count(2)
    lines = box.locator("[data-st-trials] li")
    expect(lines).to_have_count(2)
    expect(lines.nth(0)).to_have_text("Trial 8, 46 tasks: 14 proved, 18 ended otherwise, 3 stopped, 2 running, 9 waiting; 2 of 6 leaves proved.")
    expect(lines.nth(1)).to_have_text("Trial 9, 7 tasks: 2 ended otherwise, 1 running, 4 waiting; 0 of 6 leaves proved.")
    expect(box.locator("[data-num=nodes] dd")).to_have_text("2")
    expect(box.locator("[data-num=slots] dd")).to_have_text("1 of 2")
    expect(box.locator("[data-num=accepted] dd")).to_have_text("8 of 8")
    expect(box.locator("[data-st=updated]")).to_contain_text("updated 14:58 UTC")
    assert "%" not in box.inner_text()
    assert page.evaluate("document.documentElement.scrollWidth") <= width, "the page scrolls sideways"
    b = box.bounding_box()
    assert b["x"] >= 0 and b["x"] + b["width"] <= width, b


def test_the_status_block_with_one_trial_and_with_none(pages, site, records):
    """One trial open (the fixtures): one part, one line. Then nothing open (trial 8 ended, its tasks closed): no bar,
    "Nothing in the queue now.", the last trial that ended, no lines."""
    page = _status_page(pages, site, records)
    box = page.locator("[data-p2m-status][data-source=live][data-trials=t8]")
    expect(box.locator("[data-st=head]")).to_have_text("Trial 8 is running, scaffold v9, since 16 Nov.")
    expect(box.locator("[data-st=queue]")).to_have_text("11 tasks in the queue: 2 running, 9 waiting.")
    expect(box.locator("[data-st-bar] > .grp")).to_have_count(1)
    expect(box.locator("[data-st-trials] li")).to_have_count(1)
    rec = json.loads(json.dumps(records))
    for c in rec["campaigns"]["campaigns"]:
        if c["id"] == "t8":
            c.update(status="finished", ended_at="2026-11-18T12:00:00Z")
    for t in rec["tasks"]["tasks"]:
        if t["state"] in ("queued", "leased"):
            t["state"] = "cancelled"
    page = _status_page(pages, site, rec)
    box = page.locator("[data-p2m-status][data-source=live][data-trials='']")
    expect(box.locator("[data-st=head]")).to_have_text("No trial is running. Trial 8 ended on 18 Nov.")
    expect(box.locator("[data-st=queue]")).to_have_text("Nothing in the queue now.")
    expect(box.locator("[data-st-queue]")).to_be_hidden()
    expect(box.locator("[data-st-trials]")).to_be_hidden()


def test_the_status_block_keeps_the_builds_copy_when_the_gateway_fails(pages, site, data):
    """The gateway down (every /v1 request fails), and without JavaScript: the build's copy, as the records were at the
    build: the trial, the queue and its bar, the trial's line and the proofs; no nodes; and a line that says when."""
    for js in (True, False):
        page = _page(pages, js=js)
        page.goto(site + PAGE)
        page.wait_for_timeout(1500 if js else 0)
        box = page.locator("[data-p2m-status]")
        expect(box).to_have_attribute("data-source", "build")
        expect(box.locator("[data-st=head]")).to_have_text("Trial 8 is running, scaffold v9, since 16 Nov.")
        expect(box.locator("[data-st=queue]")).to_have_text("11 tasks in the queue: 2 running, 9 waiting.")
        expect(box.locator("[data-st-bar] > .grp[data-trial=t8]")).to_have_count(1)
        expect(box.locator("[data-st-trials] li")).to_have_count(1)
        expect(box.locator("[data-num=nodes]")).to_be_hidden()
        expect(box.locator("[data-num=accepted] dd")).to_have_text("8 of 8")
        expect(box.locator("[data-st=updated]")).to_have_text(
            "As of the gateway’s records at the site’s last build, 18 Nov, 14:58 UTC. The page refreshes it every minute when it can reach the gateway.")


# ---- a bench (2026-11-23: bench-v13 ran beside trial 10 and the page showed "0 running"; a bench is shown as
# a trial is)

def _with_bench(records):
    """The records with a bench as the gateway served bench-v13 on 2026-11-23 (label trial=bench-v13, scaffold v13, no
    campaign): on two of trial 8's leaves, 1 task done, 2 running and 1 waiting."""
    rec = json.loads(json.dumps(records))
    leaves = {"p2m-754bbc": "N11", "p2m-580c57": "N14"}
    for i, (leaf, state) in enumerate([("p2m-754bbc", "done"), ("p2m-754bbc", "leased"), ("p2m-580c57", "leased"), ("p2m-580c57", "queued")], 1):
        rec["tasks"]["tasks"].append(_ptask(f"bench-v13-{leaf}-s1-h0{i}", "bench-v13", leaves[leaf], leaf, "hole", state, hole=i,
                                            outcome="proved" if state == "done" else None, created_at=f"2026-11-18T14:0{i}:00Z",
                                            started_at=None if state == "queued" else "2026-11-18T14:30:00Z")
                                     | {"labels": {"trial": "bench-v13", "leaf": leaf, "alias": leaves[leaf], "group": "open", "scaffold": "v13",
                                                   "outline": "1", "hole": str(i)}})
    return rec


def test_a_bench_is_shown_as_a_trial(pages, site, records):
    """Every open task counts in the queue: the bench is a part of the bar and has a line, as a trial does, named after
    its label; "Working on now" has its block after the trial's, its leaves and their tasks. The bench counts in no
    number, trial or task row of the results."""
    page = _status_page(pages, site, _with_bench(records))
    box = page.locator("[data-p2m-status][data-source=live][data-trials='t8 bench-v13']")
    expect(box).to_be_visible()
    expect(box.locator("[data-st=head]")).to_have_text("Trial 8 (scaffold v9) and bench v13 (scaffold v13) are running.")
    expect(box.locator("[data-st=queue]")).to_have_text(
        "14 tasks in the queue: 4 running, 10 waiting (Trial 8: 2 running, 9 waiting; Bench v13: 2 running, 1 waiting).")
    q = box.locator("[data-st-queue]")
    expect(q.locator("[data-st-bar] > .grp[data-trial=bench-v13] [data-part]")).to_have_count(2)
    assert [" ".join(x.text_content().split()) for x in q.locator("[data-st-qlabels] > span").all()] == ["Trial 8 · 11", "Bench v13 · 3"]
    lines = box.locator("[data-st-trials] li")
    expect(lines).to_have_count(2)
    expect(lines.nth(1)).to_have_text("Bench v13, 4 tasks: 1 proved, 2 running, 1 waiting.")
    expect(box.locator("[data-num=accepted] dd")).to_have_text("8 of 8")

    now = page.locator("[data-p2m-now][data-source=live][data-trial='t8 bench-v13']")
    expect(now).to_be_visible()
    bench = now.locator("[data-now-trial=bench-v13]")
    expect(bench.locator("[data-now-head]")).to_have_text("Bench v13, since 18 November 2026 · scaffold v13")
    expect(bench.locator("li[data-current-leaf]")).to_have_count(2)
    expect(bench.locator("li[data-current-leaf=N11]")).to_have_attribute("data-state", "running")
    expect(now.locator("[data-now-trial=t8] li#leaf-p2m-754bbc")).to_have_count(1)
    expect(page.locator("[data-p2m-kpis][data-source=live] [data-kpi=runs] dd")).to_have_text("17")
    expect(page.locator("[data-p2m-trials] tbody tr[data-trial^=bench]")).to_have_count(0)
    page.locator("[data-p2m-alltasks] > summary").click()
    expect(page.locator("[data-p2m-alltasks] tr[data-task-id^=bench]")).to_have_count(0)


def test_every_leaf_comes_live_from_the_records(pages, site, records):
    """"Every leaf": the scorecard, the filters and the list from the records (trial 8's leaves among them, N12 accepted),
    trials 1 to 5 the frozen data's; the reader's filter stays across the refresh; the counts in "How to read this" too."""
    rec = json.loads(json.dumps(records))
    for r in rec["results"]["results"]:
        if r["item"] == "p2m-527634":
            r["state"] = "pending"      # differs from the build's copy, so the live one shows
    page = _records_page(pages, rec, site=site)
    card = page.locator("[data-p2m-scorecard][data-source=live]")
    expect(card).to_be_visible()
    expect(card.locator("tfoot [data-cell=accepted]")).to_have_text("5")
    page.locator("[data-p2m-list] > summary").click()
    row = page.locator("[data-p2m-leaf-table] tr[data-leaf-id=p2m-527634]")
    expect(row).to_have_attribute("data-outcome", "proved")
    expect(row.locator("[data-cell=trials]")).to_have_text("8")
    page.locator("[data-p2m-filters] [data-filter=outcome][data-value=accepted]").click()
    expect(row).to_be_hidden()
    expect(page.locator("[data-p2m-list-count]")).to_have_text(re.compile(r"^Showing 5 of \d+$"))
    expect(page.locator("[data-p2m-not-scored]")).to_have_text("4")


@pytest.mark.parametrize("width", (1280, 390))
def test_the_submissions_show_the_newest_five_and_scroll(pages, site, records, width):
    """The submissions table shows the five newest (newest trial first; on a phone, at most 85% of the
    window) and scrolls inside for the rest: nothing hidden, the area takes the keyboard's focus and scrolls with it; at
    390 px the page does not scroll sideways."""
    page = _records_page(pages, records, site=site, width=width)
    expect(page.locator("[data-p2m-submissions][data-source=live]")).to_be_visible()
    box = page.locator("[data-p2m-subs-scroll]")
    expect(box).to_have_attribute("tabindex", "0")
    rows = box.locator("tbody tr")
    expect(rows).to_have_count(8)
    m = box.evaluate("b => ({ client: b.clientHeight, scroll: b.scrollHeight })")
    assert m["scroll"] > m["client"], m
    assert m["client"] <= page.viewport_size["height"] * 0.85 + 2, m
    top, sixth = box.bounding_box(), rows.nth(5).bounding_box()
    assert sixth["y"] + sixth["height"] > top["y"] + top["height"], (sixth, top)
    # the first five rows fit, unless they would take more than 85% of the window
    fifth = rows.nth(4).bounding_box()
    capped = m["client"] >= page.viewport_size["height"] * 0.85 - 2
    assert capped or fifth["y"] + fifth["height"] <= top["y"] + top["height"] + 3, (fifth, top, m)
    box.focus()
    page.keyboard.press("PageDown")
    page.wait_for_function("() => document.querySelector('[data-p2m-subs-scroll]').scrollTop > 0")
    page.keyboard.press("End")
    # the last row in view inside the area (layout settles as the fonts arrive)
    page.wait_for_function("""() => { const b = document.querySelector('[data-p2m-subs-scroll]');
        const l = b.querySelector('tbody tr:last-child').getBoundingClientRect(), r = b.getBoundingClientRect();
        return b.scrollTop + b.clientHeight >= b.scrollHeight - 2 && l.bottom <= r.bottom + 3 && l.top >= r.top - 1; }""")
    assert page.evaluate("document.documentElement.scrollWidth") <= width, "the page scrolls sideways"
    expect(page.locator("h2#submissions ~ p.caps").first).to_have_text(
        "Closed by reuse: the proof imports a theorem someone else had already proved; these are not counted as proved by the model.")
