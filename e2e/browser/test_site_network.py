"""The network page's daily history ("Day by day", from /v1/stats/history) and its "Pressure" section (from
/v1/stats/pressure) in a browser, from the build in site/dist (conftest.py: dist_url, site_pages), each with fixed
gateway answers (page.route): the charts, their hover titles and tables, the line that says what limits the network
in words, the range picker, and that missing, broken or too little data hides the section.

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_network.py

SPDX-License-Identifier: Apache-2.0
"""
import json
import math
import re
import time

import pytest
from playwright.sync_api import expect

HISTORY = "**/v1/stats/history"


def fixture_days(n: int = 12, classes_from: int = 5, nodes_from: int = 8) -> list[dict]:
    days = []
    for i in range(n):
        d = {"date": time.strftime("%Y-%m-%d", time.gmtime(time.time() - (n - 1 - i) * 86400)),
             "requests": 50 + 10 * i, "completed": 48 + 10 * i,
             "prompt_tokens": 100_000 + 5_000 * i, "completion_tokens": 9_000 + 700 * i}
        if i >= classes_from:  # the classes add up to the day, in requests and in tokens (as the gateway counts)
            chat = {"requests": 10 + i, "prompt_tokens": 20_000 + 1_000 * i, "completion_tokens": 2_000 + 100 * i}
            agent = {"requests": 20 + 2 * i, "prompt_tokens": 30_000 + 2_000 * i, "completion_tokens": 3_000 + 200 * i}
            d["classes"] = {"chat": chat, "agent": agent, "science": {
                k: d[k] - chat[k] - agent[k] for k in ("requests", "prompt_tokens", "completion_tokens")}}
        if i >= nodes_from:
            d["nodes_peak"], d["nodes_avg"] = 3 + i % 2, 2.5
        days.append(d)
    return days


def history_page(site_pages, dist_url, body: str | None, status: int = 200, width: int = 1200):
    """The network page with /v1/stats/history answered by body ."""
    page = site_pages()
    page.set_viewport_size({"width": width, "height": 900})
    if body is not None:
        page.route(HISTORY, lambda r: r.fulfill(status=status, content_type="application/json", body=body))
    with page.expect_response(lambda r: "/v1/stats/history" in r.url):
        page.goto(dist_url + "/network/")
    page.wait_for_timeout(300)  # the script draws after the answer
    return page


