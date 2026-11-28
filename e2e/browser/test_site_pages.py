"""The website's own pages in a browser, from the build in site/dist (conftest.py: dist_url, site_pages): the dev log
(list, posts, feed, the home page's section, the menu link), the privacy notice and the operator terms, the node docs
(the two tiers, the update page, the Windows page, /node/'s sections), the footer and the about page's links, the live
strip's tokens label (with a fixed /v1/stats), the menu at every width, and the network page's credit tiles with large
numbers (a fixed /v1/stats/credit). What the pages say is checked here, next to the pages, so a change of wording
changes its test in the same pull request.

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_pages.py

SPDX-License-Identifier: Apache-2.0
"""
import json
import re
import xml.etree.ElementTree as ET

import pytest
from conftest import DIST, SRC
from playwright.sync_api import expect

ASSESSMENT = "/devlog/2-cases-for-ai-inference-at-home-for-science/"
GRIDCOIN = "/devlog/1-bringing-ai-into-the-boinc-gridcoin-environment/"
TAUCETI = "/devlog/3-ais-at-home-contributions-to-tau-ceti/"
STOPPING = "/devlog/4-stopping-the-tau-ceti-experiments/"
REDDIT = "https://www.reddit.com/r/aisathome/"


def list_items(page):
    return [(li.locator("time").get_attribute("datetime"), li.locator("a").get_attribute("href"))
            for li in page.locator("[data-devlog-item]").all()]


# ---- the dev log

def test_devlog_lists_posts_newest_first(site_pages, dist_url):
    page = site_pages()
    page.goto(dist_url + "/devlog/")
    expect(page.locator("h1")).to_have_text("Dev log")
    items = list_items(page)
    hrefs = [h for _, h in items]
    assert {TAUCETI, ASSESSMENT, GRIDCOIN} <= set(hrefs), f"posts listed: {hrefs}"
    dates = [d for d, _ in items]
    assert dates == sorted(dates, reverse=True), f"not newest first: {dates}"
    labels = [li.locator("[data-devlog-label]").text_content().strip() for li in page.locator("[data-devlog-item]").all()]
    numbers = [int(x.removeprefix("Dev log #")) for x in labels]
    assert numbers == sorted(numbers, reverse=True) and {1, 2, 3} <= set(numbers), f"labels: {labels}"
    for li in page.locator("[data-devlog-item]").all():  # number, title, date and a one-line summary
        assert li.locator("p").text_content().strip(), "a post without a summary"


def test_devlog_post_renders_its_tables(site_pages, dist_url):
    page = site_pages()
    page.goto(dist_url + ASSESSMENT)
    expect(page.locator("h1")).to_have_text("Cases for AI inference at home for science")  # the number is not in the title
    expect(page.locator("[data-devlog-label]")).to_have_text("Dev log #2")
    tables = page.locator(".sl-markdown-content table")
    assert tables.count() >= 3, f"{tables.count()} tables"
    expect(tables.filter(has_text="Total")).to_contain_text("about $57")
    expect(page.locator(".devlog-meta time")).to_have_attribute("datetime", "2026-10-18")
    # The link to the previous post works.
    page.get_by_role("link", name="previous post").click()
    page.wait_for_url(re.compile(re.escape(GRIDCOIN) + "$"))
    expect(page.locator("h1")).to_have_text("AIs@home: Bringing AI into the BOINC/Gridcoin environment?")
    expect(page.locator("[data-devlog-label]")).to_have_text("Dev log #1")
    expect(page.locator(".sl-markdown-content")).not_to_contain_text("Originally posted")


