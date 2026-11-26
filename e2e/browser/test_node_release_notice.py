"""The node's local page says when a newer release is out (node/localui, node/release), in a browser.

e2e/cmd/nodeui-harness serves the real page and agent, with the latest release given on its command line
(no gateway, no network). On Linux the notice offers the one-line install to copy; on Windows a link to the
installer's download page.

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q test_node_release_notice.py

SPDX-License-Identifier: Apache-2.0
"""
import os
import secrets
import subprocess

import pytest
from playwright.sync_api import expect

from test_node_setup import SRC, _free_port, _go


@pytest.fixture(scope="module")
def exe(tmp_path_factory):
    out = tmp_path_factory.mktemp("nodeui") / "nodeui-harness"
    subprocess.run([_go(), "build", "-o", str(out), "./e2e/cmd/nodeui-harness"], cwd=SRC, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"}, timeout=600)
    return out


@pytest.fixture
def start(exe, tmp_path):
    """start(*args) runs the harness and returns its base address; it stops with the test."""
    procs = []

    def run(*args: str) -> str:
        (tmp_path / "data").mkdir(exist_ok=True)
        pw = tmp_path / "ui-password"
        pw.write_text(secrets.token_urlsafe(18) + "\n")
        port = _free_port()
        p = subprocess.Popen([str(exe), "-ui-port", str(port), "-password-file", str(pw), "-dir", str(tmp_path / "data"), *args],
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        procs.append(p)
        line = p.stdout.readline()
        assert line.startswith("ready "), f"harness did not start: {line!r}"
        return f"http://127.0.0.1:{port}"

    yield run
    for p in procs:
        p.terminate()
        p.wait(timeout=10)


def open_page(browser, base):
    ctx = browser.new_context(base_url=base, viewport={"width": 1200, "height": 900})
    ctx.grant_permissions(["clipboard-read", "clipboard-write"], origin=base)
    ctx.set_default_timeout(15_000)
    p = ctx.new_page()
    p.goto("/")
    return ctx, p


def test_linux_notice_with_the_command_to_copy(browser, start):
    ctx, p = open_page(browser, start("-version", "v0.7.0", "-latest", "v0.8.0"))
    try:
        # No password needed: the notice is information, not a control.
        expect(p.locator("#update-panel")).to_be_visible()
        expect(p.locator("#update-text")).to_have_text("v0.8.0 is out (you run v0.7.0).")
        expect(p.locator("#update-notes")).to_have_attribute("href", "https://github.com/aisathome/aisathome/releases/tag/v0.8.0")
        link = p.locator("#update-link")
        expect(link).to_have_text("How to update")
        expect(link).to_have_attribute("href", "https://aisathome.org/node/update/")
        expect(p.locator("#update-cmd-text")).to_have_text("curl -fsSL https://aisathome.org/node.sh | sh")
        expect(p.locator("#update-cmd-note")).to_be_visible()
        p.click("#update-copy")
        expect(p.locator("#update-copy")).to_have_text("Copied")
        assert p.evaluate("navigator.clipboard.readText()") == "curl -fsSL https://aisathome.org/node.sh | sh"
    finally:
        ctx.close()


def test_windows_notice_links_to_the_installer(browser, start):
    ctx, p = open_page(browser, start("-windows", "-version", "v0.7.0", "-latest", "v0.8.0"))
    try:
        expect(p.locator("#update-text")).to_have_text("v0.8.0 is out (you run v0.7.0).")
        link = p.locator("#update-link")
        expect(link).to_have_text("Download the new installer")
        expect(link).to_have_attribute("href", "https://aisathome.org/node/windows/")
        expect(p.locator("#update-cmd")).to_be_hidden()
    finally:
        ctx.close()


@pytest.mark.parametrize("args", [("-version", "v0.8.0", "-latest", "v0.8.0"), ("-version", "v0.9.0", "-latest", "v0.8.0"),
                                  ("-version", "v0.7.0", "-latest", ""), ("-version", "dev", "-latest", "v0.8.0")],
                         ids=["up to date", "newer than the latest", "not known", "development build"])
def test_no_notice(browser, start, args):
    ctx, p = open_page(browser, start(*args))
    try:
        expect(p.locator("#state-word")).not_to_have_text("Loading")
        expect(p.locator("#update-panel")).to_be_hidden()
    finally:
        ctx.close()