def test_history_charts_draw_the_days(site_pages, dist_url):
    """With days of data the section shows three charts: requests and tokens, each stacked by
    class (with the "before classes" part only for days without classes), and nodes online for
    the days that have samples; every day has a hover title with the split; each day's stacked
    parts add up to the day's total; the table lists every day, newest first; on a phone
    nothing scrolls sideways."""
    days = fixture_days()
    page = history_page(site_pages, dist_url, json.dumps({"updated": "2026-10-23T12:00:00Z", "days": days}))
    box = page.locator("[data-history]")
    expect(box).to_be_visible()
    expect(box.locator("h2")).to_have_text("Day by day")
    req = box.locator('[data-chart="requests"]')
    expect(req).to_be_visible()
    expect(req.locator("g.day")).to_have_count(len(days))
    expect(req.locator("rect.s-other")).to_have_count(5)  # the days before classes were counted
    expect(req.locator("rect.s-science")).to_have_count(len(days) - 5)
    expect(req.locator("[data-legend-earlier]")).to_be_visible()
    last = days[-1]
    expect(req.locator("g.day").last.locator("title")).to_have_text(
        f"{last['date']}: {last['requests']} requests (chat {last['classes']['chat']['requests']}, "
        f"API and agents {last['classes']['agent']['requests']}, science {last['classes']['science']['requests']})")
    assert "Requests a day, 12 days" in req.locator("svg").get_attribute("aria-label")
    tok = box.locator('[data-chart="tokens"]')
    expect(tok).to_be_visible()
    expect(tok.locator("g.day")).to_have_count(len(days))
    expect(tok.locator("rect.s-other")).to_have_count(5)  # only the days before classes were counted
    for k in ("chat", "agent", "science"):
        expect(tok.locator(f"rect.s-{k}")).to_have_count(len(days) - 5)
    expect(tok.locator("[data-legend-earlier]")).to_be_visible()
    expect(tok.locator(".legend li")).to_have_text(["Chat", "API and agents", "Science", "Before classes were counted"])
    ct = {k: last["classes"][k]["prompt_tokens"] + last["classes"][k]["completion_tokens"] for k in ("chat", "agent", "science")}
    total = last["prompt_tokens"] + last["completion_tokens"]
    assert sum(ct.values()) == total  # the fixture's classes cover the day
    expect(tok.locator("g.day").last.locator("title")).to_have_text(
        f"{last['date']}: {total:,} tokens (chat {ct['chat']:,}, API and agents {ct['agent']:,}, science {ct['science']:,})")
    expect(tok.locator("g.day").first.locator("title")).to_have_text(
        f"{days[0]['date']}: {days[0]['prompt_tokens'] + days[0]['completion_tokens']:,} tokens")  # no split before classes
    assert "Tokens a day, 12 days" in tok.locator("svg").get_attribute("aria-label")
    # Each day's stack is as tall as its total: the parts add up to the day, no more and no less.
    drawn = tok.locator("svg").evaluate("""svg => {
        const base = Number(svg.querySelector('line.base').getAttribute('y1'));
        return [...svg.querySelectorAll('g.day')].map((g) => {
            const ys = [...g.querySelectorAll('rect.bar')].map((r) => Number(r.getAttribute('y')));
            return base - Math.min(...ys);
        });
    }""")
    totals = [d["prompt_tokens"] + d["completion_tokens"] for d in days]
    per_token = [h / t for h, t in zip(drawn, totals)]
    assert all(abs(x - per_token[-1]) <= 1e-9 * per_token[-1] for x in per_token), \
        f"stack heights not in proportion to the day's tokens: {list(zip(totals, drawn))}"
    nodes = box.locator('[data-chart="nodes"]')
    expect(nodes.locator("polyline")).to_have_count(2)
    expect(nodes.locator("g.day")).to_have_count(4)  # only the days with node samples
    rows = box.locator("[data-history-rows] tr")
    expect(rows).to_have_count(len(days))
    expect(rows.first.locator("th")).to_have_text(last["date"])
    box.locator("summary").click()
    expect(rows.first).to_be_visible()

    page.set_viewport_size({"width": 390, "height": 844})
    page.wait_for_timeout(400)  # redrawn at the new width
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways on a phone"
    vb = req.locator("svg").get_attribute("viewBox").split()
    assert 240 <= float(vb[2]) <= 390, f"chart not drawn at its width on a phone: {vb}"


@pytest.mark.parametrize("status,body", [
    (404, '{"error": "not found"}'),
    (500, "oops"),
    (200, "not json"),
    (200, "{}"),
    (200, '{"days": []}'),
    (200, json.dumps({"days": fixture_days(1)})),  # one day is not a history
    (200, json.dumps({"days": [{"date": "2026-10-01", "requests": -1, "prompt_tokens": "x", "completion_tokens": None},
                               {"date": "yesterday", "requests": 5, "prompt_tokens": 1, "completion_tokens": 1}]})),
    (200, json.dumps({"days": [{"date": "2026-10-01", "requests": 0, "completed": 0, "prompt_tokens": 0, "completion_tokens": 0},
                               {"date": "2026-10-02", "requests": 0, "completed": 0, "prompt_tokens": 0, "completion_tokens": 0}]})),
], ids=["404", "500", "garbage", "no-days", "empty", "one-day", "invalid-values", "all-zero"])
def test_history_hidden_without_data(site_pages, dist_url, status, body):
    """Missing, broken or too little data hides the whole section: no empty charts, dashes,
    zeros or error text."""
    page = history_page(site_pages, dist_url, body, status)
    expect(page.locator("[data-history]")).to_be_hidden()
    text = page.locator("main").inner_text()
    for bad in ("NaN", "undefined", "Infinity"):
        assert bad not in text, f"the page shows {bad!r}"


