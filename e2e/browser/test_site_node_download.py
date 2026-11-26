"""The start of a node in a browser: on the home page and /node/ the same block (site/src/components/NodeStart.astro),
the Linux one-liner first beside the Windows installer, with or without JavaScript; on /node/windows/ the download block
(site/src/components/NodeDownload.astro) with the Windows panel only; no sideways scroll on a phone; /node/'s picture of
the node's own page; the stop and remove table on /node/operate/. The site is the build in site/dist (CI builds it first; locally `cd site && npm ci && npm run build`).

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_site_node_download.py

SPDX-License-Identifier: Apache-2.0
"""
import functools
import os
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from playwright.sync_api import expect

SRC = Path(__file__).resolve().parents[2]
DIST = Path(os.environ.get("SITE_DIST", SRC / "site" / "dist"))
RELEASES = "https://github.com/aisathome/aisathome/releases/"
WINDOWS_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
LINUX_UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
ANDROID_UA = "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36"


class _Quiet(SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


@pytest.fixture(scope="module")
def site():
    if not (DIST / "node/index.html").exists():
        if os.environ.get("CI"):
            pytest.fail(f"no site build in {DIST}")
        pytest.skip(f"no site build in {DIST} (cd site && npm ci && npm run build)")
    srv = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(_Quiet, directory=str(DIST)))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


@pytest.fixture
def open_page(browser, site):
    opened = []

    def make(path="/node/", ua=WINDOWS_UA, js=True, width=1400):
        ctx = browser.new_context(viewport={"width": width, "height": 900}, user_agent=ua, java_script_enabled=js)
        ctx.set_default_timeout(15_000)
        opened.append(ctx)
        page = ctx.new_page()
        page.route("**/v1/**", lambda r: r.fulfill(status=500, body="no"))
        page.goto(site + path)
        return page

    yield make
    for c in opened:
        c.close()


def _panels(page):
    return page.locator('[data-os-panel="windows"]'), page.locator('[data-os-panel="linux"]')


ONE_LINER = "curl -fsSL https://aisathome.org/node.sh | sh"


@pytest.mark.parametrize("ua", [WINDOWS_UA, LINUX_UA, ANDROID_UA])
@pytest.mark.parametrize("js", [True, False])
def test_node_page_leads_with_the_one_liner_and_the_windows_button(open_page, ua, js):
    """/node/ opens with the same start as the home page (src/components/NodeStart.astro) for every visitor: the Linux
    one-liner, first and on the first screen, beside the Windows installer; the bundle command is not offered there."""
    page = open_page(ua=ua, js=js)
    start = page.locator("[data-node-start]")
    linux, win = start.locator("[data-node-start-linux]"), start.locator("[data-node-start-windows]")
    expect(linux.locator("pre")).to_contain_text(ONE_LINER)
    assert linux.locator("pre").bounding_box()["y"] < 900, "the one-liner is not on the first screen"
    expect(win).to_be_visible()
    button = win.get_by_role("link", name="Download for Windows")
    href = button.get_attribute("href")
    assert href.startswith(RELEASES) and href.endswith(".exe"), href
    expect(win).to_contain_text("Run anyway")
    expect(win).to_contain_text("not signed yet")
    expect(win.get_by_role("link", name="Check the download")).to_have_attribute("href", "/node/windows/#check-the-download")
    assert page.evaluate("""() => { const s = document.querySelector('[data-node-start]');
        const l = s.querySelector('[data-node-start-linux]'), w = s.querySelector('[data-node-start-windows]');
        return !!(l.compareDocumentPosition(w) & Node.DOCUMENT_POSITION_FOLLOWING); }"""), "Linux does not come first"
    expect(start).not_to_contain_text("sudo ./install.sh")
    expect(start.get_by_role("link", name="Read the script first")).to_have_attribute(
        "href", "https://github.com/aisathome/aisathome/blob/main/site/public/node.sh")
    expect(page.locator("[data-node-download]")).to_have_count(0)


def test_home_and_node_page_show_the_same_command(open_page):
    """One component for both pages: the command and the Windows button cannot differ."""
    texts = []
    for path in ("/", "/node/"):
        page = open_page(path=path)
        start = page.locator("[data-node-start]")
        texts.append((start.locator("[data-node-start-linux] pre").inner_text().strip(),
                      start.locator("[data-dl-windows]").get_attribute("href")))
    assert texts[0] == texts[1], texts
    assert ONE_LINER in texts[0][0]


@pytest.mark.parametrize("js", [True, False])
def test_windows_page_has_only_the_windows_panel(open_page, js):
    page = open_page(path="/node/windows/", ua=LINUX_UA, js=js)
    win, linux = _panels(page)
    expect(win).to_be_visible()
    expect(linux).to_have_count(0)
    button = win.get_by_role("link", name="Download for Windows")
    href = button.get_attribute("href")
    assert href.startswith(RELEASES) and href.endswith(".exe"), href
    expect(win).to_contain_text("Run anyway")
    expect(win).to_contain_text("speed test")  # the cards that fail are named before the download
    expect(page.locator("#check-the-download")).to_be_attached()
    expect(page.locator("main")).to_contain_text("Get-FileHash")


