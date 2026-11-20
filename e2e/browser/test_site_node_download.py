"""The download block on /node/ and /node/windows/ (site/src/components/NodeDownload.astro), in a browser: the Windows
button without JavaScript, the Linux command for a Linux visitor and the switch between the two, and no sideways scroll on
a phone. The site is the build in site/dist (CI builds it first; locally `cd site && npm ci && npm run build`).

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


@pytest.mark.parametrize("js", [True, False])
def test_windows_button_first(open_page, js):
    page = open_page(js=js)
    win, linux = _panels(page)
    expect(win).to_be_visible()
    expect(linux).to_be_hidden()
    button = page.get_by_role("link", name="Download for Windows")
    expect(button).to_be_visible()
    href = button.get_attribute("href")
    assert href.startswith(RELEASES) and href.endswith(".exe"), href
    expect(win).to_contain_text("Run anyway")
    expect(win).to_contain_text("not signed yet")
    # Without JavaScript the Linux link is a plain link to the Linux page.
    expect(win.get_by_role("link", name="Linux", exact=True)).to_have_attribute("href", "/node/install/")
    expect(win.get_by_role("link", name="Check the download")).to_have_attribute("href", "/node/windows/#check-the-download")
    expect(win).to_contain_text("speed test")  # the cards that fail are named before the download


def test_linux_visitor_sees_the_command_and_can_switch(open_page):
    page = open_page(ua=LINUX_UA)
    win, linux = _panels(page)
    expect(linux).to_be_visible()
    expect(win).to_be_hidden()
    expect(linux.locator("pre")).to_contain_text("sudo ./install.sh")
    expect(linux.locator("pre")).to_contain_text("sha256sum -c")
    linux.get_by_role("link", name="Windows", exact=True).click()
    expect(win).to_be_visible()
    expect(linux).to_be_hidden()
    assert page.url.endswith("/node/")  # switched in place, did not leave
    win.get_by_role("link", name="Linux", exact=True).click()
    expect(linux).to_be_visible()


def test_android_is_not_linux(open_page):
    win, linux = _panels(open_page(ua=ANDROID_UA, width=390))
    expect(win).to_be_visible()
    expect(linux).to_be_hidden()


def test_windows_page_has_only_the_windows_panel(open_page):
    page = open_page(path="/node/windows/", ua=LINUX_UA)
    win, linux = _panels(page)
    expect(win).to_be_visible()
    expect(linux).to_have_count(0)
    expect(page.locator("#check-the-download")).to_be_attached()
    expect(page.locator("main")).to_contain_text("Get-FileHash")


@pytest.mark.parametrize("path,ua", [("/node/", WINDOWS_UA), ("/node/", LINUX_UA), ("/node/windows/", WINDOWS_UA)])
def test_phone_width_no_sideways_scroll(open_page, path, ua):
    page = open_page(path=path, ua=ua, width=360)
    expect(page.locator("[data-node-download]")).to_be_visible()
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth")
    box = page.locator("[data-node-download]").bounding_box()
    assert box["x"] >= 0 and box["x"] + box["width"] <= 360
