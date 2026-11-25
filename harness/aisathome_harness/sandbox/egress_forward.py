# SPDX-License-Identifier: Apache-2.0
"""The sandbox's only way out: an HTTP forwarder to one endpoint, which holds the endpoint's API key.

Runs in its own container (started by sandbox.ensure_egress) attached to the sandbox's internal Docker
network and to an ordinary bridge. The model's container has no route out; its /etc/hosts points the
endpoint's name at this container, and opencode's base URL is plain HTTP to that name (sandbox.model_url).
For each connection the forwarder reads one request head, rewrites it and sends it on to the one endpoint,
over TLS with the endpoint's certificate checked (or plain HTTP when the endpoint itself is http://):

- the API key never enters the model's container: the forwarder reads it from its own file and sets
  `Authorization: Bearer <key>` itself; whatever Authorization or proxy header the client sent is dropped;
- the target is fixed: Host is set to the endpoint and a request in absolute form (`GET http://other/...`) is
  refused, so the key never goes to another host;
- only model calls pass: `POST <base>/chat/completions` and `GET <base>/models` (MODEL_API), the exact
  path with no query, fragment, encoding or dot segment. Everything else is refused before the endpoint is
  contacted: an aisathome gateway serves its task, campaign and project API under the same base path,
  and the key must not open it to code run in the sandbox;
- one request per connection: `Connection: close` on the request and on the response head (so the client
  never reuses a connection the forwarder closes); hop-by-hop headers and `Expect` are dropped; the rest
  of the request (its body) and of the response are passed through as they come (streams stay streams).

With `--hold-preempted`, a preemption is a pause: after a response that carries an aisathome gateway's
preemption (a person's request took the node: `"code":"preempted"` in the HTTP 503 body of a request that had
not started streaming, `"reason":"preempted"` in the error event that ends one that had: PREEMPT_MARKS), the
next requests wait before they are sent on, 5 s after the first preemption in a row, doubling up to 60 s
(`Hold`); a response without one ends the pause. The client's retry then meets a node that has had time to
free up instead of being preempted again at once. Off by default.

The target is resolved again for every connection, so a changed endpoint address is picked up without a
restart. Only counts go to the log (no addresses of the client's peers, no headers, no data, never the
key). Standard library only: it runs as `python3 -I` inside the sandbox image, which needs CA certificates
for an https endpoint.

    python3 -I egress_forward.py --listen 0.0.0.0:80 --target api.example.org:443 --scheme https \\
        --path-prefix /v1 --key-file /run/egress/key
"""
from __future__ import annotations

import argparse
import asyncio
import os
import re
import socket
import ssl
import sys
import time

MAX_CONN = 64            # concurrent connections (two attempts need a handful)
CONNECT_TIMEOUT_S = 15
HEAD_TIMEOUT_S = 120     # the client sends its request head at once
IDLE_TIMEOUT_S = 1800    # a stream may stay silent for minutes (a queue and a long prefill before the first chunk)
HEAD_LIMIT = 65536
# dropped from the client's request: credentials, proxy headers, and hop-by-hop headers (RFC 9110 7.6.1)
DROP = {"authorization", "proxy-authorization", "proxy-authenticate", "proxy-connection", "host", "connection",
        "keep-alive", "upgrade", "te", "trailer", "expect"}
CA_BUNDLES = ("/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt")
TOKEN = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+")
VISIBLE = re.compile(r"[\x21-\x7e]+")       # a request target: visible ASCII only
CTL = re.compile(r"[\x00-\x08\x0a-\x1f\x7f]")  # control bytes a header value must not hold (TAB may)


# An aisathome gateway's preemption (gateway/preempt.go): `"code":"preempted"` in the 503 body of a request that
# had not started streaming (preemptedError), `"reason":"preempted"` in the error event that ends one that had
# (relay.preempted). Both are JSON structure: a model's text or tool arguments inside a stream are JSON strings,
# whose quotes are escaped, so they cannot hold either mark.
PREEMPT_MARKS = (b'"code":"preempted"', b'"reason":"preempted"')


