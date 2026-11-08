# SPDX-License-Identifier: Apache-2.0
"""The sandbox's only way out: a TCP forwarder to one host:port.

Runs in its own container (started by sandbox.ensure_egress) attached to the sandbox's internal Docker
network and to an ordinary bridge. The model's container has no route out; its /etc/hosts points the
endpoint's name at this container, so opencode connects here and TLS runs end to end with the real
endpoint (no certificate of ours, nothing decrypted). Every connection goes to the one target, whatever
the client asks for: a client that names another host still reaches only the endpoint, whose
certificate then does not match.

The target is resolved again for every connection, so a changed endpoint address is picked up without
a restart. Only counts go to the log (no addresses of the client's peers, no data). Standard library
only: it runs as `python3 -I` inside the sandbox image.

    python3 -I egress_forward.py --listen 0.0.0.0:443 --target api.example.org:443
"""
from __future__ import annotations

import argparse
import asyncio
import socket
import sys
import time

MAX_CONN = 64            # concurrent connections (two attempts need a handful)
CONNECT_TIMEOUT_S = 15
IDLE_TIMEOUT_S = 1800    # a stream may stay silent for minutes (a queue and a long prefill before the first chunk)


async def pipe(r: asyncio.StreamReader, w: asyncio.StreamWriter, stats: dict, key: str):
    try:
        while True:
            data = await asyncio.wait_for(r.read(65536), IDLE_TIMEOUT_S)
            if not data:
                break
            stats[key] += len(data)
            w.write(data)
            await w.drain()
    except (asyncio.TimeoutError, ConnectionError, OSError):
        pass
    finally:
        try:
            w.close()
        except Exception:
            pass


async def handle(cr: asyncio.StreamReader, cw: asyncio.StreamWriter, host: str, port: int,
                 sem: asyncio.Semaphore, counters: dict):
    if sem.locked():
        cw.close()
        counters["refused"] += 1
        return
    async with sem:
        t0 = time.monotonic()
        stats = {"up": 0, "down": 0}
        try:
            # a fresh lookup per connection: the endpoint's address may change
            ur, uw = await asyncio.wait_for(asyncio.open_connection(host, port), CONNECT_TIMEOUT_S)
        except (OSError, asyncio.TimeoutError) as e:
            counters["failed"] += 1
            print(f"connect to {host}:{port} failed: {type(e).__name__}", flush=True)
            cw.close()
            return
        counters["ok"] += 1
        await asyncio.gather(pipe(cr, uw, stats, "up"), pipe(ur, cw, stats, "down"))
        print(f"conn done: {stats['up']} B up, {stats['down']} B down, {time.monotonic() - t0:.0f} s "
              f"(ok {counters['ok']}, failed {counters['failed']}, refused {counters['refused']})", flush=True)


async def main_async(listen: str, target: str):
    lh, lp = listen.rsplit(":", 1)
    th, tp = target.rsplit(":", 1)
    sem = asyncio.Semaphore(MAX_CONN)
    counters = {"ok": 0, "failed": 0, "refused": 0}
    srv = await asyncio.start_server(lambda r, w: handle(r, w, th, int(tp), sem, counters),
                                     lh, int(lp), family=socket.AF_INET, reuse_address=True)
    print(f"egress forwarder: {listen} -> {target}", flush=True)
    async with srv:
        await srv.serve_forever()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--listen", default="0.0.0.0:443")
    ap.add_argument("--target", required=True, help="host:port, the only destination")
    a = ap.parse_args()
    try:
        asyncio.run(main_async(a.listen, a.target))
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
