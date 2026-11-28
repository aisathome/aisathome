"""The Tau Ceti science page (/science/tauceti/) in a browser, from the build in site/dist (conftest.py: dist_url,
site_pages). Tau Ceti is a finished project: the page renders the committed final data
(site/src/data/science/tauceti.json), labelled finished, and loads nothing live. Checked here against that data: the
numbers on top, the scorecard by type and its sorting, the list (every PR and task without one, their states, review
rounds, fix and internal rounds, tokens and minutes, sorting and filters), the chart, the progress line, the home page's
line, the menu and sidebar, a phone in both themes, and that the page says nothing internal. (That the published JSON
is the committed data with public fields only: site/scripts/check-dist.mjs.)

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_tauceti.py

SPDX-License-Identifier: Apache-2.0
"""
import calendar
import json
import re
import time
from decimal import ROUND_HALF_UP, Decimal

import pytest
from conftest import SRC
from playwright.sync_api import expect

PAGE = "/science/tauceti/"
DATA_JSON = "/science/tauceti.json"  # the final data, as the site publishes it
OLD_LIVE = "/v1/projects/tauceti/science"  # the gateway's address for it while the project ran
BUILD_DATA = SRC / "site/src/data/science/tauceti.json"


def build_data() -> dict:
    """The committed data the build renders."""
    return json.loads(BUILD_DATA.read_text())


def iso(t: float) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t))


def parse(s: str) -> float:
    return calendar.timegm(time.strptime(s[:19], "%Y-%m-%dT%H:%M:%S"))


def ago(then: float, now: float) -> str:
    """The page's "N ago" (site/src/data/science-model.ts ago)."""
    m = max(0, round((now - then) / 60))
    if m < 60:
        return "just now" if m <= 1 else f"{m} min ago"
    return f"{m // 60} h ago" if m // 60 < 48 else f"{m // 60 // 24} days ago"


@pytest.fixture(scope="module")
def posted() -> dict:
    """The final data the page renders (the name is from when a fake runner posted it)."""
    d = build_data()
    assert any(c for p in d["prs"] for rd in p.get("rounds", []) for c in rd["changes"]), \
        "the committed data has no review round that asked for changes"
    return d


def open_page(site_pages, dist_url, width: int | None = None):
    page = site_pages()
    if width:
        page.set_viewport_size({"width": width, "height": 844})
    page.goto(dist_url + PAGE)
    expect(page.locator("[data-science-root]")).to_have_attribute("data-source", "build")
    page.locator("[data-science-list] > summary").click()  # closed by default
    return page


def test_updated_line_and_numbers(site_pages, dist_url, posted):
    """The page shows the final data: "Updated N ago" from its generated_at, its totals and its
    rows; the JSON link points at the published data."""
    page = open_page(site_pages, dist_url)
    gen, t = parse(posted["generated_at"]), time.time()
    texts = {f"Updated {ago(gen, t + dt)}" for dt in (-60, 0, 60)}
    expect(page.locator("[data-science-updated]")).to_have_text(re.compile("^(" + "|".join(map(re.escape, texts)) + ")$"))
    prs = posted["prs"]
    expect(page.locator("[data-kpi=merged] dd")).to_have_text(str(sum(p["state"] == "merged" for p in prs)))
    expect(page.locator("[data-kpi=merged] dt")).to_contain_text(f"of {len(prs)} pull requests opened")
    rows = page.locator("[data-science-prs] tr[data-pr]")
    assert sorted(int(r.get_attribute("data-pr")) for r in rows.all()) == sorted(p["number"] for p in prs)
    for p in prs:
        row = page.locator(f"tr[data-pr='{p['number']}']")
        expect(row.get_by_role("link", name=f"#{p['number']}")).to_have_attribute("href", p["url"])
        if p.get("size"):
            expect(row.locator(".title > .meta").first).to_contain_text(p["size"])
    expect(page.locator("[data-science-json]")).to_have_attribute("href", DATA_JSON)
    main = page.locator("main")
    for bad in ("Claude", "Anthropic", "Opus", "codex", "undefined", "NaN"):
        expect(main).not_to_contain_text(bad)


def test_review_rounds_per_pr(site_pages, dist_url, posted):
    """Clicking a PR's review rounds opens them: one entry per round with its commit, date and
    result; each rubric that asked for changes with Tau Ceti's reason, verbatim, linked to the
    comment; and the link to Tau Ceti's review. PRs without rounds have none."""
    page = open_page(site_pages, dist_url)
    for p in posted["prs"]:
        row = page.locator(f"tr[data-pr='{p['number']}']")
        details = row.locator("[data-rounds]")
        if not p["rounds"]:
            expect(details).to_have_count(0)
            continue
        expect(details.locator("ol")).not_to_be_visible()
        details.locator("summary").click()
        expect(details.locator("summary")).to_have_text(f"{len(p['rounds'])} review round{'' if len(p['rounds']) == 1 else 's'}")
        items = details.locator("li[data-round]")
        expect(items).to_have_count(len(p["rounds"]))
        for i, rd in enumerate(p["rounds"]):
            item = items.nth(i)
            expect(item).to_have_attribute("data-result", rd["result"])
            expect(item.locator(".round-head")).to_contain_text(f"Round {rd['round']} · {rd['commit'][:7]}")
            changes = item.locator("li[data-change]")
            expect(changes).to_have_count(len(rd["changes"]))
            for j, c in enumerate(rd["changes"]):
                ch = changes.nth(j)
                expect(ch.locator(".rubric")).to_have_text(c["rubric"])
                expect(ch.locator("a[data-change-link]")).to_have_attribute("href", c["url"])
                if c["reason"]:  # verbatim: the text as written, `code` spans shown as code
                    expect(ch.locator("[data-reason]")).to_have_text(f"“{c['reason'].replace('`', '')}”")
        if p.get("review_url"):
            expect(details.locator("a[data-review-link]")).to_have_attribute("href", p["review_url"])