class Hold:
    """The pause after preemptions (`--hold-preempted`): after the n-th preempted response in a row, a request
    waits until `base_s` x 2^(n-1) seconds (at most `max_s`) have passed since the last preemption."""

    def __init__(self, base_s: float = 5, max_s: float = 60, clock=time.monotonic, sleep=asyncio.sleep):
        self.base_s, self.max_s, self.clock, self.sleep = base_s, max_s, clock, sleep
        self.n, self.last = 0, 0.0
        self.counters = {"held": 0, "held_s": 0.0, "preempted": 0}

    def delay(self) -> float:
        if not self.n:
            return 0.0
        return max(0.0, min(self.max_s, self.base_s * 2 ** (self.n - 1)) - (self.clock() - self.last))

    async def wait(self):
        d = self.delay()
        if d > 0:
            self.counters["held"] += 1
            self.counters["held_s"] += d
            await self.sleep(d)

    def preempted(self):
        self.n += 1
        self.last = self.clock()
        self.counters["preempted"] += 1

    def ok(self):
        self.n = 0


class Watch:
    """Whether a byte stream holds one of `marks`, also split across chunks. `on_seen()` (if given) runs when
    the first mark is fed, before the caller passes those bytes on."""

    def __init__(self, *marks: bytes, on_seen=None):
        self.marks, self.tail, self.seen, self.on_seen = marks, b"", False, on_seen
        self.keep = max(len(m) for m in marks) - 1

    def feed(self, data: bytes):
        if not self.seen:
            buf = self.tail + data
            self.seen = any(m in buf for m in self.marks)
            self.tail = buf[-self.keep:] if self.keep else b""
            if self.seen and self.on_seen:
                self.on_seen()


class Refused(Exception):
    def __init__(self, status: int, reason: str):
        super().__init__(reason)
        self.status, self.reason = status, reason


def read_key(path: str | None) -> str | None:
    """The API key from its file (stripped); a key that could break a header line is refused."""
    if not path:
        return None
    with open(path, encoding="utf-8") as f:
        key = f.read().strip()
    if not key or any(ord(c) < 0x21 or ord(c) >= 0x7f for c in key):
        raise SystemExit("egress forwarder: the key file is empty or holds whitespace, control or non-ASCII characters")
    return key


# The calls the model's client (opencode's @ai-sdk/openai-compatible provider) makes, under the endpoint's
# base path, with their methods. The forwarder adds the key to these and to nothing else.
MODEL_API = {"chat/completions": "POST", "models": "GET"}


def path_allowed(method: str, target: str, prefix: str) -> bool:
    """`target` (the request target as sent) is exactly one of MODEL_API's paths under the base path
    `prefix`, asked with its method. Compared as raw text, so a query, fragment, percent-encoding, dot
    segment, doubled or trailing slash makes it another path, which is refused."""
    base = "/" + prefix.strip("/")
    for sub, m in MODEL_API.items():
        if target == base.rstrip("/") + "/" + sub:
            return method == m
    return False


def rewrite(head: bytes, host_header: str, prefix: str, key: str | None) -> bytes:
    """The request head (without its final blank line) as it goes to the endpoint, or Refused.
    Lines end in CRLF only: a bare CR or LF anywhere is refused, so is any byte in the method or target
    outside visible ASCII (a TAB or LF there would let a lenient endpoint split off a second request,
    to another path, with the key), and any control byte other than TAB in a header value."""
    lines = head.decode("latin-1").split("\r\n")
    if any("\r" in ln or "\n" in ln for ln in lines):
        raise Refused(400, "bare CR or LF in the request head")
    parts = lines[0].split(" ")
    if (len(parts) != 3 or not TOKEN.fullmatch(parts[0]) or not VISIBLE.fullmatch(parts[1])
            or parts[2] not in ("HTTP/1.1", "HTTP/1.0")):
        raise Refused(400, "bad request line")
    method, target, version = parts
    if method == "CONNECT" or not target.startswith("/"):
        raise Refused(400, "only origin-form requests to the endpoint")
    if not path_allowed(method, target, prefix):
        raise Refused(403, "not a model call (only POST chat/completions and GET models pass)")
    headers: list[tuple[str, str]] = []
    for ln in lines[1:]:
        if ln[:1] in (" ", "\t") or ":" not in ln:
            raise Refused(400, "bad header line")
        name, value = ln.split(":", 1)
        if not TOKEN.fullmatch(name) or CTL.search(value):
            raise Refused(400, "bad header")
        headers.append((name, value.strip(" \t")))   # OWS only (str.strip would also take \x85, \xa0)
    named = {t.strip().lower() for n, v in headers if n.lower() == "connection" for t in v.split(",")}
    out = [f"{method} {target} {version}", f"Host: {host_header}"]
    out += [f"{n}: {v}" for n, v in headers if n.lower() not in DROP and n.lower() not in named]
    if key:
        out.append(f"Authorization: Bearer {key}")
    out.append("Connection: close")
    return ("\r\n".join(out) + "\r\n\r\n").encode("latin-1")