def test_devlog_3_renders_its_tables_and_links(site_pages, dist_url):
    """Dev log #3 (2026-10-25): both tables, the PR links and the live-numbers link."""
    page = site_pages()
    page.goto(dist_url + TAUCETI)
    expect(page.locator("h1")).to_have_text("AIs@home's contributions to Tau Ceti")
    expect(page.locator("[data-devlog-label]")).to_have_text("Dev log #3")
    expect(page.locator(".devlog-meta time")).to_have_attribute("datetime", "2026-10-25")
    content = page.locator(".sl-markdown-content")
    expect(content.locator("h1")).to_have_count(0)  # the source's title line is the page title, not a heading
    tables = content.locator("table")
    expect(tables).to_have_count(2)
    expect(tables.first.locator("tr", has_text="Total")).to_contain_text("12")
    expect(tables.nth(1).locator("tr", has_text="#19781")).to_contain_text("21.1M")
    expect(content.get_by_role("link", name="#19779")).to_have_attribute(
        "href", "https://github.com/TauCetiProject/TauCeti/pull/19779")
    expect(content.get_by_role("link", name="live numbers")).to_have_attribute(
        "href", "https://aisathome.org/science/tauceti/")
    expect(content.get_by_role("link", name="prove2me")).to_have_attribute("href", "https://prove2.me/")


def test_devlog_4_renders_its_table_and_links(site_pages, dist_url):
    """Dev log #4 (2026-11-13): listed in the dev log (newest first, so after any later post), its title and date,
    the table's total of 28 merged, and its links to #3, the Tau Ceti numbers and the prove2me page, which this
    site has."""
    page = site_pages()
    page.goto(dist_url + "/devlog/")
    items = list_items(page)
    assert STOPPING in [p for _, p in items], f"#4 is not listed: {items}"
    assert [d for d, _ in items] == sorted((d for d, _ in items), reverse=True), f"not newest first: {items}"
    page.goto(dist_url + STOPPING)
    expect(page.locator("h1")).to_have_text("Stopping the Tau Ceti experiments")
    expect(page.locator("[data-devlog-label]")).to_have_text("Dev log #4")
    expect(page.locator(".devlog-meta time")).to_have_attribute("datetime", "2026-11-13")
    content = page.locator(".sl-markdown-content")
    expect(content.locator("h1")).to_have_count(0)
    expect(content.locator("table")).to_have_count(1)
    expect(content.locator("table tr", has_text="Total")).to_contain_text("28")
    expect(content.get_by_role("link", name="live numbers")).to_have_attribute("href", "https://aisathome.org/science/tauceti/")
    expect(content.locator("a[href='https://aisathome.org/devlog/3-ais-at-home-contributions-to-tau-ceti/']")).to_have_count(1)
    expect(content.locator("a[href='https://aisathome.org/science/prove2me/']")).to_have_count(1)
    expect(content.get_by_role("link", name="prove2me")).to_have_attribute("href", "https://prove2.me/")
    page.goto(dist_url + "/science/prove2me/")
    expect(page.locator("h1")).to_have_text("Science: prove2me")  # the page #4 links exists on this site


def test_devlog_feed_is_rss_and_the_list_page_names_it(site_pages, dist_url):
    """The feed is well-formed RSS 2.0 whose every item has a title, a date and a description, and the list page
    names it in its head. (That it lists exactly the list page's posts: site/scripts/check-dist.mjs.)"""
    root = ET.fromstring((DIST / "devlog/rss.xml").read_bytes())  # raises on malformed XML
    assert root.tag == "rss" and root.get("version") == "2.0"
    items = root.findall("./channel/item")
    assert items and all(i.findtext("title") and i.findtext("pubDate") and i.findtext("description") for i in items)
    page = site_pages()
    page.goto(dist_url + "/devlog/")
    assert page.locator('head link[rel="alternate"][type="application/rss+xml"]').get_attribute("href") == "/devlog/rss.xml"