def test_a_closed_rounds_list_copies_no_stray_quotes(site_pages, dist_url, posted):
    """Finding 2026-10-29: a pasted page had a line of "" per closed review round. <q> makes its marks
    with generated content, which a browser's copy can pick up from a closed <details> (seen in a
    pasted page; Chromium here doesn't, so the test holds the cause): no <q>, and the marks are text."""
    page = open_page(site_pages, dist_url)
    expect(page.locator("[data-science-root] q")).to_have_count(0)
    p = next(p for p in posted["prs"] if any(c["reason"] for rd in p["rounds"] for c in rd["changes"]))
    reason = page.locator(f"tr[data-pr='{p['number']}'] [data-reason]").first
    assert reason.evaluate("e => e.textContent").startswith("“")
    assert reason.evaluate("e => getComputedStyle(e, '::before').content") == "none"


def test_not_recorded_is_no_clutter(site_pages, dist_url, posted):
    """A sum that misses early attempts shows the sum, without "+1 not recorded" under it; the hover text keeps it, and a value never recorded still says so."""
    page = open_page(site_pages, dist_url)
    table = page.locator("[data-science-prs]")
    expect(table).not_to_contain_text("+1 not recorded")
    assert not [c for c in table.locator("td.num .meta").all()]
    missing = [p for p in posted["prs"] if p["gpu_minutes"] is not None and p["attempts_without_gpu_time"]]
    for p in missing:
        expect(table.locator(f"tr[data-pr='{p['number']}'] .min")).to_have_attribute("title", re.compile("not recorded"))


def test_fix_and_internal_rounds_are_counted(site_pages, dist_url, posted):
    """Finding 2026-10-29: "Changes asked at first review" beside "0 fix rounds" on PRs with rounds. A
    PR shows its fix rounds and, when it had any, its internal rounds; every PR where Tau Ceti asked
    for changes and that went on to pass has a round to show for it."""
    page = open_page(site_pages, dist_url)
    for p in posted["prs"]:
        row = page.locator(f"tr[data-pr='{p['number']}']")
        n = p["fix_rounds"]
        expect(row.locator(".fix")).to_have_text(f"{n} fix round{'' if n == 1 else 's'}")
        m = p.get("internal_rounds") or 0
        internal = row.locator("[data-internal-rounds]")
        if m:
            expect(internal).to_have_text(f"{m} internal round{'' if m == 1 else 's'}")
        else:
            expect(internal).to_have_count(0)
        if p["state"] == "merged" and p["first_review_approved"] is False:
            assert n + m > 0, f"#{p['number']}: changes asked and merged, with no round"


def test_every_row_names_its_task(site_pages, dist_url, posted):
    """The task's id on every PR row and every row without a PR (the name was not visible in the list)."""
    page = open_page(site_pages, dist_url)
    for p in posted["prs"]:
        expect(page.locator(f"tr[data-pr='{p['number']}'] th [data-task]")).to_have_text(p["task"])
    ids = {t["task"] for t in posted["tasks_without_pr"]}
    assert ids and sorted(e.inner_text() for e in page.locator("tr[data-no-pr] th [data-task]").all()) == sorted(ids)


def test_public_text_has_no_internal_words(site_pages, dist_url, posted):
    """No reviewer's name, a rented host, a cost or our internal words anywhere on the
    page. "Internal review" is what the public sees of every review stage."""
    page = open_page(site_pages, dist_url)
    for d in page.locator("details[data-rounds]").all():
        d.evaluate("e => { e.open = true }")
    # Our own words only: mathematics in titles and Tau Ceti's quoted reasons may say "operator".
    text = page.evaluate("""() => { const m = document.querySelector('main').cloneNode(true);
        m.querySelectorAll('.title, .target, [data-reason]').forEach((e) => e.remove()); return m.textContent; }""")
    for bad in ("Claude", "Codex", "Anthropic", "GPT-", "Gemini", "Vast", "owner", "candidate", "prereview", "pre-review",
                "pre-PR", "operator", "ledger", "T arm", ".review", "E2E-", "$", "USD"):
        assert not re.search(rf"(?<!\w){re.escape(bad)}(?!\w)", text), f"the page says {bad!r}"


# ---- the numbers on top (the scorecard)

SMALL = 5
TYPES = ("Namespace move", "New lemma", "Docstring cleanup", "Generalisation", "Lint fix")


def fresh_page(site_pages, dist_url, width: int | None = None, height: int = 800):
    """The page as a visitor gets it: the list still closed."""
    page = site_pages()
    page.set_viewport_size({"width": width or 1280, "height": height})
    page.goto(dist_url + PAGE)
    expect(page.locator("[data-science-root]")).to_have_attribute("data-source", "build")
    return page


def stats(posted: dict) -> dict:
    """The numbers on top, recomputed here from the rows, per type and for all: the page must agree.
    Merged is the row's state (Bors merges count: the generator reads them, test_science_json.py)."""
    def one(prs, tasks):
        merged = [p for p in prs if p["state"] == "merged"]
        reviewed = [p for p in prs if p["first_review_approved"] is not None and (p["first_review_approved"] or p["rounds"])]
        work = prs + tasks
        tok = sum((r.get("qwen_tokens") or 0) + (r.get("review_qwen_tokens") or 0) for r in work)
        mins = sum((r.get("gpu_minutes") or 0) + (r.get("review_gpu_minutes") or 0) for r in work)
        n = len(merged)
        return {"opened": len(prs), "merged": n, "x": sum(p["first_review_approved"] is True for p in reviewed), "n": len(reviewed),
                "rounds": sum(p["fix_rounds"] + (p.get("internal_rounds") or 0) for p in merged) / n if n else None,
                "tok": tok / n if n else None, "min": mins / n if n else None}
    out = {t: one([p for p in posted["prs"] if p["type"] == t], [x for x in posted["tasks_without_pr"] if x["type"] == t]) for t in TYPES}
    out = {t: v for t, v in out.items() if v["opened"] or any(x["type"] == t for x in posted["tasks_without_pr"])}
    out["All types"] = one(posted["prs"], posted["tasks_without_pr"])
    return out


def compact(v: float) -> str:
    return f"{js_fixed(v / 1e6)}M" if v >= 1e6 else f"{js_round(v / 1e3)}k" if v >= 1e3 else str(js_round(v))


def js_fixed(v: float) -> str:
    """toFixed(1): half up on the exact value, where Python's format rounds half to even."""
    return str(Decimal(v).quantize(Decimal("0.1"), rounding=ROUND_HALF_UP))


def js_round(v: float) -> int:
    return int(v + 0.5)


