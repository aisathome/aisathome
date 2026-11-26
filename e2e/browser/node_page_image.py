"""Pictures of a node's own page, light and dark: the top of it for the home page
(site/public/images/home/node-page-{light,dark}.webp), and down to its controls for /node/
(node-page-controls-{light,dark}.webp). The real local page (node/localui/assets) rendered in a browser with made-up
status data, as the page themes itself, signed in so the Linux controls show (Pause, Restart engine, Stop and free the
GPU, Uninstall). Nothing
comes from a real node: the id, numbers and events below are examples, and the version is the release the site offers
(site/src/data/node-release.ts). Run it again when the local page changes or after a release:

  cd e2e/browser && uv run --with-requirements requirements.txt --with pillow python node_page_image.py

SPDX-License-Identifier: Apache-2.0
"""
import io
import json
import mimetypes
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

from PIL import Image
from playwright.sync_api import sync_playwright

SRC = Path(__file__).resolve().parents[2]
ASSETS = SRC / "node" / "localui" / "assets"
OUT = SRC / "site" / "public" / "images" / "home"
SIZE = (1000, 700)  # the browser's view (the home page's picture is its top), then scaled to WIDTH
WIDTH = 640
MARGIN = 16  # below the controls (less than the gap to the next panel), in the browser's pixels
# The release the site offers, as the agent names itself ("aisathome-node/0.7.0").
VERSION = re.search(r"version: 'v([0-9.]+)'", (SRC / "site" / "src" / "data" / "node-release.ts").read_text()).group(1)


def status():
    now = datetime.now(timezone.utc)
    ago = lambda m: (now - timedelta(minutes=m)).isoformat()
    hours = lambda h, m: (h * 60 + m) * 60_000
    return {
        "state": "serving", "connected": True, "paused": False, "engine": "healthy",
        "node_id": "example-node", "gateway": "aisathome.org", "gateway_id": "example",
        "connected_since": ago(192), "started_at": ago(194), "agent": f"aisathome-node/{VERSION}", "protocol": 2,
        "models": [{"name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 1, "ctx": 196608}],
        "slots_in_use": 1, "slots_total": 1, "day": now.date().isoformat(),
        "today": {"requests": 214, "failed": 0, "prompt_tokens": 9_840_000, "completion_tokens": 281_000,
                  "cached_tokens": 9_100_000, "busy_ms": hours(2, 41)},
        "total": {"requests": 1630, "failed": 0, "prompt_tokens": 73_200_000, "completion_tokens": 2_050_000,
                  "cached_tokens": 68_000_000, "busy_ms": hours(21, 5)},
        "events": [{"time": ago(192), "kind": "connected", "text": "aisathome.org"},
                   {"time": ago(193), "kind": "engine_healthy"}, {"time": ago(194), "kind": "started"}],
        "can_restart": True, "can_control": True,
        "anonymous": {"state": "admitted", "checks_passed": 3, "checks_needed": 3},
        "gpu": {"gpus": [{"index": 0, "name": "NVIDIA GeForce RTX 4090", "mem_used_mib": 22310, "mem_total_mib": 24564,
                          "temp_c": 66, "util_pct": 97, "power_w": 402, "power_limit_w": 450}]},
        "controls": {"enabled": True, "signed_in": True, "restart_command": "sudo systemctl restart aisathome-llama",
                     "status_command": "systemctl status aisathome-node",
                     "password_hint": "sudo cat /etc/aisathome-node/ui-password", "engine_control": False,
                     "stop_command": "sudo systemctl stop aisathome-node aisathome-llama",
                     "start_command": "sudo systemctl start aisathome-llama aisathome-node",
                     "uninstall_command": "sudo aisathome-node-uninstall"},
        "account_url": "https://aisathome.org/account",
    }


def serve(route):
    path = route.request.url.split("node.local", 1)[1].split("?", 1)[0]
    if path == "/api/status":
        return route.fulfill(status=200, content_type="application/json", body=json.dumps(status()))
    f = ASSETS / ("index.html" if path == "/" else path.lstrip("/"))
    if not f.is_file() or ASSETS not in f.resolve().parents:
        return route.fulfill(status=404, body="")
    return route.fulfill(status=200, content_type=mimetypes.guess_type(f.name)[0] or "application/octet-stream", body=f.read_bytes())


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as p:
        browser = p.chromium.launch()
        for scheme in ("light", "dark"):
            ctx = browser.new_context(viewport={"width": SIZE[0], "height": SIZE[1]}, color_scheme=scheme)
            page = ctx.new_page()
            page.route("http://node.local/**", serve)
            page.goto("http://node.local/")
            page.wait_for_selector('#state[data-state="serving"]')
            page.add_style_tag(content="*, *::before { animation: none !important; transition: none !important; }")
            page.wait_for_selector("#node-ctl:not([hidden])")
            page.wait_for_timeout(300)
            box = page.locator('section[aria-labelledby="controls-h"]').bounding_box()
            full = Image.open(io.BytesIO(page.screenshot(full_page=True))).convert("RGB")
            ctx.close()
            for name, height in (("node-page", SIZE[1]), ("node-page-controls", round(box["y"] + box["height"] + MARGIN))):
                img = full.crop((0, 0, SIZE[0], height)).resize((WIDTH, round(WIDTH * height / SIZE[0])), Image.LANCZOS)
                out = OUT / f"{name}-{scheme}.webp"
                img.save(out, "WEBP", quality=80, method=6)
                print(out.relative_to(SRC), f"{img.width}x{img.height},", out.stat().st_size, "bytes")
        browser.close()


if __name__ == "__main__":
    main()
