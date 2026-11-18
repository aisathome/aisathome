"""The Windows node's setup page (node/localui, node/setupflow), in a browser.

No Windows, GPU or gateway needed: e2e/cmd/setup-harness serves the real page, setup steps and
agent against stand-ins (one RTX 4080, a small model over local TLS, an engine that answers the
speed test, a gateway that refuses every token but one with 401 "unknown node token"). The test
builds it from this checkout with Go.

  cd e2e/browser && uv run --with-requirements requirements.txt pytest -q

SPDX-License-Identifier: Apache-2.0
"""
import os
import secrets
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest
from playwright.sync_api import expect

SRC = Path(__file__).resolve().parents[2]
GOOD = "nt-n-0000e2e0.good-" + secrets.token_hex(8)
WRONG = "nt-n-0000e2e0.wrong-" + secrets.token_hex(8)


def _go() -> str:
    for g in (os.environ.get("GO"), shutil.which("go"), str(Path.home() / ".local/go/bin/go")):
        if g and Path(g).exists():
            return g
    if os.environ.get("CI"):
        pytest.fail("no Go to build the setup harness")
    pytest.skip("no Go to build the setup harness")


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def harness(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("setup")
    exe = tmp / "setup-harness"
    subprocess.run([_go(), "build", "-o", str(exe), "./e2e/cmd/setup-harness"], cwd=SRC, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"}, timeout=600)
    password = secrets.token_urlsafe(18)
    pw = tmp / "ui-password"
    pw.write_text(password + "\n")
    data = tmp / "data"
    data.mkdir()
    port = _free_port()
    proc = subprocess.Popen([str(exe), "-ui-port", str(port), "-password-file", str(pw), "-dir", str(data),
                             "-good-token", GOOD, "-model-mb", "24"],
                            stdout=subprocess.PIPE, stderr=open(tmp / "harness.log", "w"), text=True)
    line = proc.stdout.readline()
    assert line.startswith("ready "), f"harness did not start: {line!r}, see {tmp / 'harness.log'}"
    yield {"base": f"http://127.0.0.1:{port}", "password": password, "data": data}
    proc.terminate()
    proc.wait(timeout=10)


@pytest.fixture
def node_page(browser, harness):
    ctx = browser.new_context(base_url=harness["base"], viewport={"width": 1200, "height": 900})
    ctx.set_default_timeout(20_000)
    p = ctx.new_page()
    p.on("dialog", lambda d: d.accept())
    yield p
    ctx.close()


def test_setup_page_from_a_fresh_install_to_a_connected_node(node_page, harness):
    p = node_page
    # The installer's shortcut opens "/": a fresh node shows the setup page, locked.
    p.goto("/")
    expect(p).to_have_url(harness["base"] + "/setup")
    expect(p.locator("#signin")).to_be_visible()
    expect(p.locator("#steps")).to_be_hidden()
    expect(p.locator("#pw-hint")).to_contain_text("ui-password")

    p.fill("#password", "wrong-password-123")
    p.click("#login button[type=submit]")
    expect(p.locator("#login-msg")).to_contain_text("wrong password")
    time.sleep(1.1)  # the page waits a second after a wrong password
    p.fill("#password", harness["password"])
    p.click("#login button[type=submit]")
    expect(p.locator("#steps")).to_be_visible()
    expect(p.locator("#start")).to_be_disabled()
    expect(p.locator("#download")).to_be_disabled()

    # 1. The card: the stand-in RTX 4080 gets the standard tier.
    p.click("#check")
    expect(p.locator("#card-line")).to_contain_text("NVIDIA GeForce RTX 4080")
    expect(p.locator("#card-line")).to_contain_text("standard")
    expect(p.locator("#step-card")).to_have_attribute("data-done", "true")

    # 2. The download shows progress; paused, it keeps its part and continues from it.
    p.click("#download")
    expect(p.locator("#cancel-download")).to_be_visible()
    expect(p.locator("#download-line")).to_contain_text(" of ")
    p.click("#cancel-download")
    expect(p.locator("#download-msg")).to_contain_text("part is kept")
    expect(p.locator("#download")).to_have_text("Continue the download")
    assert (harness["data"] / "models/model.gguf.part").exists()
    p.click("#download")
    expect(p.locator("#download-line")).to_contain_text("Downloaded and checked", timeout=30_000)
    expect(p.locator("#step-download")).to_have_attribute("data-done", "true")

    # 3. The speed test loads the engine and measures.
    p.click("#speed")
    expect(p.locator("#speed-line")).to_contain_text("31.5 tokens/s", timeout=30_000)
    expect(p.locator("#step-speed")).to_have_attribute("data-done", "true")

    # 4. The token: a malformed one is refused on the page; a well-formed one the gateway
    # does not know starts the node, and the page shows the gateway's reason.
    p.fill("#token", "not-a-token")
    p.click("#start")
    expect(p.locator("#start-msg")).to_contain_text("not a node token")
    p.fill("#token", WRONG)
    p.click("#start")
    expect(p.locator("#running")).to_be_visible()
    expect(p.locator("#running-line")).to_contain_text("HTTP 401: unknown node token")
    # The right token from "Use another node token": the node connects.
    p.fill("#new-token", GOOD)
    p.click("#token-form button[type=submit]")
    expect(p.locator("#running-line")).to_contain_text("is connected", timeout=30_000)
    # The token never comes back to the page.
    p.reload()
    expect(p.locator("#running-line")).to_contain_text("is connected")
    html = p.content()
    assert GOOD.split(".")[1] not in html and WRONG.split(".")[1] not in html

    # The status page: no redirect any more; Stop engine frees the GPU and shows "stopped".
    p.goto("/")
    expect(p).to_have_url(harness["base"] + "/")
    expect(p.locator("#state-word")).to_have_text("Connected", timeout=30_000)
    expect(p.locator("#setup-link")).to_be_visible()
    expect(p.locator("#help-engine")).to_be_visible()
    expect(p.locator("#help-linux")).to_be_hidden()
    p.click("#engine-toggle")
    expect(p.locator("#state-line")).to_contain_text("Engine stopped")
    expect(p.locator("#state-word")).to_have_text("Disconnected")
    expect(p.locator("#state-line")).not_to_contain_text("does not answer its health check")
    expect(p.locator("#engine-toggle")).to_have_text("Start engine")
    p.click("#engine-toggle")
    expect(p.locator("#state-word")).to_have_text("Connected", timeout=30_000)
    expect(p.locator("#engine-toggle")).to_have_text("Stop engine")

    # "Run the setup again" stops the node; "/" is the setup page again until it is left.
    p.goto("/setup")
    p.click("#again")
    expect(p.locator("#steps")).to_be_visible()
    p.goto("/")
    expect(p).to_have_url(harness["base"] + "/setup")
    p.click("#leave")
    expect(p.locator("#running-line")).to_contain_text("is connected", timeout=30_000)
