"""Fixtures for the browser tests: one headless Chromium for the session (HEADED=1 shows it), and for the website's
pages the build in site/dist served by a small static server (`dist_url`) with pages whose gateway fails unless a test
answers it (`site_pages`).

SPDX-License-Identifier: Apache-2.0
"""
import functools
import os
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
from playwright.sync_api import sync_playwright

SRC = Path(__file__).resolve().parents[2]
DIST = Path(os.environ.get("SITE_DIST", SRC / "site" / "dist"))


@pytest.fixture(scope="session")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch(headless=os.environ.get("HEADED") != "1")
        yield b
        b.close()


class _Quiet(SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass


@pytest.fixture(scope="session")
def dist_url():
    """The site build (site/dist) on a local port. CI builds it first (.github/workflows/ci.yml, job browser);
    locally: cd site && npm ci && P2M_RECORDS=tests/fixtures/prove2me-records-2026-11-18.json npm run build"""
    if not (DIST / "index.html").exists():
        if os.environ.get("CI"):
            pytest.fail(f"no site build in {DIST}")
        pytest.skip(f"no site build in {DIST} (cd site && npm ci && npm run build)")
    srv = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(_Quiet, directory=str(DIST)))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


@pytest.fixture
def site_pages(browser):
    """site_pages(width=1400, height=900, js=True, scheme=None) -> a new page in a context of its own. Every request
    to the gateway (/v1/...) fails with HTTP 500 and the project data documents (/data/...) with 404, so a page shows
    the build's copy; a test answers what it needs with page.route (the newest route wins)."""
    opened = []

    def make(width=1400, height=900, js=True, scheme=None):
        ctx = browser.new_context(viewport={"width": width, "height": height}, java_script_enabled=js,
                                  color_scheme=scheme)
        ctx.set_default_timeout(15_000)
        opened.append(ctx)
        page = ctx.new_page()
        page.route("**/v1/**", lambda r: r.fulfill(status=500, body="no"))
        page.route("**/data/**", lambda r: r.fulfill(status=404, body="no"))
        return page

    yield make
    for c in opened:
        c.close()
