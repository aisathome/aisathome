"""The home page's picture of a node's own page (site/public/images/home/node-page-{light,dark}.webp): the real local
page (node/localui/assets) rendered in a browser with made-up status data, light and dark, as the page themes itself.
Nothing comes from a real node: the id, numbers and events below are examples. Run it again when the local page
changes:

  cd e2e/browser && uv run --with-requirements requirements.txt --with pillow python node_page_image.py

SPDX-License-Identifier: Apache-2.0
"""
import io
import json
import mimetypes
from datetime import datetime, timedelta, timezone
from pathlib import Path

from PIL import Image
from playwright.sync_api import sync_playwright

SRC = Path(__file__).resolve().parents[2]
ASSETS = SRC / "node" / "localui" / "assets"
OUT = SRC / "site" / "public" / "images" / "home"
SIZE = (1000, 700)  # the browser's view, then scaled to WIDTH
WIDTH = 640


def status():
    now = datetime.now(timezone.utc)
    ago = lambda m: (now - timedelta(minutes=m)).isoformat()
    hours = lambda h, m: (h * 60 + m) * 60_000
    return {
        "state": "serving", "connected": True, "paused": False, "engine": "healthy",
        "node_id": "example-node", "gateway": "aisathome.org", "gateway_id": "example",
        "connected_since": ago(192), "started_at": ago(194), "agent": "aisathome-node/0.6.0", "protocol": 2,
        "models": [{"name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 1, "ctx": 196608}],
        "slots_in_use": 1, "slots_total": 1, "day": now.date().isoformat(),
        "today": {"requests": 214, "failed": 0, "prompt_tokens": 9_840_000, "completion_tokens": 281_000,
                  "cached_tokens": 9_100_000, "busy_ms": hours(2, 41)},
        "total": {"requests": 1630, "failed": 0, "prompt_tokens": 73_200_000, "completion_tokens": 2_050_000,
                  "cached_tokens": 68_000_000, "busy_ms": hours(21, 5)},
        "events": [{"time": ago(192), "kind": "connected", "text": "aisathome.org"},
                   {"time": ago(193), "kind": "engine_healthy"}, {"time": ago(194), "kind": "started"}],
        "can_restart": True,
        "anonymous": {"state": "admitted", "checks_passed": 3, "checks_needed": 3},
        "gpu": {"gpus": [{"index": 0, "name": "NVIDIA GeForce RTX 4090", "mem_used_mib": 22310, "mem_total_mib": 24564,
                          "temp_c": 66, "util_pct": 97, "power_w": 402, "power_limit_w": 450}]},
        "controls": {"enabled": True, "signed_in": True, "restart_command": "sudo systemctl restart aisathome-llama",
                     "status_command": "systemctl status aisathome-node",
                     "password_hint": "sudo cat /etc/aisathome-node/ui-password", "engine_control": False},
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
            page.wait_for_timeout(300)
            png = page.screenshot(clip={"x": 0, "y": 0, "width": SIZE[0], "height": SIZE[1]})
            ctx.close()
            img = Image.open(io.BytesIO(png)).convert("RGB")
            img = img.resize((WIDTH, round(WIDTH * SIZE[1] / SIZE[0])), Image.LANCZOS)
            out = OUT / f"node-page-{scheme}.webp"
            img.save(out, "WEBP", quality=80, method=6)
            print(out.relative_to(SRC), out.stat().st_size, "bytes")
        browser.close()


if __name__ == "__main__":
    main()