def test_history_nodes_chart_hidden_without_samples(site_pages, dist_url):
    """Days from before the gateway sampled nodes draw the request and token charts but no
    nodes chart."""
    page = history_page(site_pages, dist_url, json.dumps({"days": fixture_days(6, classes_from=6, nodes_from=99)}))
    box = page.locator("[data-history]")
    expect(box.locator('[data-chart="requests"]')).to_be_visible()
    expect(box.locator('[data-chart="tokens"]')).to_be_visible()
    expect(box.locator('[data-chart="nodes"]')).to_be_hidden()
    for name in ("requests", "tokens"):  # no day has classes: both charts are all "before classes"
        expect(box.locator(f'[data-chart="{name}"] [data-legend-earlier]')).to_be_visible()


# ---- the Pressure section

PRESSURE = "**/v1/stats/pressure*"
HOURS, GAP = 48, range(20, 23)  # 48 hours with three missing (the gateway was down)


def g(avg: float, peak: int | None = None) -> dict:
    return {"avg": avg, "peak": math.ceil(avg) if peak is None else peak}


def runner(cpu: float = 0.4, refaults: float = 300, lean_wait_p95: float = 4, free: float = 28) -> dict:
    """The runners' part of a bucket as the gateway serves it (a 7.6 GB runner, one Lean slot)."""
    return {"reports": 60, "cpu": g(cpu, 0.95), "iowait": g(0.02, 0.1), "steal": g(0, 0), "mem_total_gb": 7.6,
            "mem_private_gb": g(1.3, 2.2), "page_cache_gb": {"avg": 5.6, "low": 5.0}, "refaults_s": g(refaults, 4000),
            "disk_read_mb_s": g(2, 60), "disk_write_mb_s": g(0.5, 9), "free_gb_low": free,
            "psi": {"cpu": g(0.1, 0.5), "memory": g(0.01, 0.2), "io": g(0.02, 0.3)},
            "lean": {"slots": 1, "busy": g(0.3, 1), "waiting": g(0.1, 2), "run_s": {"count": 90, "p50": 8, "p95": 30},
                     "wait_s": {"count": 40, "p50": 0, "p95": lean_wait_p95}},
            "sessions": g(2.5, 3), "spot_notices": 0}


def node(i: int = 0) -> dict:
    return {"requests": 45, "engine_timed": 40, "prefill_share": round(0.2 + 0.05 * math.sin(i / 4), 3),
            "prefill_tok_s": 900, "decode_tok_s": 31.5, "cache_hit": round(0.8 + 0.1 * math.cos(i / 6), 3)}


def bucket(start: str, i: int) -> dict:
    u = round(0.5 + 0.45 * math.sin(i / 5), 3)
    b = {"start": start, "samples": 360, "slots": g(4), "busy": g(round(4 * u, 2), 4), "utilisation": u,
         "queue": {"people": g(0, i % 3), "science": g(1, 4), "all": g(1, 4)}, "held": g(0), "holds": 0, "runners": g(2, 2),
         "science_slots": g(3), "open_tasks": g(6), "demand": g(2.5, 3),
         "wait_s": {"chat": {"count": 5, "p50": 0, "p95": 2}, "science": {"count": 40, "p50": 20, "p95": 60 + 10 * (i % 12)}},
         "first_token_s": {"chat": {"count": 5, "p50": 1, "p95": 3}, "science": {"count": 40, "p50": 8, "p95": 25}},
         "outcomes": {"ok": 45, "refused": i % 2}, "runner": runner(), "node": node(i)}
    if i % 5 == 0:  # a few hours with an agent request
        b["first_token_s"]["agent"] = {"count": 1, "p50": 9, "p95": 9}
    return b