def test_home_page_shows_the_newest_posts(site_pages, dist_url):
    """The home page's "Dev log" section (after the hero) lists the newest posts of the dev log in its order, as rows
    of number, date, title and summary; the title opens the post and "All dev log posts" opens the dev log. On a phone
    it is below the hero."""
    page = site_pages()
    page.goto(dist_url + "/devlog/")
    order = [h for _, h in list_items(page)]
    page.goto(dist_url + "/")
    section = page.locator("[data-home-devlog]")
    expect(section.locator("h2")).to_have_text("Dev log")
    rows = section.locator("[data-home-post]")
    shown = [r.locator("[data-home-post-link]").get_attribute("href") for r in rows.all()]
    assert shown == order[:3], f"home {shown} vs dev log {order}"
    newest = rows.first
    number = shown[0].split("/")[2].split("-")[0]
    expect(newest).to_contain_text(f"#{number}")
    assert newest.locator("time").get_attribute("datetime"), "no date"
    assert newest.locator("p").text_content().strip(), "no summary"
    hero = page.locator("h1#_top").bounding_box()
    assert section.bounding_box()["y"] > hero["y"] + hero["height"], "the section is not after the hero"
    newest.locator("[data-home-post-link]").click()
    page.wait_for_url(dist_url + shown[0])
    expect(page.locator("[data-devlog-label]")).to_have_text(f"Dev log #{number}")
    page.goto(dist_url + "/")
    section.get_by_role("link", name="All dev log posts").click()  # a link text that names its place
    page.wait_for_url(dist_url + "/devlog/")
    page.set_viewport_size({"width": 390, "height": 844})
    page.goto(dist_url + "/")
    box = page.locator("[data-home-devlog]").bounding_box()
    assert box["width"] > 300 and box["x"] + box["width"] <= 390, f"section on a phone: {box}"


def test_menu_links_to_the_devlog(site_pages, dist_url):
    page = site_pages()
    page.goto(dist_url + "/")
    page.locator("nav.top-links").get_by_role("link", name="Dev log").click()
    page.wait_for_url(dist_url + "/devlog/")
    expect(page.locator("h1")).to_have_text("Dev log")


def test_every_devlog_post_ends_with_discuss_on_reddit(site_pages, dist_url):
    page = site_pages()
    page.goto(dist_url + "/devlog/")
    posts = [h for _, h in list_items(page)]
    assert posts, "no posts"
    for path in posts:
        page.goto(dist_url + path)
        line = page.locator("[data-devlog-discuss]")
        expect(line).to_have_text("Discuss on r/aisathome")
        expect(line.get_by_role("link", name="r/aisathome")).to_have_attribute("href", REDDIT)
        # The last thing of the post, after its text.
        assert page.evaluate("document.querySelector('[data-devlog-discuss]').nextElementSibling === null"), \
            f"{path}: the line is not at the end of the post"


# ---- the privacy notice and the operator terms

def test_privacy_page_says_what_the_statistics_keep(site_pages, dist_url):
    """The privacy page describes the visitor statistics as tools/sitestats/doc.go does, including the referring
    domain per page."""
    page = site_pages()
    page.goto(dist_url + "/about/privacy/")
    main = page.locator("main")
    expect(main).to_contain_text("Visitor statistics (this website only).")
    expect(main).to_contain_text("never the full address), also per page")
    expect(main).to_contain_text("never on disk or in backups")
    expect(main).to_contain_text("visits on different days cannot be linked, not even by us")
    expect(main.get_by_role("link", name="tools/sitestats")).to_have_attribute(
        "href", "https://github.com/aisathome/aisathome/tree/main/tools/sitestats")


def test_privacy_page_says_what_email_notices_keep(site_pages, dist_url):
    """The privacy page describes the opt-in contact address as gateway/contact.go keeps it: the sign-in box's own
    label (the gateway's, quoted exactly), consent, deletion, the mail service and the notice cookie; the legal basis
    in its table."""
    box = "Email me about important changes and outages"
    gateway = "".join(p.read_text() for p in (SRC / "gateway").glob("*.go") if not p.name.endswith("_test.go"))
    assert box in gateway, "the sign-in box's label is not the gateway's any more: quote the new one"
    page = site_pages()
    page.goto(dist_url + "/about/privacy/")
    main = page.locator("main")
    expect(main).to_contain_text("your verified email address only if you ask for email notices")
    expect(main).to_contain_text("Email about important changes (only if you ask).")
    expect(main).to_contain_text(f"“{box}”")
    expect(main).to_contain_text("Amazon SES")
    expect(main).to_contain_text("the address is deleted at once")
    expect(main.get_by_role("row", name="Opt-in email about changes and outages")).to_contain_text("(a) your consent")
    expect(main).to_contain_text("aisathome_notice_…")