def rate_text(x: int, n: int) -> str:
    if not n:
        return "none yet"
    return f"{x} of {n}†" if n < SMALL else f"{x} of {n} ({js_round(100 * x / n)}%)"


def per_merged(v, merged: int, text) -> str:
    if v is None:
        return "none merged"
    return text(v) + ("†" if merged < SMALL else "")


def test_tauceti_shows_as_finished(site_pages, dist_url, posted):
    """A finished project (no PRs since 2026-11-07, at its maintainers' request): the
    page says so above the numbers, and the home page's science block in its text."""
    page = fresh_page(site_pages, dist_url)
    note = page.locator("aside.starlight-aside--note")
    expect(note).to_be_visible()
    expect(note).to_contain_text("A finished project")
    expect(note).to_contain_text("At the request of Tau Ceti’s maintainers, we stopped sending pull requests")
    expect(note).to_contain_text("The record below is final")
    expect(page.locator("main")).not_to_contain_text("Paused")
    assert note.bounding_box()["y"] < page.locator("[data-science-kpis] div").first.bounding_box()["y"]
    home = site_pages()
    home.goto(dist_url + "/")
    expect(home.locator("[data-home-science] [data-home-tauceti]")).to_contain_text("merged into Tau Ceti")  # one short line


def test_insights_come_first_and_the_list_is_closed(site_pages, dist_url, posted):
    """The order: the numbers above the fold, by type; the list after them, closed. Its rows
    are there, one per PR and task without one. No "Working on now": nothing runs any more."""
    page = fresh_page(site_pages, dist_url)
    first = page.locator("[data-science-kpis] div").first
    box = first.bounding_box()
    assert 0 < box["y"] < 800 - box["height"], f"the first number is below the fold: {box}"
    expect(page.locator("[data-science-now]")).to_have_count(0)
    assert page.evaluate("""() => { const a = document.querySelector('[data-science-insights]'), c = document.querySelector('[data-science-list]');
        return !!(a.compareDocumentPosition(c) & Node.DOCUMENT_POSITION_FOLLOWING); }""")
    lst = page.locator("[data-science-list]")
    expect(lst).not_to_have_attribute("open", "")
    expect(lst.locator("[data-science-prs] tr[data-pr]").first).not_to_be_visible()
    assert lst.locator("tr[data-pr]").count() == len(posted["prs"]) and lst.locator("tr[data-no-pr]").count() == len(posted["tasks_without_pr"])
    lst.locator("> summary").click()
    expect(lst.locator("[data-science-prs] tr[data-pr]").first).to_be_visible()


def test_numbers_by_type_agree_with_the_rows(site_pages, dist_url, posted):
    """Each type's numbers in the scorecard as recomputed from the rows:
    merged of opened, approved at the first review with its n, and per merged PR rounds, tokens and
    GPU minutes over all the type's work. A small sample shows a count, marked, no percentage."""
    page = fresh_page(site_pages, dist_url)
    want = stats(posted)
    for t, v in want.items():
        row = page.locator(f"[data-science-scorecard] tr[data-insight-row='{t}']")
        expect(row.locator("[data-cell=merged]")).to_have_text(f"{v['merged']} of {v['opened']}")
        expect(row.locator("[data-cell=first-review]")).to_have_text(rate_text(v["x"], v["n"]))
        expect(row.locator("[data-cell=rounds]")).to_have_text(per_merged(v["rounds"], v["merged"], js_fixed))
        expect(row.locator("[data-cell=tokens]")).to_have_text(per_merged(v["tok"], v["merged"], compact))
        expect(row.locator("[data-cell=gpu]")).to_have_text(per_merged(v["min"], v["merged"], lambda m: str(js_round(m))))
        small = row.locator("[data-cell=first-review] [data-small]")
        expect(small).to_have_count(1 if 0 < v["n"] < SMALL else 0)
    a = want["All types"]
    expect(page.locator("[data-kpi=merged] dd")).to_have_text(str(a["merged"]))
    expect(page.locator("[data-science-notes]")).to_contain_text("small sample")


def test_the_cumulative_chart_reaches_the_merged_total(site_pages, dist_url, posted):
    """The graph shows every merged PR (its axis once stopped at 21 with 26 merged,
    because the ticks went up in sevens and left the top off). One point per merged PR (all Bors's,
    which GitHub shows as closed: the row's state counts them), the last point says the total, it
    equals the headline, and the axis reaches it."""
    page = fresh_page(site_pages, dist_url)
    chart = page.locator("[data-science-chart]")
    expect(chart.locator("figcaption")).to_have_text("Merged PRs over time (cumulative)")
    merged = [p for p in posted["prs"] if p["state"] == "merged"]
    expect(chart.locator("[data-merge-point]")).to_have_count(len(merged))
    expect(chart.locator("[data-merge-total]")).to_have_text(str(len(merged)))
    expect(page.locator("[data-kpi=merged] dd")).to_have_text(str(len(merged)))
    ticks = [int(t.get_attribute("data-y-tick")) for t in chart.locator("[data-y-tick]").all()]
    assert max(ticks) >= len(merged), (ticks, len(merged))
    ids = sorted(merged, key=lambda p: parse(p["merged"]))
    last = chart.locator("[data-merge-point]").last.get_attribute("data-tip")
    assert last.endswith(f"({len(merged)} in total)"), last
    assert f"#{ids[-1]['number']} " in last or parse(ids[-1]["merged"]) == parse(ids[-2]["merged"]), last


def test_what_was_taken_out_stays_out(site_pages, dist_url, posted):
    """Taken out on 2026-10-31: three headline numbers only (no first-review rate, no rounds
    per merged PR, no new-lemma box), no "New mathematics and housekeeping" block, neither of the two
    weekly charts with their legends and notes; the other layouts and their switch are gone."""
    page = fresh_page(site_pages, dist_url)
    assert [k.get_attribute("data-kpi") for k in page.locator("[data-science-kpis] > div").all()] == ["merged", "gpu", "tokens"]
    expect(page.locator("[data-chart=new-share]")).to_have_count(0)
    main = page.locator("main")
    for gone in ("New mathematics and housekeeping", "the rest is housekeeping", "Merged per week", "by week opened",
                 "weeks start on Monday", "Darker is a higher share"):
        expect(main).not_to_contain_text(gone)
    expect(page.locator("[data-chart], [data-legend]")).to_have_count(0)
    expect(page.locator("[data-layout-switch]")).to_have_count(0)
    for gone in ("scorecard", "by-type", "trends"):
        assert page.request.get(f"{dist_url}{PAGE}{gone}/").status == 404, gone


