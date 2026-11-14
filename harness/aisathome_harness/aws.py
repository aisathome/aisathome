# SPDX-License-Identifier: Apache-2.0
"""Optional AWS plugins for a stateless worker: an S3 store for checkpoints and evidence, and the watch for a
spot interruption notice. Nothing here is needed off AWS; the deployment's own settings (bucket, region,
roles, which runners watch) stay with the deployment and are passed in.

    S3Store(bucket, region, endpoint=None, creds=None)   checkpoint.py's store over S3: path-style requests
                                                          signed with SigV4 (standard library, no boto3, so it
                                                          runs under a system python3); single-part PUT
    Credentials(imds)                                     the first found: AWS_ACCESS_KEY_ID/_SECRET_ACCESS_KEY
                                                          [/AWS_SESSION_TOKEN] in the environment; with AWS_PROFILE
                                                          set, `aws configure export-credentials` (nothing printed);
                                                          else the instance role through IMDSv2. Refreshed 5
                                                          minutes before they expire; kept in memory, used only
                                                          in the signature.
    NoticeWatch(on_notice, base, every_s)                 polls IMDSv2 for spot/instance-action or the Auto Scaling
                                                          group's Terminated lifecycle state and calls
                                                          on_notice(why) once (worker.Worker's `watchers`:
                                                          lambda on: NoticeWatch(on))

A regional endpoint is needed on a host whose egress goes only to S3. Standard library only.
"""
from __future__ import annotations

import datetime as dt
import hashlib
import hmac
import json
import os
import shutil
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path

from .checkpoint import StoreError

IMDS = "http://169.254.169.254"
RETRIES, RETRY_WAIT_S = 3, 2.0


def imds_token(base: str = IMDS, timeout: float = 2) -> str:
    req = urllib.request.Request(base.rstrip("/") + "/latest/api/token", method="PUT",
                                 headers={"X-aws-ec2-metadata-token-ttl-seconds": "21600"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read().decode()


def imds_get(base: str, path: str, token: str, timeout: float = 2) -> bytes:
    req = urllib.request.Request(base.rstrip("/") + path, headers={"X-aws-ec2-metadata-token": token})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


def _expiry(s: str | None) -> float:
    if not s:
        return time.time() + 3600
    return dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


class Credentials:
    """The first credentials found (see the module's docstring), refreshed 5 minutes before they expire."""

    def __init__(self, imds: str = IMDS, env=None):
        self.imds, self.env = imds.rstrip("/"), env
        self._c, self._exp, self._lock = None, 0.0, threading.Lock()

    def get(self) -> tuple[str, str, str | None]:
        with self._lock:
            if self._c and time.time() < self._exp - 300:
                return self._c
            e = self.env if self.env is not None else os.environ
            if e.get("AWS_ACCESS_KEY_ID") and e.get("AWS_SECRET_ACCESS_KEY"):
                self._c, self._exp = (e["AWS_ACCESS_KEY_ID"], e["AWS_SECRET_ACCESS_KEY"], e.get("AWS_SESSION_TOKEN")), time.time() + 3600
                return self._c
            if e.get("AWS_PROFILE") and shutil.which("aws"):
                p = subprocess.run(["aws", "configure", "export-credentials", "--format", "process"],
                                   capture_output=True, text=True, timeout=60)
                if p.returncode == 0:
                    j = json.loads(p.stdout)
                    self._c = (j["AccessKeyId"], j["SecretAccessKey"], j.get("SessionToken"))
                    self._exp = _expiry(j.get("Expiration"))
                    return self._c
                raise StoreError("aws configure export-credentials failed (not logged in?)")
            tok = imds_token(self.imds)
            role = imds_get(self.imds, "/latest/meta-data/iam/security-credentials/", tok).decode().split()[0]
            j = json.loads(imds_get(self.imds, "/latest/meta-data/iam/security-credentials/" + role, tok))
            self._c = (j["AccessKeyId"], j["SecretAccessKey"], j.get("Token"))
            self._exp = _expiry(j.get("Expiration"))
            return self._c


def _q(s: str, safe: str = "-_.~") -> str:
    return urllib.parse.quote(s, safe=safe)


def _s3_error(data: bytes) -> str:
    try:
        r = ET.fromstring(data)
        code = r.findtext("Code") or ""
        return (code + " " + (r.findtext("Message") or "")).strip()[:200]
    except ET.ParseError:
        return ""


class S3Store:
    """Path-style S3 requests signed with SigV4 (single-part PUT: a checkpoint of a few hundred MB at most)."""

    def __init__(self, bucket: str, region: str, endpoint: str | None = None, creds: Credentials | None = None,
                 timeout: float = 60, retries: int = RETRIES, retry_wait_s: float = RETRY_WAIT_S):
        self.bucket, self.region = bucket, region
        self.endpoint = (endpoint or f"https://s3.{region}.amazonaws.com").rstrip("/")
        self.host = urllib.parse.urlsplit(self.endpoint).netloc
        self.creds, self.timeout = creds or Credentials(), timeout
        self.retries, self.retry_wait_s = retries, retry_wait_s

    def _request(self, method: str, key: str = "", query: dict | None = None, body: bytes = b"",
                 headers: dict | None = None, out: Path | None = None) -> bytes:
        path = "/" + self.bucket + ("/" + _q(key, safe="-_.~/") if key else "")
        qs = "&".join(f"{_q(k)}={_q(v)}" for k, v in sorted((query or {}).items()))
        err = None
        for i in range(self.retries):
            ak, sk, tok = self.creds.get()
            now = dt.datetime.now(dt.timezone.utc)
            amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
            h = {"host": self.host, "x-amz-date": amz, "x-amz-content-sha256": hashlib.sha256(body).hexdigest(),
                 **{k.lower(): v for k, v in (headers or {}).items()}}
            if tok:
                h["x-amz-security-token"] = tok
            names = sorted(h)
            canon = "\n".join([method, path, qs, "".join(f"{k}:{str(h[k]).strip()}\n" for k in names), ";".join(names),
                               h["x-amz-content-sha256"]])
            scope = f"{day}/{self.region}/s3/aws4_request"
            sts = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canon.encode()).hexdigest()])
            k = ("AWS4" + sk).encode()
            for part in (day, self.region, "s3", "aws4_request"):
                k = hmac.new(k, part.encode(), hashlib.sha256).digest()
            sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
            h["authorization"] = f"AWS4-HMAC-SHA256 Credential={ak}/{scope}, SignedHeaders={';'.join(names)}, Signature={sig}"
            req = urllib.request.Request(self.endpoint + path + ("?" + qs if qs else ""), data=body if method == "PUT" else None,
                                         method=method, headers={k: v for k, v in h.items() if k != "host"})
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as r:
                    if out is not None:
                        with open(out, "wb") as fh:
                            shutil.copyfileobj(r, fh, 1 << 20)
                        return b""
                    data = r.read()
                if b"<Error>" in data[:200] and method == "PUT":       # CopyObject can fail with a 200
                    raise StoreError(f"{method} {key}: {_s3_error(data)}")
                return data
            except urllib.error.HTTPError as e:
                msg = _s3_error(e.read() or b"")
                if e.code in (400, 401, 403, 404, 409, 412):
                    raise StoreError(f"{method} {key or self.bucket}: HTTP {e.code} {msg}") from None
                err = f"HTTP {e.code} {msg}"
            except (OSError, socket.timeout) as e:
                err = f"{type(e).__name__}: {e}"
            time.sleep(self.retry_wait_s * (i + 1))
        raise StoreError(f"{method} {key or self.bucket}: {err} (after {self.retries} tries)")

    def put(self, key: str, data: bytes):
        self._request("PUT", key, body=data)

    def put_file(self, key: str, path: Path):
        self.put(key, Path(path).read_bytes())

    def get(self, key: str) -> bytes:
        return self._request("GET", key)

    def get_file(self, key: str, path: Path):
        self._request("GET", key, out=Path(path))

    def copy(self, src: str, dst: str):
        self._request("PUT", dst, headers={"x-amz-copy-source": "/" + self.bucket + "/" + _q(src, safe="-_.~/")})

    def list(self, prefix: str) -> list[str]:
        keys, token = [], None
        while True:
            q = {"list-type": "2", "prefix": prefix, **({"continuation-token": token} if token else {})}
            root = ET.fromstring(self._request("GET", query=q))
            ns = {"s": root.tag.split("}")[0].strip("{")} if root.tag.startswith("{") else {}
            find = (lambda e, t: e.findall(f"s:{t}", ns)) if ns else (lambda e, t: e.findall(t))
            keys += [k.text for c in find(root, "Contents") for k in find(c, "Key")]
            trunc = find(root, "IsTruncated")
            nxt = find(root, "NextContinuationToken")
            if not (trunc and trunc[0].text == "true" and nxt):
                return keys
            token = nxt[0].text