def fixture(hours: int = HOURS, gap=GAP, last: dict | None = None, unit: str = "hour", rng: str = "7d") -> dict:
    step = 3600 if unit == "hour" else 86400
    now = int(time.time()) // step * step
    fmt = "%Y-%m-%dT%H:00Z" if unit == "hour" else "%Y-%m-%d"
    bs = [bucket(time.strftime(fmt, time.gmtime(now - (hours - 1 - i) * step)), i)
          for i in range(hours) if i not in gap]
    if last is None:
        last = {k: v for k, v in bs[-1].items() if k != "start"}
        last.update(utilisation=0.92, outcomes={"ok": 1000, "refused": 12},
                    wait_s={"chat": {"count": 100, "p50": 0, "p95": 1}, "science": {"count": 900, "p50": 30, "p95": 180}})
    return {"updated": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "range": rng, "bucket": unit,
            "sample_seconds": 10, "buckets": bs, "total": last, "last_24h": last, "now": {}}


def pressure_page(site_pages, dist_url, body: str | None, status: int = 200, width: int = 1200, query: str = ""):
    """The network page with /v1/stats/pressure answered by body ."""
    page = site_pages()
    page.set_viewport_size({"width": width, "height": 900})
    if body is not None:
        page.route(PRESSURE, lambda r: r.fulfill(status=status, content_type="application/json", body=body))
    with page.expect_response(lambda r: "/v1/stats/pressure" in r.url):
        page.goto(dist_url + "/network/" + query)
    page.wait_for_timeout(300)  # the script draws after the answer
    return page


def test_pressure_draws_the_hours(site_pages, dist_url):
    """With hours of data the section says what limits the network now, in words, and draws four charts on a gap-free
    hourly grid (a missing hour is a gap, not joined across): slots busy, the wait for a slot and the time to the first
    token (p95 per class, with a legend), and science's demand against its slots; a hover title per hour; the table
    lists the hours with data, newest first; on a phone nothing scrolls sideways and the charts are redrawn at their
    width."""
    page = pressure_page(site_pages, dist_url, json.dumps(fixture()))
    box = page.locator("[data-pressure]")
    expect(box).to_be_visible()
    expect(box.locator("h2")).to_have_text("Pressure")
    expect(box.locator("[data-pressure-line]")).to_have_text(
        "GPU slots are the limit now: GPU slots were 92% busy in the last 24 h; science waited p95 3 min for a slot; "
        "12 of 1,012 requests found no node to take them; runners were 40% busy (CPU), Lean checks waited p95 4 s for a slot.")
    expect(box.locator("[data-pressure-range]")).to_have_text("the last 48 hours")
    util = box.locator('[data-pchart="util"]')
    expect(util).to_be_visible()
    expect(util.locator("g.day")).to_have_count(HOURS)  # every hour of the grid, the missing ones too
    expect(util.locator("rect.bar")).to_have_count(HOURS - len(GAP))
    first = fixture()["buckets"][0]
    expect(util.locator("g.day").first.locator("title")).to_contain_text(f"{round(first['utilisation'] * 100)}% busy")
    expect(util.locator("g.day").nth(GAP.start).locator("title")).to_contain_text("no slots online")
    wait = box.locator('[data-pchart="wait"]')
    expect(wait).to_be_visible()
    expect(wait.locator(".legend li")).to_have_text(["Chat", "API and agents", "Science"])
    expect(wait.locator("polyline.s-science")).to_have_count(2)  # broken at the gap
    expect(wait.locator("polyline.s-agent")).to_have_count(0)  # no agent waited
    ttft = box.locator('[data-pchart="ttft"]')
    expect(ttft).to_be_visible()
    expect(ttft.locator("polyline.s-chat")).to_have_count(2)
    expect(ttft.locator("circle.point.s-agent")).not_to_have_count(0)  # lone hours are dots
    dem = box.locator('[data-pchart="demand"]')
    expect(dem).to_be_visible()
    expect(dem.locator("polyline")).to_have_count(4)  # demand and slots, each broken at the gap
    # Part 2: the runners' CPU and memory (shares, three lines), their Lean checks, the nodes' prefill and cache hits.
    run = box.locator('[data-pchart="runner"]')
    expect(run).to_be_visible()
    expect(run.locator(".legend li")).to_have_text(["CPU busy", "Memory used by programs", "Memory caching files (Lean's library)"])
    expect(run.locator("polyline")).to_have_count(6)
    expect(run.locator("text.tick").nth(2)).to_have_text("100%")
    expect(run.locator("g.day").first.locator("title")).to_contain_text("CPU 40% busy")
    expect(box.locator('[data-pchart="lean"] polyline')).to_have_count(4)
    nd = box.locator('[data-pchart="node"]')
    expect(nd).to_be_visible()
    expect(nd.locator("g.day").first.locator("title")).to_contain_text("of prompt tokens from the cache")
    expect(box.locator("[data-range]")).to_have_text(["24 h", "48 h", "7 days", "14 days", "30 days", "90 days"])
    expect(box.locator('[data-range="7d"]')).to_have_attribute("aria-pressed", "true")
    rows = box.locator("[data-pressure-rows] tr")
    expect(rows).to_have_count(HOURS - len(GAP))
    expect(rows.first.locator("th, td")).to_have_count(13)
    expect(rows.first.locator("td").nth(8)).to_have_text("40%")  # runner CPU
    box.locator("summary").click()
    expect(rows.first).to_be_visible()
    text = page.locator("main").inner_text()
    for bad in ("NaN", "undefined", "Infinity"):
        assert bad not in text, f"the page shows {bad!r}"

    page.set_viewport_size({"width": 390, "height": 844})
    page.wait_for_timeout(400)  # redrawn at the new width
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways on a phone"
    vb = util.locator("svg").get_attribute("viewBox").split()
    assert 240 <= float(vb[2]) <= 390, f"chart not drawn at its width on a phone: {vb}"