# ---- the sortable scorecard: the same control as the list; All types stays last

SCORE_FIRST = {"type": "asc", "merged": "desc", "first-review": "desc", "rounds": "desc", "tokens": "desc", "gpu": "desc"}


def score_rows(posted: dict) -> list[str]:
    return [t for t in stats(posted) if t != "All types"]


def score_order(posted: dict, key: str, direction: str) -> list[str]:
    """Worked out here. Merged by count, then opened. A rate by rate, but counts under 5 are no rates:
    rates, then marked counts by count, then none yet, those tiers in that order both ways. A mean by
    its value, with "none merged" last. Ties keep the page's order."""
    v = stats(posted)
    names = score_rows(posted)
    sign = 1 if direction == "asc" else -1
    def rank(t):
        r = v[t]
        if key == "type":
            return (0, sign * 0)
        if key == "merged":
            return (0, sign * r["merged"], sign * r["opened"])
        if key == "first-review":
            if not r["n"]:
                return (2,)
            return (0, sign * r["x"] / r["n"], sign * r["n"]) if r["n"] >= SMALL else (1, sign * r["x"], sign * r["n"])
        x = r[{"rounds": "rounds", "tokens": "tok", "gpu": "min"}[key]]
        return (1,) if x is None else (0, sign * x)
    if key == "type":
        return sorted(names, reverse=direction == "desc")
    return sorted(names, key=rank)  # stable: ties keep the page's order


def score_shown(page) -> list[str]:
    return page.evaluate("""() => [...document.querySelectorAll('[data-science-scorecard] tbody tr, [data-science-scorecard] tfoot tr')]
        .map((r) => r.dataset.insightRow)""")


def score_head(page, key: str):
    return page.locator(f"[data-science-scorecard] th[data-score-col='{key}']")


def score_sorted(page, key: str, direction: str):
    th = score_head(page, key)
    expect(th).to_have_attribute("aria-sort", "ascending" if direction == "asc" else "descending")
    expect(th.locator(".sort-mark")).to_have_text("▲" if direction == "asc" else "▼")
    expect(page.locator("[data-science-scorecard] th[aria-sort]")).to_have_count(1)


def test_the_scorecard_starts_in_its_own_order_unsorted(site_pages, dist_url, posted):
    page = fresh_page(site_pages, dist_url)
    assert score_shown(page) == [*score_rows(posted), "All types"]
    expect(page.locator("[data-science-scorecard] th[aria-sort]")).to_have_count(0)


@pytest.mark.parametrize("key", SCORE_FIRST)
def test_each_scorecard_column_sorts_both_ways_with_all_types_last(site_pages, dist_url, posted, key):
    """Against values recomputed from the rows, each way; "All types" is last every time."""
    page = fresh_page(site_pages, dist_url)
    button = score_head(page, key).locator("button")
    first = SCORE_FIRST[key]
    second = "asc" if first == "desc" else "desc"
    button.click()
    score_sorted(page, key, first)
    assert score_shown(page) == [*score_order(posted, key, first), "All types"]
    button.click()
    score_sorted(page, key, second)
    assert score_shown(page) == [*score_order(posted, key, second), "All types"]


def test_the_scorecard_sorts_from_the_keyboard(site_pages, dist_url, posted):
    page = fresh_page(site_pages, dist_url)
    button = score_head(page, "merged").locator("button")
    button.focus()
    page.keyboard.press("Enter")
    score_sorted(page, "merged", "desc")
    page.keyboard.press("Space")
    score_sorted(page, "merged", "asc")
    assert score_shown(page) == [*score_order(posted, "merged", "asc"), "All types"]
    assert page.evaluate("document.activeElement.dataset.scoreSort") == "merged"


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_the_scorecard_sorts_on_a_phone_in_both_themes(site_pages, dist_url, posted, theme):
    page = fresh_page(site_pages, dist_url, width=390, height=844)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    button = score_head(page, "gpu").locator("button")
    button.click()
    score_sorted(page, "gpu", "desc")
    expect(score_head(page, "gpu").locator(".sort-mark")).to_be_visible()
    assert score_shown(page) == [*score_order(posted, "gpu", "desc"), "All types"]
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"


# ---- the sortable list: headers sort, newest first by default

STATUS_ORDER = ("merged", "open", "closed", "running", "queued", "checks-passed", "failed")
SORT_FIRST = {"number": "desc", "title": "asc", "date": "desc", "status": "asc", "tokens": "desc", "minutes": "desc"}


def when(s: str | None) -> float | None:
    return parse(s) if s else None


def expected_order(page, posted, key: str, direction: str, keep=lambda r: True) -> list[str]:
    """The rows' ids in order, worked out here from the posted rows: a value that is missing goes last
    in either direction, ties go newest first. Titles compare as the browser does."""
    rows = [{"id": str(p["number"]), "kind": "pr", "row": p, "number": p["number"], "title": p["title"], "date": when(p["opened"]),
             "status": p["state"], "tokens": p["qwen_tokens"], "minutes": p["gpu_minutes"]} for p in posted["prs"]]
    rows += [{"id": t["task"], "kind": "task", "row": t, "number": None, "title": t["target"], "date": when(t["last_attempt"]),
              "status": t["state"], "tokens": t["qwen_tokens"], "minutes": t["gpu_minutes"]} for t in posted["tasks_without_pr"]]
    ranks = page.evaluate("""ts => { const s = [...new Set(ts)].sort((a, b) => a.localeCompare(b)); return Object.fromEntries(s.map((t, i) => [t, i])); }""",
                          [r["title"] for r in rows])
    def value(r):
        if key == "title":
            return ranks[r["title"]]
        if key == "status":
            return STATUS_ORDER.index(r["status"])
        return r[key]
    sign = 1 if direction == "asc" else -1
    ties = sorted(rows, key=lambda r: (-(r["date"] or 0), -(r["number"] or 0), ranks[r["title"]]))
    known = sorted((r for r in ties if value(r) is not None), key=lambda r: sign * value(r))  # stable: ties keep their order
    return [r["id"] for r in known + [r for r in ties if value(r) is None] if keep(r)]