class NoticeWatch(threading.Thread):
    """Polls IMDSv2 every `every_s` for a spot interruption notice (spot/instance-action) or the Auto Scaling
    group's termination (autoscaling/target-lifecycle-state = Terminated) and calls `on_notice(why)` once."""

    PATHS = (("/latest/meta-data/spot/instance-action", lambda b: b.strip() != b""),
             ("/latest/meta-data/autoscaling/target-lifecycle-state", lambda b: b.strip() == b"Terminated"))

    def __init__(self, on_notice, base: str = IMDS, every_s: float = 5, log=print):
        super().__init__(daemon=True, name="spot-notice")
        self.on_notice, self.base, self.every_s, self.log = on_notice, base, every_s, log
        self.done = threading.Event()
        self._tok, self._tok_at = None, 0.0
        self.fails = 0

    def check(self) -> str | None:
        if not self._tok or time.monotonic() - self._tok_at > 3600:
            self._tok, self._tok_at = imds_token(self.base), time.monotonic()
        for path, hit in self.PATHS:
            try:
                body = imds_get(self.base, path, self._tok)
            except urllib.error.HTTPError as e:
                if e.code == 401:
                    self._tok = None
                if e.code in (401, 404):
                    continue
                raise
            if hit(body):
                return f"{path.rsplit('/', 1)[-1]}: {body.decode(errors='replace').strip()[:200]}"
        return None

    def run(self):
        while not self.done.wait(self.every_s):
            try:
                why = self.check()
                self.fails = 0
            except OSError as e:
                self.fails += 1
                if self.fails in (1, 60):
                    self.log(f"spot notice: IMDS unreachable ({type(e).__name__}); still polling")
                continue
            if why:
                self.log(f"spot notice: {why}; giving the task back")
                self.on_notice(why)
                return

    def stop(self):
        self.done.set()