def test_privacy_page_names_the_controller_and_links_the_operator_terms(site_pages, dist_url):
    """The privacy notice: the controller line, "hardened, not confidential", the guard evidence it keeps, no
    overclaims, and its link to the operator terms lands on that section of the terms page."""
    page = site_pages()
    page.goto(dist_url + "/about/privacy/")
    main = page.locator("main")
    expect(main).to_contain_text("JFR Holding AS (org. no. 937 566 042), contact@aisathome.org.")
    expect(main).to_contain_text("Hardened, not confidential.")
    # Guard evidence: what is kept of a stopped or flagged answer, and for how long.
    expect(main).to_contain_text("never the conversation itself")
    expect(main).to_contain_text("at most 300 characters, with known secret formats removed) for 14 days")
    evidence = main.get_by_role("row", name=re.compile(r"^Evidence of a safety check"))
    expect(evidence).to_have_count(2)  # its legal basis and its retention
    expect(evidence.filter(has_text="legitimate interest")).to_contain_text(
        "protecting users from malicious nodes and telling a real attack from a false alarm")
    expect(evidence.filter(has_not_text="legitimate interest")).to_contain_text("14 days")
    expect(main).not_to_contain_text("Your aisathome.org account made from your BOINC account")
    # No overclaims, and never the shareable weak key as proof of ownership.
    expect(main).to_contain_text("never accept the weak account key as proof")
    expect(main).to_contain_text("with your full BOINC account key")
    expect(main).to_contain_text("but that is not a guarantee")
    expect(main).to_contain_text("A conversation can therefore reach more than one node.")
    expect(main).to_contain_text("The cache has a size limit but no time limit")
    expect(main).not_to_contain_text("only while the node computes the answer")
    page.locator("main").get_by_role("link", name="terms for operators").click()
    expect(page).to_have_url(re.compile(r"/about/terms/#6-requests-your-node-answers$"))
    expect(page.get_by_role("heading", name="6. Requests your node answers")).to_be_visible()
    expect(page.locator("main")).to_contain_text(  # no apostrophe: the site prints curly ones
        "only after you have accepted these terms; until then it does science work only")


# ---- the node docs

def test_update_page_reachable_from_node_docs(site_pages, dist_url):
    """"Update your node" is linked from the node pages' text, the docs index and the sidebar, and gives the short
    update command and the container route."""
    page = site_pages()
    for path in ("/node/operate/", "/node/install/", "/docs/"):
        page.goto(dist_url + path)
        link = page.locator(".sl-markdown-content").get_by_role("link", name="Update your node")
        expect(link.first).to_have_attribute("href", "/node/update/")
        expect(page.locator("nav.sidebar a[href='/node/update/']")).to_have_count(1)
    page.goto(dist_url + "/node/operate/")
    page.locator(".sl-markdown-content").get_by_role("link", name="Update your node").first.click()
    page.wait_for_url(dist_url + "/node/update/")
    expect(page.locator("h1")).to_have_text("Update your node")
    expect(page.locator("main")).to_contain_text("sudo ./install.sh --keep-token")
    expect(page.get_by_role("heading", name="Without systemd (containers)")).to_be_visible()


def test_node_docs_name_both_tiers(site_pages, dist_url):
    """Two tiers, two model files: the model settings page gives each tier's file, model name and free memory, says
    what the 16 GB file costs, and that a conversation stays on one model; the API page lists both names; the node page
    no longer asks 16 GB cards to run headless."""
    page = site_pages()
    page.goto(dist_url + "/node/presets/")
    content = page.locator(".sl-markdown-content")
    row = content.locator("tr", has_text="Standard").first
    for want in ("UD-Q3_K_XL", "qwen3.8-27b-q3", "81,920", "14,300 MiB free"):
        expect(row).to_contain_text(want)
    row = content.locator("tr", has_text="Big").first
    for want in ("UD-IQ4_XS", "qwen3.8-27b", "196,608", "18,500 MiB free"):
        expect(row).to_contain_text(want)
    expect(content).to_contain_text("HumanEval 92.7% against 96.3%")
    expect(content).to_contain_text("A conversation stays on one model.")
    expect(content).not_to_contain_text("15,200")
    page.goto(dist_url + "/use/api/")
    content = page.locator(".sl-markdown-content")
    for name in ("qwen3.8-27b", "qwen3.8-27b-q3"):
        expect(content.locator("td code", has_text=re.compile("^" + re.escape(name) + "$"))).to_have_count(1)
    expect(content).to_contain_text("A conversation stays on the file it started on")
    page.goto(dist_url + "/node/")
    content = page.locator(".sl-markdown-content")
    expect(content).to_contain_text("light desktop")
    expect(content).not_to_contain_text("headless")