def shown_order(page) -> list[str]:
    return page.evaluate("""() => [...document.querySelectorAll('[data-science-prs] tbody tr')].filter((r) => !r.hidden)
        .map((r) => r.dataset.pr ?? r.dataset.taskId)""")


def header(page, key: str):
    return page.locator(f"[data-science-prs] th[data-sort-col={key}]")


def sorted_by(page, key: str, direction: str):
    th = header(page, key)
    expect(th).to_have_attribute("aria-sort", "ascending" if direction == "asc" else "descending")
    expect(th.locator(".sort-mark")).to_have_text("▲" if direction == "asc" else "▼")
    expect(page.locator("[data-science-prs] th[aria-sort]")).to_have_count(1)


def test_the_list_starts_newest_first_and_closed(site_pages, dist_url, posted):
    """Newest first, shown as such, in the closed list and once open; also as the build renders it."""
    page = fresh_page(site_pages, dist_url)
    expect(page.locator("[data-science-list]")).not_to_have_attribute("open", "")
    sorted_by(page, "date", "desc")
    assert shown_order(page) == expected_order(page, posted, "date", "desc")
    page.locator("[data-science-list] > summary").click()
    assert shown_order(page) == expected_order(page, posted, "date", "desc")
    built = site_pages()
    built.goto(dist_url + PAGE)
    d = build_data()
    expect(built.locator("[data-science-root]")).to_have_attribute("data-source", "build")
    want = expected_order(built, d, "date", "desc")
    assert shown_order(built) == want
    # the HTML itself, as a browser without JavaScript gets it
    html = built.request.get(dist_url + PAGE).text()
    ids = [a or b for a, b in re.findall(r'<tr data-pr="(\d+)"|<tr data-no-pr[^>]*data-task-id="([^"]+)"', html)]
    assert ids == want


@pytest.mark.parametrize("key", SORT_FIRST)
def test_each_column_sorts_both_ways(site_pages, dist_url, posted, key):
    """A click sorts by the column against the row data, a second click reverses it; a missing value
    stays last either way; the arrow shows which column and which way."""
    page = open_page(site_pages, dist_url)
    button = header(page, key).locator("button")
    first = SORT_FIRST[key]
    second = "asc" if first == "desc" else "desc"
    if key == "date":
        button.click()               # it starts on this column: the first click reverses
        first, second = second, first
    else:
        button.click()
    sorted_by(page, key, first)
    assert shown_order(page) == expected_order(page, posted, key, first)
    button.click()
    sorted_by(page, key, second)
    assert shown_order(page) == expected_order(page, posted, key, second)


def test_ties_go_newest_first_whatever_came_before(site_pages, dist_url, posted):
    """Rows equal on the column (a status) keep the fixed tie order, not the previous sort's."""
    page = open_page(site_pages, dist_url)
    header(page, "date").locator("button").click()   # oldest first
    sorted_by(page, "date", "asc")
    header(page, "status").locator("button").click()
    sorted_by(page, "status", "asc")
    assert shown_order(page) == expected_order(page, posted, "status", "asc")


def test_sorting_works_from_the_keyboard(site_pages, dist_url, posted):
    page = open_page(site_pages, dist_url)
    button = header(page, "number").locator("button")
    button.focus()
    page.keyboard.press("Enter")
    sorted_by(page, "number", "desc")
    page.keyboard.press("Space")
    sorted_by(page, "number", "asc")
    assert shown_order(page) == expected_order(page, posted, "number", "asc")
    assert page.evaluate("document.activeElement.dataset.sort") == "number"


def test_sorting_works_with_the_filters(site_pages, dist_url, posted):
    """A sort applies to what the filters leave, and to everything again when they are cleared; a
    filter set after a sort keeps it."""
    page = open_page(site_pages, dist_url)
    lst = page.locator("[data-science-list]")
    t = "Namespace move"
    is_type = lambda r: r["row"]["type"] == t
    lst.locator(f"[data-science-filters] [data-filter=type][data-value='{t}']").click()
    header(page, "tokens").locator("button").click()
    assert shown_order(page) == expected_order(page, posted, "tokens", "desc", is_type)
    lst.locator("[data-science-filters] [data-filter=state][data-value='merged']").click()
    assert shown_order(page) == expected_order(page, posted, "tokens", "desc", lambda r: is_type(r) and r["status"] == "merged")
    header(page, "tokens").locator("button").click()
    assert shown_order(page) == expected_order(page, posted, "tokens", "asc", lambda r: is_type(r) and r["status"] == "merged")
    lst.locator("[data-science-filters] [data-filter=type][data-value='']").click()
    lst.locator("[data-science-filters] [data-filter=state][data-value='']").click()
    assert shown_order(page) == expected_order(page, posted, "tokens", "asc")


@pytest.mark.parametrize("theme", ("dark", "light"))
def test_sorting_on_a_phone_in_both_themes(site_pages, dist_url, posted, theme):
    """The headers are there on a phone, in a row of their own, and sort; the arrow reads on either
    theme; nothing scrolls sideways. At desktop width no column is cut off."""
    page = open_page(site_pages, dist_url)
    page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
    wrap = page.locator("[data-science-list] .sci-wrap")
    assert wrap.evaluate("e => e.scrollWidth <= e.clientWidth"), "the table is wider than its column"
    page.set_viewport_size({"width": 390, "height": 844})
    button = header(page, "minutes").locator("button")
    expect(button).to_be_visible()
    button.click()
    sorted_by(page, "minutes", "desc")
    assert shown_order(page) == expected_order(page, posted, "minutes", "desc")
    expect(header(page, "minutes").locator(".sort-mark")).to_be_visible()
    colours = page.evaluate("""() => { const c = getComputedStyle(document.querySelector('[data-science-prs] .sort-mark')); return c.color; }""")
    assert colours != "rgba(0, 0, 0, 0)"
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"