def test_pressure_one_hour_is_a_line_without_charts(site_pages, dist_url):
    """One hour of data: the line says it all; no chart, which needs two hours. An idle network says the slots are not
    the limit."""
    last = {k: v for k, v in bucket("", 0).items() if k not in ("start", "runner", "node")}  # no runner reported
    last.update(utilisation=0.2, wait_s={"chat": {"count": 3, "p50": 0, "p95": 0}}, outcomes={"ok": 3})
    page = pressure_page(site_pages, dist_url, json.dumps(fixture(hours=1, gap=(), last=last)))
    box = page.locator("[data-pressure]")
    expect(box.locator("[data-pressure-line]")).to_have_text(
        "GPU slots are not the limit now: they were 20% busy in the last 24 h; no request waited for one.")
    for name in ("util", "wait", "ttft", "demand", "runner", "lean", "node"):
        expect(box.locator(f'[data-pchart="{name}"]')).to_be_hidden()


@pytest.mark.parametrize("more,line", [
    ({"refaults": 4096, "lean_wait_p95": 45},
     "Runner memory and the runners' Lean slots are the limit now: GPU slots were 40% busy in the last 24 h; "
     "runners were 40% busy (CPU), re-read 16 MB/s from disk that memory could not keep (5.6 GB of files cached on average), "
     "Lean checks waited p95 45 s for a slot."),
    ({"cpu": 0.9}, "Runner CPU is the limit now: GPU slots were 40% busy in the last 24 h; runners were 90% busy (CPU), "
                   "Lean checks waited p95 4 s for a slot."),
    ({"free": 3}, "Runner disk is the limit now: GPU slots were 40% busy in the last 24 h; runners were 40% busy (CPU), "
                  "Lean checks waited p95 4 s for a slot, only 3 GB of disk free at the lowest."),
    ({}, "Neither GPU slots nor runners are the limit now: GPU slots were 40% busy in the last 24 h; runners were 40% busy (CPU), "
         "Lean checks waited p95 4 s for a slot."),
], ids=["memory", "cpu", "disk", "none"])
def test_pressure_runner_verdict(site_pages, dist_url, more, line):
    """The line names what limits the runners, at the runner profile's thresholds (refaults 2,000/s, CPU 85%, Lean
    wait p95 30 s, 5 GB free), next to the GPU slots."""
    last = {k: v for k, v in bucket("", 0).items() if k != "start"}
    last.update(utilisation=0.4, wait_s={}, outcomes={}, runner=runner(**more))
    page = pressure_page(site_pages, dist_url, json.dumps(fixture(last=last)))
    expect(page.locator("[data-pressure-line]")).to_have_text(line)