WINDOWS = "/node/windows/"
GATEWAY_PATHS = ("/account", "/v1/", "/__login", "/stats/", "/link")  # served by the gateway, not the build


def test_windows_install_page(site_pages, dist_url):
    """"Install on Windows": the download button, the unsigned installer's warning, the setup steps, the hash check and
    the privacy line; in the sidebar and linked from the node pages, the docs index and the FAQ (which no longer says
    "not yet"); its links within the site resolve, anchors included; it fits a phone."""
    page = site_pages()
    page.goto(dist_url + WINDOWS)
    expect(page.locator("h1")).to_have_text("Install on Windows")
    content = page.locator(".sl-markdown-content")
    button = content.get_by_role("link", name="Download for Windows")
    expect(button).to_be_visible()
    assert re.fullmatch(r"https://github\.com/aisathome/aisathome/releases/(latest/download|download/v[^/]+)/AIsAtHome-node-setup(-v[^/]+)?\.exe",
                        button.get_attribute("href")), button.get_attribute("href")
    for want in ("Get-FileHash .\\AIsAtHome-node-setup*.exe", "not code-signed yet",
                 "More info → Run anyway", "http://127.0.0.1:7381/", "20 tokens/s", "Other gateway",
                 "Stop engine", "Collect diagnostics", "aisathome-node.exe\" diagnose",
                 "Settings → Apps → AIs@home node", "151 requests", "hardened, not confidential"):
        expect(content).to_contain_text(re.compile(r"\s+".join(map(re.escape, want.split()))))
    expect(content.locator("a[href='https://github.com/aisathome/aisathome/releases/latest']").first).to_be_visible()
    for name in ("What you need", "1. Download and run the installer", "2. Set up on the node’s page",
                 "The tray icon", "When something goes wrong", "Update and uninstall", "Tested so far",
                 "Check the download", "Privacy"):
        expect(content.get_by_role("heading", name=name, exact=True)).to_be_visible()
    expect(page.locator("nav.sidebar a[href='/node/windows/']")).to_have_attribute("aria-current", "page")

    # Every link within the site resolves; an anchor names an element on its page. (The gateway's pages, such as
    # /account, are not in the build.)
    hrefs = sorted({h for h in content.locator("a").evaluate_all("els => els.map(e => e.getAttribute('href'))")
                    if h and h.startswith("/") and not h.startswith(GATEWAY_PATHS)})
    assert hrefs, "no links within the site"
    for href in hrefs:
        path, _, frag = href.partition("#")
        r = page.request.get(dist_url + path, max_redirects=0)
        assert r.status == 200, f"{href}: HTTP {r.status}"
        if frag:
            assert re.search(rf'id="{re.escape(frag)}"', r.text()), f"{href}: no element with that id"

    # Linked from the node docs, the docs index and the FAQ.
    for path in ("/node/", "/node/binaries/", "/node/update/", "/docs/", "/about/faq/"):
        page.goto(dist_url + path)
        expect(page.locator(".sl-markdown-content a[href^='/node/windows/']:visible").first).to_be_visible()
    expect(page.locator(".sl-markdown-content")).not_to_contain_text("Windows: not yet")
    page.goto(dist_url + "/node/")
    page.locator("[data-node-start]").get_by_role("link", name="Windows install in detail").click()
    page.wait_for_url(dist_url + WINDOWS)

    page.set_viewport_size({"width": 390, "height": 844})
    page.goto(dist_url + WINDOWS)
    assert page.evaluate("document.documentElement.scrollWidth") <= 390, "the page scrolls sideways"
    box = content.locator("pre").first.bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 390, f"a code block on a phone: {box}"