def response_head(head: bytes) -> bytes:
    """The endpoint's response head (with its final blank line) as the client gets it: `Connection: close`,
    no keep-alive. An interim (1xx) head passes unchanged."""
    lines = head[:-4].split(b"\r\n")
    if lines[0][9:10] == b"1":
        return head
    keep = [ln for ln in lines[1:] if ln.split(b":", 1)[0].strip().lower() not in (b"connection", b"keep-alive")]
    return b"\r\n".join([lines[0], *keep, b"Connection: close"]) + b"\r\n\r\n"


async def respond(ur: asyncio.StreamReader, cw: asyncio.StreamWriter, stats: dict, watch: Watch | None = None):
    """The response: its head rewritten (response_head), then the rest as it comes (`watch` sees it all)."""
    try:
        head = await asyncio.wait_for(ur.readuntil(b"\r\n\r\n"), IDLE_TIMEOUT_S)
        head = response_head(head)
    except asyncio.IncompleteReadError as e:
        head = e.partial
    except (asyncio.LimitOverrunError, asyncio.TimeoutError, ConnectionError, OSError, ssl.SSLError):
        cw.close()
        return
    stats["down"] += len(head)
    if watch:
        watch.feed(head)
    cw.write(head)
    await pipe(ur, cw, stats, "down", watch)


def tls_context(cafile: str | None = None) -> ssl.SSLContext:
    """A client context that checks the endpoint's certificate and name: `cafile`, or the image's CAs."""
    ctx = ssl.create_default_context(cafile=cafile)
    if not cafile and not ctx.cert_store_stats().get("x509_ca"):
        for b in CA_BUNDLES:
            if os.path.isfile(b):
                ctx.load_verify_locations(cafile=b)
                break
        else:
            capath = ssl.get_default_verify_paths().capath
            if not (capath and os.path.isdir(capath) and os.listdir(capath)):
                raise SystemExit("egress forwarder: no CA certificates in this image (install ca-certificates)")
    return ctx


async def pipe(r: asyncio.StreamReader, w: asyncio.StreamWriter, stats: dict, key: str, watch: Watch | None = None):
    try:
        while True:
            data = await asyncio.wait_for(r.read(65536), IDLE_TIMEOUT_S)
            if not data:
                break
            stats[key] += len(data)
            if watch:
                watch.feed(data)
            w.write(data)
            await w.drain()
    except (asyncio.TimeoutError, ConnectionError, OSError, ssl.SSLError):
        pass
    finally:
        try:
            w.close()
        except Exception:
            pass


def _answer(cw: asyncio.StreamWriter, status: int, reason: str):
    body = f"egress forwarder: {reason}\n".encode()
    phrase = {400: "Bad Request", 403: "Forbidden", 431: "Request Header Fields Too Large",
              502: "Bad Gateway"}.get(status, "Error")
    cw.write(f"HTTP/1.1 {status} {phrase}\r\nContent-Type: text/plain\r\nContent-Length: {len(body)}\r\n"
             "Connection: close\r\n\r\n".encode() + body)
    cw.close()