def test_the_list_filters_by_type_and_state(site_pages, dist_url, posted):
    """Chips narrow the list to one type and one state, with the count; "No PR" shows the tasks
    without one; "All" brings everything back."""
    page = fresh_page(site_pages, dist_url)
    lst = page.locator("[data-science-list]")
    lst.locator("> summary").click()
    total = len(posted["prs"]) + len(posted["tasks_without_pr"])
    expect(lst.locator("[data-list-count]")).to_have_text(f"Showing {total} of {total}")
    chip = lambda kind, value: lst.locator(f"[data-science-filters] [data-filter={kind}][data-value='{value}']")
    t = "Namespace move"
    chip("type", t).click()
    expect(chip("type", t)).to_have_attribute("aria-pressed", "true")
    prs = [p for p in posted["prs"] if p["type"] == t]
    no = [x for x in posted["tasks_without_pr"] if x["type"] == t]
    expect(lst.locator("tr[data-pr]:visible")).to_have_count(len(prs))
    expect(lst.locator("[data-list-count]")).to_have_text(f"Showing {len(prs) + len(no)} of {total}")
    expect(lst.locator("tr[data-type='Docstring cleanup']:visible")).to_have_count(0)
    chip("state", "merged").click()
    expect(lst.locator("tr[data-pr]:visible")).to_have_count(sum(p["state"] == "merged" for p in prs))
    expect(lst.locator("tr[data-no-pr]:visible")).to_have_count(0)
    chip("state", "no-pr").click()
    expect(lst.locator("tr[data-pr]:visible")).to_have_count(0)
    expect(lst.locator("tr[data-no-pr]:visible")).to_have_count(len(no))
    chip("type", "").click()
    chip("state", "").click()
    expect(lst.locator("[data-list-count]")).to_have_text(f"Showing {total} of {total}")


def test_a_type_in_the_scorecard_opens_the_list_on_it(site_pages, dist_url, posted):
    page = fresh_page(site_pages, dist_url)
    t = "Generalisation"
    page.locator(f"[data-science-scorecard] tr[data-insight-row='{t}'] button").click()
    lst = page.locator("[data-science-list]")
    expect(lst).to_have_attribute("open", "")
    expect(lst.locator("tr[data-pr]:visible")).to_have_count(sum(p["type"] == t for p in posted["prs"]))


def test_a_chart_mark_tells_its_value(site_pages, dist_url, posted):
    """Hover and keyboard focus show the same text; nothing is left hanging."""
    page = fresh_page(site_pages, dist_url)
    mark = page.locator("[data-science-chart] [data-merge-point]").first
    tip = page.locator("[data-science-tip]")
    mark.hover()
    expect(tip).to_be_visible()
    expect(tip).to_have_text(mark.get_attribute("data-tip"))
    page.mouse.move(0, 0)
    mark.focus()
    expect(tip).to_be_visible()
    page.locator("h1").click()
    expect(tip).to_be_hidden()


def test_the_page_fits_a_phone_and_both_themes(site_pages, dist_url, posted):
    page = fresh_page(site_pages, dist_url, width=390, height=844)
    lst = page.locator("[data-science-list]")
    lst.locator("> summary").click()
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    box = page.locator("[data-science-kpis]").bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 390, box
    assert page.evaluate("getComputedStyle(document.querySelector('[data-science-chart] [data-merge-total]')).fontSize") == "22px"  # the chart's text stays readable
    lst.locator("[data-science-filters] [data-filter=state][data-value='no-pr']").click()   # rows are grids on a phone
    expect(lst.locator("tr[data-pr]:visible")).to_have_count(0)
    series = lambda: page.evaluate("getComputedStyle(document.documentElement).getPropertyValue('--sci-1').trim()")
    colours = set()
    for theme in ("dark", "light"):
        page.evaluate("t => { document.documentElement.dataset.theme = t }", theme)
        colours.add(series())
    assert len(colours) == 2, colours


def test_rounds_fit_a_phone(site_pages, dist_url, posted):
    page = open_page(site_pages, dist_url, width=390)
    p = max(posted["prs"], key=lambda p: sum(len(r["changes"]) for r in p["rounds"]))
    page.locator(f"tr[data-pr='{p['number']}'] [data-rounds] summary").click()
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"


def test_tau_cetis_own_progress(site_pages, dist_url, posted):
    """One line on Tau Ceti as a whole, from the final data: merged PRs in the window, their GitHub
    accounts, roadmaps active and declared complete, with the date it was read."""
    page = open_page(site_pages, dist_url)
    p = posted["project_progress"]
    line = page.locator("[data-science-progress]")
    expect(line).to_have_count(1)
    day = time.strftime("%Y-%m-%d", time.gmtime(parse(p["as_of"])))
    expect(line.locator("time[data-progress-asof]")).to_have_attribute("datetime", day)
    expect(line.locator("[data-progress-merged]")).to_have_text(f"{p['merged_prs']:,} pull requests merged in the last {p['window_days']} days")
    expect(line.locator("[data-progress-authors]")).to_have_text(f"{p['merged_pr_authors']:,} GitHub accounts")
    expect(line.locator("[data-progress-roadmaps]")).to_have_text(
        f"{p['roadmaps_active']:,} active roadmaps, and {p['roadmaps_completed']:,} declared complete by its maintainers")
    expect(line).to_contain_text("Tau Ceti as a whole, as of ")


def test_the_page_asks_nothing_live(site_pages, dist_url, posted):
    """The Tau Ceti page and the home page ask the gateway for nothing about Tau Ceti; the home page's count is the
    final data's."""
    for path in (PAGE, "/"):
        page = site_pages()
        asked = []
        page.on("request", lambda r: asked.append(r.url))
        page.goto(dist_url + path)
        page.wait_for_load_state("networkidle")
        assert not [u for u in asked if "/v1/projects/tauceti" in u or u.endswith(DATA_JSON)], f"{path} asked {asked}"
    expect(page.locator("[data-home-science-merged]")).to_have_text(str(sum(p["state"] == "merged" for p in posted["prs"])))


# ---- the page as the build renders it, the home page's line, the menu

# The stop (2026-11-07): at Tau Ceti's request we closed our open PRs, and tasks that passed their checks were never
# sent (they read as rejections and as pending before).
PAUSE_CLOSED = (24465, 25595, 25788, 25838)
CLOSED_BY_US = "Closed by us at Tau Ceti’s request"
NOT_SENT = "Not sent: project finished"