def test_node_page_sections_and_the_update_notes(site_pages, dist_url):
    """/node/'s sections in their words (what is gone stays gone), the engine's name, the picture of the node's own
    page with its controls; the node software's JSON-LD on /node/ only; /node/update/ warns that the update turns MTP
    on. (The one-liner and the Windows button: test_site_node_download.py.)"""
    page = site_pages()
    page.goto(dist_url + "/")
    assert not any(n.get("@type") == "SoftwareApplication"
                   for n in json.loads(page.locator('script[type="application/ld+json"]').inner_text())["@graph"])
    page.goto(dist_url + "/node/")
    main = page.locator("main")
    for heading in ("What you need", "Your node’s own page", "Link it to an account", "How the node runs"):
        expect(main.get_by_role("heading", name=heading, exact=True)).to_be_visible()
    for gone in ("Stop, remove, link later", "Tested systems", "Three steps", "Answers are not stopped"):
        expect(main).not_to_contain_text(gone)
    expect(main).to_contain_text("aisathome-llama")
    shot = page.locator("img[data-node-shot='controls']:visible")
    expect(shot).to_have_count(1)
    assert "Uninstall" in shot.get_attribute("alt")
    ld = [json.loads(t) for t in page.locator('script[type="application/ld+json"]').all_inner_texts()]
    apps = [n for d in ld for n in d["@graph"] if n.get("@type") == "SoftwareApplication"]
    assert len(apps) == 1 and apps[0]["@id"].endswith("/node/#software"), apps
    assert apps[0]["releaseNotes"].endswith("/releases/tag/v" + apps[0]["softwareVersion"]), apps[0]
    page.goto(dist_url + "/node/update/")
    expect(page.locator("main")).to_contain_text("turns MTP on")
    expect(page.locator("main")).to_contain_text("--speculation off")


# ---- the footer and the about page

def test_footer_docs_and_no_subreddit(site_pages, dist_url):
    """The site footer (home, docs, dev log): Docs opens the docs index; no subreddit link. The dev log posts and the
    about page still name it."""
    page = site_pages()
    for path in ("/", "/docs/", "/devlog/"):
        page.goto(dist_url + path)
        footer = page.locator(".site-footer")
        expect(footer.get_by_role("link", name="Docs")).to_have_attribute("href", "/docs/")
        expect(footer.locator('a[href*="reddit.com"]')).to_have_count(0)


def test_about_page_links_to_the_subreddit(site_pages, dist_url):
    page = site_pages()
    page.goto(dist_url + "/about/")
    item = page.locator(".sl-markdown-content li", has_text="Discuss:")
    expect(item).to_have_count(1)
    expect(item.get_by_role("link", name="r/aisathome")).to_have_attribute("href", REDDIT)


def test_about_page_links_issues_not_discussions(site_pages, dist_url):
    """Discussions is off on the repository (its link was a 404); questions and bug reports go to the Issues."""
    page = site_pages()
    page.goto(dist_url + "/about/")
    item = page.locator(".sl-markdown-content li", has_text="Questions, ideas and bug reports")
    expect(item.get_by_role("link")).to_have_attribute("href", "https://github.com/aisathome/aisathome/issues")
    expect(page.locator('a[href*="/discussions"]')).to_have_count(0)


# ---- the live strip (a fixed /v1/stats)

STATS = {"updated": "2026-11-13T16:10:23Z", "nodes": {"online": 2}, "total": {"completed": 11_000, "tokens": 5_250_000}}
TOKENS_LABEL = "Tokens processed at home"
TOKENS_NOTE = "on consumer GPUs with open models, not OpenAI, Anthropic or Google"


def _live_strip(page, url):
    page.route("**/v1/stats", lambda r: r.fulfill(status=200, content_type="application/json", body=json.dumps(STATS)))
    page.goto(url)
    strip = page.locator("[data-live-stats]").first
    expect(strip).to_be_visible()  # hidden until the /v1/stats numbers arrive
    return strip