def test_pressure_range_picker(site_pages, dist_url):
    """The range buttons ask the gateway for that range: a daily range draws days (the note, the table's first column),
    the address keeps the choice (?range=30d) and a reload opens it; an older answer that arrives late does not replace
    a newer one."""
    asked = []

    def answer(route):
        rng = re.search(r"range=(\w+)", route.request.url).group(1)
        asked.append(rng)
        body = fixture(hours=30, gap=(), unit="day", rng="30d") if rng == "30d" else fixture(hours=24, gap=(), rng=rng)
        route.fulfill(status=200, content_type="application/json", body=json.dumps(body))

    page = site_pages()
    page.set_viewport_size({"width": 1200, "height": 900})
    page.route(PRESSURE, answer)
    page.goto(dist_url + "/network/")
    box = page.locator("[data-pressure]")
    expect(box).to_be_visible()
    expect(box.locator("[data-pressure-range]")).to_have_text("the last 24 hours")
    with page.expect_response(lambda r: "range=30d" in r.url):
        box.locator('[data-range="30d"]').click()
    expect(box.locator('[data-range="30d"]')).to_have_attribute("aria-pressed", "true")
    expect(box.locator('[data-range="7d"]')).to_have_attribute("aria-pressed", "false")
    expect(box.locator("[data-pressure-unit]")).to_have_text("day")
    expect(box.locator("[data-pressure-range]")).to_have_text("the last 30 days")
    expect(box.locator("[data-pressure-th]")).to_have_text("Day (UTC)")
    expect(box.locator('[data-pchart="util"] g.day')).to_have_count(30)
    assert page.url.endswith("/network/?range=30d"), page.url
    page.reload()
    expect(box.locator("[data-pressure-range]")).to_have_text("the last 30 days")
    assert asked == ["7d", "30d", "30d"], asked
    with page.expect_response(lambda r: "range=48h" in r.url):
        box.locator('[data-range="48h"]').click()
    expect(box.locator("[data-pressure-unit]")).to_have_text("hour")
    with page.expect_response(lambda r: "range=7d" in r.url):
        box.locator('[data-range="7d"]').click()
    assert page.url.endswith("/network/"), page.url  # the default range needs no ?range=
    # A picked range that fails goes back to the one shown: the section and its buttons stay.
    page.unroute(PRESSURE)
    page.route(PRESSURE, lambda r: answer(r) if "range=7d" in r.request.url else r.fulfill(status=500, body="oops"))
    with page.expect_response(lambda r: "range=90d" in r.url):
        box.locator('[data-range="90d"]').click()
    expect(box).to_be_visible()
    expect(box.locator('[data-range="7d"]')).to_have_attribute("aria-pressed", "true")
    assert page.url.endswith("/network/"), page.url


@pytest.mark.parametrize("status,body", [
    (404, '{"error": "not found"}'),
    (500, "oops"),
    (200, "not json"),
    (200, "{}"),
    (200, json.dumps({**fixture(), "last_24h": {**fixture()["last_24h"], "samples": 0}})),
    (200, json.dumps({**fixture(), "bucket": "week"})),
    (200, json.dumps({**fixture(), "last_24h": {"samples": 5, "slots": "many"}})),
], ids=["404", "500", "garbage", "empty", "no-samples", "bad-unit", "invalid-values"])
def test_pressure_hidden_without_data(site_pages, dist_url, status, body):
    """Missing, broken or empty data hides the whole section: no empty charts, dashes, zeros or error text."""
    page = pressure_page(site_pages, dist_url, body, status)
    expect(page.locator("[data-pressure]")).to_be_hidden()
    text = page.locator("main").inner_text()
    for bad in ("NaN", "undefined", "Infinity"):
        assert bad not in text, f"the page shows {bad!r}"