def _build_page(site_pages, width: int | None = None):
    """The science page as the build renders it. The old address stays blocked so that a regression cannot change
    what these tests see."""
    page = site_pages(width=width or 1400, height=844 if width else 900)
    page.route(f"**{OLD_LIVE}", lambda route: route.abort())
    return page


def test_science_page_shows_every_pr_and_how_to_read_it(site_pages, dist_url, posted):
    """Headline numbers agree with the JSON; the table lists every PR in the JSON (closed ones too) with its GitHub
    link, status, fix rounds and first review; the chart adds up to the merges; "How to read this" says who judges and
    links the JSON."""
    page = _build_page(site_pages)
    page.goto(dist_url + PAGE)
    page.locator("[data-science-list] > summary").click()
    expect(page.locator("h1")).to_have_text("Science: Tau Ceti")
    d = posted
    prs = d["prs"]
    merged = [p for p in prs if p["state"] == "merged"]
    expect(page.locator("[data-kpi=merged] dd")).to_have_text(str(len(merged)))
    expect(page.locator("[data-kpi=merged] dt")).to_contain_text(f"of {len(prs)} pull requests opened")
    # The first review: approved, changes asked (with a review round to show for it), or pending.
    first = {p["number"]: "approved" if p["first_review_approved"] is True else
             "changes" if p["first_review_approved"] is False and p["rounds"] else "pending" for p in prs}
    approved = sum(v == "approved" for v in first.values())
    reviewed = sum(v != "pending" for v in first.values())
    expect(page.locator("tr[data-insight-row='All types'] [data-cell=first-review]")).to_contain_text(f"{approved} of {reviewed}")
    rows = page.locator("[data-science-prs] tr[data-pr]")
    assert sorted(int(r.get_attribute("data-pr")) for r in rows.all()) == sorted(p["number"] for p in prs)
    for p in prs:
        row = page.locator(f"tr[data-pr='{p['number']}']")
        expect(row.get_by_role("link", name=f"#{p['number']}")).to_have_attribute("href", p["url"])
        expect(row.locator(".fix")).to_have_attribute("data-fix-rounds", str(p["fix_rounds"]))
        expect(row.locator(".first")).to_have_attribute("data-first-review", first[p["number"]])
        if first[p["number"]] == "pending":
            expect(row.locator(".first")).to_have_text("First review pending")
        expect(row.locator(".state")).to_contain_text(
            {"merged": "Merged", "closed": CLOSED_BY_US if p["number"] in PAUSE_CLOSED else "Closed, not merged",
             "open": "Open"}[p["state"]])
        if p["title"]:
            expect(row.locator(".title")).to_contain_text(p["title"])
    chart = page.locator("[data-science-chart]")
    expect(chart).to_be_visible()
    expect(chart.locator("[data-merge-point]")).to_have_count(len(merged))
    expect(page.locator("h2#how-to-read-this")).to_have_text("How to read this")
    main = page.locator("main")
    expect(main).to_contain_text("Tau Ceti’s review is the judge")
    expect(main).to_contain_text("our own measurements")
    expect(page.locator("[data-science-asof]")).to_have_attribute("datetime", d["states_as_of"])
    for bad in ("Claude", "Anthropic", "undefined", "NaN"):
        expect(main).not_to_contain_text(bad)
    expect(page.locator("[data-science-json]")).to_have_attribute("href", DATA_JSON)


def test_science_intro_links_tau_cetis_own_pages(site_pages, dist_url, posted):
    """The introduction links Tau Ceti's own statistics, progress board and roadmaps, and the build shows Tau Ceti's
    progress line from the committed JSON when it has one, nothing when not."""
    page = _build_page(site_pages)
    page.goto(dist_url + PAGE)
    want = {"statistics": "https://taucetiproject.org/statistics/", "progress": "https://taucetiproject.org/progress/",
            "roadmaps": "https://github.com/TauCetiProject/TauCetiRoadmap"}
    expect(page.locator("main [data-tauceti-link]")).to_have_count(len(want))
    for key, href in want.items():
        expect(page.locator(f"main a[data-tauceti-link={key}]")).to_have_attribute("href", href)
    line = page.locator("[data-science-progress]")
    p = posted.get("project_progress")
    if not p:
        expect(line).to_have_count(0)
        return
    if p.get("merged_prs") is not None:
        expect(line.locator("[data-progress-merged]")).to_have_attribute("data-progress-merged", str(p["merged_prs"]))
        expect(line.locator("[data-progress-authors]")).to_have_attribute("data-progress-authors", str(p["merged_pr_authors"]))
    if p.get("roadmaps_active") is not None:
        expect(line.locator("[data-progress-roadmaps]")).to_have_attribute("data-progress-roadmaps", str(p["roadmaps_active"]))
        expect(line.locator("[data-progress-roadmaps]")).to_have_attribute("data-progress-completed", str(p["roadmaps_completed"]))
    expect(line.locator("time[data-progress-asof]")).to_have_attribute("datetime", p["as_of"][:10])


def _cell_value(cell, attr: str):
    """A token or minutes cell: its exact value, or None when it says "not recorded"."""
    v = cell.get_attribute(attr)
    if v is None:
        expect(cell).to_contain_text("not recorded")
        return None
    return float(v)


def test_science_table_tokens_and_minutes_are_the_datas(site_pages, dist_url, posted):
    """Each row's tokens and minutes are the JSON's (exact number in the tooltip, and the written tokens), an unknown
    value says "not recorded", never 0; tasks without a PR show their type; the note says what the cache re-reads are."""
    page = _build_page(site_pages)
    page.goto(dist_url + PAGE)
    d = posted
    for p in d["prs"]:
        for cls, attr, field in ((".tok", "data-tokens", "qwen_tokens"), (".min", "data-minutes", "gpu_minutes")):
            cell = page.locator(f"tr[data-pr='{p['number']}'] {cls}")
            v = _cell_value(cell, attr)
            assert v == p[field], f"#{p['number']} {field}: page {v}, JSON {p[field]}"
            assert v != 0, f"#{p['number']} {field} shows 0"
            if field == "qwen_tokens" and v is not None:
                title = cell.get_attribute("title")
                assert f"{int(v):,}" in title, f"#{p['number']}: tooltip {title!r}"
                if p["qwen_output_tokens"] is not None:
                    assert f"{p['qwen_output_tokens']:,} of them written" in title
    for t in d.get("tasks_without_pr", []):
        row = page.locator(f"tr[data-no-pr][data-task-id='{t['task']}']")
        expect(row.locator(".title")).to_contain_text(t["type"])
        for cls, attr, field in ((".tok", "data-tokens", "qwen_tokens"), (".min", "data-minutes", "gpu_minutes")):
            assert _cell_value(row.locator(cls), attr) == t[field], f"{t['target']} {field}"
    expect(page.locator(".sci-note")).to_contain_text("re-read from the cache")