def test_live_tokens_stat_says_at_home(site_pages, dist_url):
    """The live strip's token count reads "Tokens processed at home", with one muted line under that stat only saying
    what "at home" means. The three numbers stay on one line on a desktop; on a phone the note wraps inside the screen.
    Home and network pages."""
    page = site_pages()
    for path in ("/", "/network/"):
        strip = _live_strip(page, dist_url + path)
        item = strip.locator(".live-item").filter(has=page.locator('[data-stat="tokens"]'))
        expect(item.locator('[data-stat="tokens"]')).to_have_text("5.25M")
        expect(item.locator("dt")).to_have_text(TOKENS_LABEL)
        note = strip.locator("[data-stat-note]")
        expect(note).to_have_count(1)  # under the tokens stat only
        expect(item.locator("[data-stat-note]")).to_have_text(TOKENS_NOTE)
        expect(note).to_be_visible()
        number = item.locator('[data-stat="tokens"]').bounding_box()
        label = item.locator("dt").bounding_box()
        below = note.bounding_box()
        assert number["y"] < label["y"] <= below["y"], f"{path}: number {number}, label {label}, note {below}"
        assert float(note.evaluate("e => parseFloat(getComputedStyle(e).fontSize)")) < \
            float(item.locator("dt").evaluate("e => parseFloat(getComputedStyle(e).fontSize)")), f"{path}: the note is not smaller than the label"
        tops = [round(b["y"]) for b in (d.bounding_box() for d in strip.locator("dd[data-stat]").all())]
        assert max(tops) - min(tops) <= 1, f"{path}: the numbers are not on one line: {tops}"
    page.set_viewport_size({"width": 390, "height": 844})
    for path in ("/", "/network/"):
        strip = _live_strip(page, dist_url + path)
        assert page.evaluate("document.documentElement.scrollWidth") <= 390, f"{path}: the page scrolls sideways"
        box = strip.locator("[data-stat-note]").bounding_box()
        assert box["x"] >= 0 and box["x"] + box["width"] <= 390, f"{path}: the note on a phone: {box}"


# ---- the menu at every width (site/src/components/Header.astro, Sidebar.astro, HeaderLinks.astro): a phone menu
# button on pages without a sidebar, the main links on top of the sidebar's menu, a Menu button between 50 and 72rem,
# the links in the header from 72rem. (The gateway's own pages have their own menu.)

MENU = ["Chat", "Account", "Docs", "Run a node", "Network", "Science", "Dev log", "About"]
MENU_PAGES = ["/", "/network/", "/docs/", "/science/prove2me/", "/devlog/"]
# The menu buttons: ours on pages without a sidebar, Starlight's (it opens the sidebar), the header's Menu (50 to 72rem).
BUTTONS = ["details.phone-menu > summary", "button.sl-menu-button", "header .right-group details.nav-drop > summary"]
# The main links wherever they are (Starlight's sidebar is a nav labelled "Main" too, so by class).
LINKS = "nav.top-links a, nav.nav-drop-pane a, nav.phone-menu-links a, nav.sidebar-main a"


def _visible_links(page) -> list[str]:
    return [a.inner_text().strip() for a in page.locator(LINKS).all() if a.is_visible()]


def _open_menu(page) -> list[str]:
    """Clicks the one visible menu button and returns the main links it shows, in order."""
    shown = [b for b in BUTTONS if page.locator(b).first.is_visible()]
    assert len(shown) == 1, f"menu buttons visible on {page.url}: {shown}"
    assert _visible_links(page) == [], f"main links visible before the menu opened on {page.url}"
    page.locator(shown[0]).first.click()
    page.wait_for_timeout(300)
    return _visible_links(page)


@pytest.mark.parametrize("scheme", ["dark", "light"])
@pytest.mark.parametrize("path", MENU_PAGES)
def test_a_phone_has_a_menu_on_every_page(site_pages, dist_url, path, scheme):
    """At 390 px every page has one menu button that opens all the main links, in the menu's order, in both themes; a
    link in it works; nothing scrolls sideways."""
    page = site_pages(width=390, height=844, scheme=scheme)
    page.goto(dist_url + path)
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"{path} scrolls sideways at 390 px"
    assert _open_menu(page) == MENU, f"{path}: the phone menu's links"
    page.locator(LINKS).filter(visible=True).filter(has_text="Dev log").first.click()
    page.wait_for_url(re.compile(r"/devlog/$"))