@pytest.mark.parametrize("path,ua,box", [("/node/", WINDOWS_UA, "[data-node-start]"), ("/node/", LINUX_UA, "[data-node-start]"),
                                         ("/node/windows/", WINDOWS_UA, "[data-node-download]")])
def test_phone_width_no_sideways_scroll(open_page, path, ua, box):
    page = open_page(path=path, ua=ua, width=360)
    expect(page.locator(box)).to_be_visible()
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth")
    b = page.locator(box).bounding_box()
    assert b["x"] >= 0 and b["x"] + b["width"] <= 360


def test_node_page_shows_the_node_page_with_its_controls(open_page):
    """/node/'s picture of the node's own page: the one for the theme, down to the v0.7.0 controls, and it loads."""
    page = open_page()
    light, dark = page.locator('img.node-shot-light[data-node-shot="controls"]'), page.locator('img.node-shot-dark[data-node-shot="controls"]')
    light.scroll_into_view_if_needed()
    expect(light).to_be_visible()
    expect(dark).to_be_hidden()
    alt = light.get_attribute("alt")
    assert "example data" in alt and "Stop and free the GPU" in alt and "Uninstall" in alt, alt
    page.wait_for_function("document.querySelector('img.node-shot-light[data-node-shot=controls]').naturalWidth > 0")


def test_stop_and_remove_live_on_the_operate_page(open_page):
    """The stop, start and remove table moved from /node/ to /node/operate/; the home page links it there."""
    page = open_page(path="/node/operate/")
    expect(page.locator("#stop-start-remove")).to_be_attached()
    table = page.locator("main table").first  # the first thing on the page
    for text in ("Stop and free the GPU", "aisathome-node-uninstall", "Stop engine", "Settings → Apps"):
        expect(table).to_contain_text(text)
    home = open_page(path="/")
    expect(home.get_by_role("link", name="Stop, remove or link a node")).to_have_attribute("href", "/node/operate/#stop-start-remove")
    node = open_page()
    expect(node.locator("#stop-remove-link-later")).to_have_count(0)
    expect(node.locator("main")).not_to_contain_text("uninstall.sh")


@pytest.mark.parametrize("js", [True, False])
def test_home_page_has_the_one_command_and_the_windows_button(open_page, js):
    """The home page's node block: the one-line start without an account, a link to the script's source, the Windows
    button to the latest release, and the hero's "Run a node" goes to it, with or without JavaScript."""
    page = open_page(path="/", js=js)
    block = page.locator("[data-home-node]")
    expect(block.locator("pre")).to_contain_text(ONE_LINER)
    expect(block.get_by_role("link", name="Read the script first")).to_have_attribute(
        "href", "https://github.com/aisathome/aisathome/blob/main/site/public/node.sh")
    expect(block.locator("[data-dl-windows]")).to_have_attribute("href", RELEASES + "latest/download/AIsAtHome-node-setup.exe")
    expect(block).to_contain_text("science only")
    expect(page.locator(".hero").get_by_role("link", name="Run a node")).to_have_attribute("href", "#run-a-node")


def test_home_page_phone_width_no_sideways_scroll(open_page):
    page = open_page(path="/", ua=LINUX_UA, width=360)
    expect(page.locator("[data-home-node]")).to_be_visible()
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth")


def test_home_page_shows_the_node_page_picture_for_its_theme(open_page):
    """One picture of the node's own page per theme: the light one in a light browser, and it loads."""
    page = open_page(path="/")
    light, dark = page.locator('.node-shot-light[data-node-shot="top"]'), page.locator('.node-shot-dark[data-node-shot="top"]')
    light.scroll_into_view_if_needed()
    expect(light).to_be_visible()
    expect(dark).to_be_hidden()
    assert "example data" in light.get_attribute("alt")
    page.wait_for_function("document.querySelector('.node-shot-light[data-node-shot=top]').naturalWidth > 0")


def test_home_page_links_say_where_they_go(open_page):
    """Link texts name their destination; the hero links the mission page; the node block's first step links the
    latest release; the footer's Docs opens the docs index and the footer has no subreddit link."""
    page = open_page(path="/")
    expect(page.locator(".hero a[data-home-why]")).to_have_attribute("href", "/about/")
    for name, href in [("prove2me trials: Qwen’s Lean proofs", "/science/prove2me/"), ("All science projects", "/science/"),
                       ("Every Tau Ceti pull request", "/science/tauceti/"), ("All dev log posts", "/devlog/")]:
        expect(page.get_by_role("link", name=name)).to_have_attribute("href", href)
    expect(page.locator("[data-home-node]").get_by_role("link", name="the latest release")).to_have_attribute(
        "href", RELEASES + "latest")
    footer = page.locator(".site-footer")
    expect(footer.get_by_role("link", name="Docs")).to_have_attribute("href", "/docs/")
    expect(footer.locator('a[href*="reddit.com"]')).to_have_count(0)