def test_science_page_fits_a_phone(site_pages, dist_url):
    page = _build_page(site_pages, width=390)
    page.goto(dist_url + PAGE)
    page.locator("[data-science-list] > summary").click()
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    first = page.locator("[data-science-prs] tr[data-pr]").first
    expect(first.get_by_role("link")).to_be_visible()
    box = first.bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 390, f"a PR row on a phone: {box}"


def test_science_pause_says_who_closed_and_what_is_not_sent(site_pages, dist_url, posted):
    """The four PRs we closed on 2026-11-07 at Tau Ceti's request read "Closed by us at Tau Ceti's request", not
    "Closed, not merged" like one Tau Ceti turned down; tasks that passed their checks read "Not sent: project
    finished", not "Waiting for internal review or to be sent"."""
    d = posted
    closed = {p["number"] for p in d["prs"] if p["state"] == "closed"}
    assert set(PAUSE_CLOSED) <= closed, f"the build's data does not have the pause closures: {sorted(closed)}"
    assert closed - set(PAUSE_CLOSED), "no PR closed by Tau Ceti left to compare with"
    page = _build_page(site_pages)
    page.goto(dist_url + PAGE)
    expect(page.locator("[data-science-root]")).to_have_attribute("data-source", "build")
    page.locator("[data-science-list] > summary").click()
    for p in d["prs"]:
        state = page.locator(f"tr[data-pr='{p['number']}'] .state")
        if p["number"] in PAUSE_CLOSED:
            expect(state).to_have_text(CLOSED_BY_US)
            expect(state).to_have_attribute("data-closed-by", "us")
        elif p["state"] == "closed":
            expect(state).to_have_text("Closed, not merged")
            expect(state).not_to_have_attribute("data-closed-by", "us")
    passed = [t for t in d["tasks_without_pr"] if t["state"] == "checks-passed"]
    assert passed, "no task that passed its checks"
    for t in passed:
        row = page.locator(f"tr[data-no-pr][data-task-id='{t['task']}']")
        expect(row.locator(".state")).to_have_text("Passed local checks")
        expect(row.locator(".status .meta")).to_have_text(NOT_SENT)
    expect(page.locator("[data-science-prs]")).not_to_contain_text("Waiting for internal review")


def test_home_page_science_section(site_pages, dist_url, posted):
    """Right after the hero, before the live strip, the node block and the dev log: the merged count from the JSON in
    Tau Ceti's one line, and a link to the science page."""
    page = site_pages()
    page.route(f"**{OLD_LIVE}", lambda route: route.abort())
    page.goto(dist_url + "/")
    merged = sum(p["state"] == "merged" for p in posted["prs"])
    section = page.locator("[data-home-science]")
    if not merged:
        expect(section.locator("[data-home-tauceti]")).to_have_count(0)
        return
    expect(section.locator("h2")).to_have_text("Our GPUs prove theorems")
    expect(section.locator("[data-home-tauceti]")).to_contain_text("merged into Tau Ceti")
    expect(section.locator("[data-home-science-merged]")).to_have_text(str(merged))
    hero = page.locator("h1#_top").bounding_box()
    box = section.bounding_box()
    assert box["y"] > hero["y"] + hero["height"], "the section is not after the hero"
    order = page.evaluate("""() => [...document.querySelectorAll('[data-home-science], [data-live-stats], [data-home-node], [data-home-devlog]')]
        .map((e) => e.matches('[data-home-science]') ? 'science' : e.matches('[data-live-stats]') ? 'live' : e.matches('[data-home-node]') ? 'node' : 'devlog')""")
    assert order == ["science", "live", "node", "devlog"], f"home page order: {order}"
    section.locator("[data-home-tauceti] a").click()
    page.wait_for_url(dist_url + PAGE)


def test_home_science_count_is_the_final_data(site_pages, dist_url, posted):
    """Tau Ceti is finished, so the home count is the final data's and nothing is asked live: even newer data offered
    at the old address changes nothing."""
    merged = sum(p["state"] == "merged" for p in posted["prs"])
    if not merged:
        pytest.skip("no merges in the build's data: no home section")
    more = json.loads(json.dumps(posted))
    p = next(p for p in more["prs"] if p["state"] == "open" or p["state"] == "closed")
    p.update(state="merged", merged=p["opened"], closed=None)
    more["generated_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    page = site_pages()
    asked = []
    page.route(f"**{OLD_LIVE}", lambda route: (asked.append(route.request.url),
                                               route.fulfill(status=200, content_type="application/json", body=json.dumps(more))))
    page.goto(dist_url + "/")
    page.wait_for_load_state("networkidle")
    section = page.locator("[data-home-science]")
    expect(section.locator("[data-home-science-merged]")).to_have_text(str(merged))
    expect(section).not_to_have_attribute("data-source", "live")
    assert not asked, f"the home page asked {asked}"


def test_menu_and_sidebar_link_to_science(site_pages, dist_url):
    """The header's "Science" opens the overview (/science/); the sidebar's Tau Ceti entry opens the Tau Ceti page and
    is marked current there."""
    page = site_pages()
    page.goto(dist_url + "/")
    page.locator("nav.top-links").get_by_role("link", name="Science").click()
    page.wait_for_url(dist_url + "/science/")
    expect(page.locator("nav.sidebar ul.top-level a[href='/science/']")).to_have_attribute("aria-current", "page")
    page.locator("nav.sidebar").get_by_role("link", name="Tau Ceti", exact=True).click()
    page.wait_for_url(dist_url + PAGE)
    expect(page.locator("nav.sidebar").get_by_role("link", name="Tau Ceti", exact=True)).to_have_attribute("aria-current", "page")