@pytest.mark.parametrize("path", MENU_PAGES)
def test_a_tablet_has_a_menu_on_every_page(site_pages, dist_url, path):
    """Between 50 and 72rem (820 px) the header's Menu opens the main links, and closes on Escape."""
    page = site_pages(width=820, height=844, scheme="dark")
    page.goto(dist_url + path)
    assert _open_menu(page) == MENU, f"{path}: the tablet menu's links"
    page.keyboard.press("Escape")
    expect(page.locator(LINKS).filter(visible=True)).to_have_count(0)


@pytest.mark.parametrize("path", MENU_PAGES)
def test_a_desktop_shows_the_links_in_the_header(site_pages, dist_url, path):
    """From 72rem the main links are in the header, with no menu button."""
    page = site_pages(width=1400, height=844, scheme="dark")
    page.goto(dist_url + path)
    assert _visible_links(page) == MENU, f"{path}: the header's links"
    assert not [b for b in BUTTONS if page.locator(b).first.is_visible()], f"{path}: a menu button on a desktop"


def test_the_menu_marks_the_current_section(site_pages, dist_url):
    """The phone menu marks the section the page is in (aria-current)."""
    page = site_pages(width=390, height=844, scheme="dark")
    page.goto(dist_url + "/network/")
    _open_menu(page)
    expect(page.locator(LINKS).filter(visible=True).and_(page.locator("[aria-current=page]"))).to_have_text("Network")


# ---- the network page's credit tiles with large numbers (a fixed /v1/stats/credit)

def _credit(total: float) -> str:
    return json.dumps({"updated": "2026-11-13T12:00:00Z", "nodes": {"online": 1, "with_credit": 12},
                       "total": {"credit": total, "today": 48211.3, "recent_average": 0.5},
                       "volunteers": {"count": 34, "nodes": 1, "credit": 12.5, "today": 1.2, "recent_average": 0.5},
                       "anonymous": {"count": 0, "credit": 0, "today": 0}, "maintainer": {"nodes": 0, "credit": 0, "today": 0},
                       "top": [{"rank": 1, "name": "alice", "total_credit": 12.5, "today": 1.2, "recent_average": 0.5, "nodes": 1}]})


@pytest.mark.parametrize("width", [390, 820, 1400])
@pytest.mark.parametrize("total,shown", [(2259043, "2,259,043"), (1234567890123, "1,234,567,890,123")])
def test_credit_tiles_fit_large_numbers(site_pages, dist_url, width, total, shown):
    """A total of 2,259,043, and a far larger one, stay inside their tiles on a phone, a tablet and a desktop, in two
    or four columns (never three and one). Before the fix 2,259,043 spilled out at 820 and 1400 px, the larger total at
    every width."""
    page = site_pages(width=width, height=844, scheme="dark")
    page.route("**/v1/stats/credit", lambda r: r.fulfill(status=200, content_type="application/json", body=_credit(total)))
    page.goto(dist_url + "/network/")
    tiles = page.locator("[data-credit-board] .board-sums > div")
    expect(tiles.first.locator("dd")).to_have_text(shown)
    boxes = page.evaluate("""() => [...document.querySelectorAll('[data-credit-board] .board-sums > div')].map(d => {
        const dd = d.querySelector('dd'), r = d.getBoundingClientRect();
        const range = document.createRange(); range.selectNodeContents(dd); const w = range.getBoundingClientRect();
        return {left: r.left, top: r.top, right: r.right, textRight: w.right, textLeft: w.left, overflow: dd.scrollWidth > dd.clientWidth + 1};
    })""")
    for b in boxes:
        assert not b["overflow"] and b["textLeft"] >= b["left"] and b["textRight"] <= b["right"] + 0.5, f"a number spills out of its tile: {b}"
    rows = {}
    for b in boxes:
        rows.setdefault(round(b["top"]), []).append(b)
    assert sorted(len(r) for r in rows.values()) in ([2, 2], [4]), f"tiles per row: {[len(r) for r in rows.values()]}"
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "the page scrolls sideways"