class Forwarder:
    """One endpoint: `scheme`://`host`:`port`, its base path `prefix`, the key it adds (or None)."""

    def __init__(self, host: str, port: int, scheme: str = "https", prefix: str = "/", key: str | None = None,
                 cafile: str | None = None, hold: Hold | None = None):
        self.host, self.port, self.prefix, self.key = host, port, prefix, key
        self.hold = hold
        self.ssl = tls_context(cafile) if scheme == "https" else None
        default = 443 if scheme == "https" else 80
        self.host_header = host if port == default else f"{host}:{port}"
        self.sem = asyncio.Semaphore(MAX_CONN)
        self.counters = {"ok": 0, "failed": 0, "refused": 0}

    async def handle(self, cr: asyncio.StreamReader, cw: asyncio.StreamWriter):
        if self.sem.locked():
            cw.close()
            self.counters["refused"] += 1
            return
        async with self.sem:
            t0 = time.monotonic()
            stats = {"up": 0, "down": 0}
            try:
                head = await asyncio.wait_for(cr.readuntil(b"\r\n\r\n"), HEAD_TIMEOUT_S)
                out = rewrite(head[:-4], self.host_header, self.prefix, self.key)
            except Refused as e:
                self.counters["refused"] += 1
                print(f"request refused: {e.status} {e.reason}", flush=True)
                _answer(cw, e.status, e.reason)
                return
            except asyncio.LimitOverrunError:
                self.counters["refused"] += 1
                _answer(cw, 431, "request head too large")
                return
            except (asyncio.IncompleteReadError, asyncio.TimeoutError, ConnectionError, OSError):
                cw.close()
                return
            if self.hold:
                await self.hold.wait()
            try:
                # a fresh lookup per connection: the endpoint's address may change
                ur, uw = await asyncio.wait_for(asyncio.open_connection(
                    self.host, self.port, ssl=self.ssl, server_hostname=self.host if self.ssl else None),
                    CONNECT_TIMEOUT_S)
            except (OSError, asyncio.TimeoutError, ssl.SSLError) as e:
                self.counters["failed"] += 1
                print(f"connect to {self.host}:{self.port} failed: {type(e).__name__}", flush=True)
                _answer(cw, 502, f"cannot reach the endpoint ({type(e).__name__})")
                return
            self.counters["ok"] += 1
            uw.write(out)
            # the hold starts as the preemption passes, before the client has it: its retry can come while
            # this connection is still closing
            watch = Watch(*PREEMPT_MARKS, on_seen=self.hold.preempted) if self.hold else None
            await asyncio.gather(pipe(cr, uw, stats, "up"), respond(ur, cw, stats, watch))
            if watch and not watch.seen and stats["down"]:
                self.hold.ok()
            c = self.counters
            h = (f", preempted {self.hold.counters['preempted']}, held {self.hold.counters['held']} "
                 f"({self.hold.counters['held_s']:.0f} s)") if self.hold else ""
            print(f"conn done: {stats['up'] + len(out)} B up, {stats['down']} B down, {time.monotonic() - t0:.0f} s "
                  f"(ok {c['ok']}, failed {c['failed']}, refused {c['refused']}{h})", flush=True)


async def main_async(listen: str, target: str, scheme: str = "https", prefix: str = "/",
                     key_file: str | None = None, cafile: str | None = None, hold_preempted: bool = False):
    lh, lp = listen.rsplit(":", 1)
    th, tp = target.rsplit(":", 1)
    fw = Forwarder(th, int(tp), scheme, prefix, read_key(key_file), cafile, Hold() if hold_preempted else None)
    srv = await asyncio.start_server(fw.handle, lh, int(lp), family=socket.AF_INET, reuse_address=True,
                                     limit=HEAD_LIMIT)
    print(f"egress forwarder: {listen} -> {scheme}://{target}{prefix} "
          f"({'adds the key' if fw.key else 'no key'})", flush=True)
    async with srv:
        await srv.serve_forever()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--listen", default="0.0.0.0:80")
    ap.add_argument("--target", required=True, help="host:port, the only destination")
    ap.add_argument("--scheme", choices=("https", "http"), default="https", help="how to reach the target")
    ap.add_argument("--path-prefix", default="/", help="the endpoint's base path; only its model calls pass (MODEL_API)")
    ap.add_argument("--key-file", help="the API key, sent as `Authorization: Bearer <key>`")
    ap.add_argument("--cafile", help="CA certificates for the target (default: the image's)")
    ap.add_argument("--hold-preempted", action="store_true",
                    help="after a preempted response, hold the next requests (5 s doubling to 60 s)")
    a = ap.parse_args()
    try:
        asyncio.run(main_async(a.listen, a.target, a.scheme, a.path_prefix, a.key_file, a.cafile, a.hold_preempted))
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
