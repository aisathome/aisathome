# SPDX-License-Identifier: Apache-2.0
"""checkpoint: a toy project's state saved to a directory store and restored on another machine (another root),
the hash check and the fall-back to `previous` after an upload cut between archive and manifest, which copy
wins (newer seq, incomplete local copy, force), parts that are cleared or left out, evidence sync and fetch; and
aws.NoticeWatch against a fake instance metadata service."""
from __future__ import annotations

import http.server
import json
import shutil
import tempfile
import threading
import time
import unittest
from pathlib import Path

from aisathome_harness import aws
from aisathome_harness import checkpoint as K


def machine(root: Path, task: str = "t1", removed: list | None = None) -> K.Layout:
    """A toy project's layout on one machine: data/ (its cache/ rebuilt, not shipped), home/, state/base (the
    rest of state/ is rebuilt; a restore clears it)."""
    return K.Layout(task=task, parts=[K.Part("data", root / "data" / task, exclude=("cache",)),
                                      K.Part("home", root / "home" / task),
                                      K.Part("state/base", root / "state" / task / "base", optional=True,
                                             clear=root / "state" / task)],
                    notes=root / "notes", needs=(root / "data" / task / "run.json", root / "home" / task),
                    work=str(root), remove=(lambda d: (removed.append(d), shutil.rmtree(d))) if removed is not None else None)


def fill(root: Path, task: str = "t1", n: int = 1):
    d = root / "data" / task
    (d / "cache").mkdir(parents=True, exist_ok=True)
    (d / "cache" / "big.bin").write_bytes(b"x" * 1000)
    (d / "run.json").write_text(json.dumps({"round": n}))
    (d / "link").unlink(missing_ok=True)
    (d / "link").symlink_to("/etc/hostname")              # kept as a link, never followed
    (root / "home" / task).mkdir(parents=True, exist_ok=True)
    (root / "home" / task / "notes.md").write_text(f"round {n}\n")
    (root / "state" / task / "base").mkdir(parents=True, exist_ok=True)
    (root / "state" / task / "base" / "HEAD").write_text("base\n")
    (root / "state" / task / "overlay").mkdir(parents=True, exist_ok=True)
    (root / "state" / task / "overlay" / "junk").write_text("rebuilt on demand\n")


@unittest.skipUnless(shutil.which("zstd") and shutil.which("tar"), "needs zstd and GNU tar")
class Checkpoints(unittest.TestCase):
    def setUp(self):
        self.d = Path(tempfile.mkdtemp(prefix="ckpt-"))
        self.addCleanup(shutil.rmtree, self.d, True)
        self.st = K.DirStore(self.d / "bucket")
        self.a, self.b = self.d / "a", self.d / "b"
        self.pre = "ckpt/x/t1/"
        self.logs: list[str] = []

    def save(self, why="round", n=1, root=None):
        root = root or self.a
        fill(root, n=n)
        return K.save(self.st, machine(root), self.pre, why, worker="host-a", meta={"trial": "x", "n": n},
                      log=self.logs.append)

    def test_a_checkpoint_restores_on_another_machine(self):
        m1 = self.save("round", 1)
        m2 = self.save("final", 2)
        self.assertEqual((m1["seq"], m2["seq"], m2["why"], m2["trial"], m2["n"], m2["worker"], m2["task"]),
                         (1, 2, "final", "x", 2, "host-a", "t1"))
        self.assertEqual(json.loads(self.st.get(self.pre + "previous.json"))["seq"], 1)
        self.assertEqual(K.remote_manifest(self.st, self.pre)["seq"], 2)
        self.assertIsNone(K.remote_manifest(self.st, "ckpt/x/none/"))
        self.assertEqual(K.local_seq(machine(self.a)), 2)
        after = []
        lb = machine(self.b)
        self.assertFalse(lb.complete())
        got = K.restore(self.st, lb, self.pre, after=lambda: after.append(1), log=self.logs.append)
        self.assertEqual((got["seq"], after), (2, [1]))
        self.assertTrue(lb.complete())
        self.assertEqual(json.loads((self.b / "data/t1/run.json").read_text()), {"round": 2})
        self.assertEqual((self.b / "home/t1/notes.md").read_text(), "round 2\n")
        self.assertEqual((self.b / "state/t1/base/HEAD").read_text(), "base\n")
        self.assertFalse((self.b / "data/t1/cache").exists())          # left out: rebuilt by after()
        self.assertTrue((self.b / "data/t1/link").is_symlink())
        self.assertEqual(str((self.b / "data/t1/link").readlink()), "/etc/hostname")
        self.assertEqual(K.local_seq(lb), 2)
        self.assertTrue(any("restored checkpoint 2 (final, from host-a)" in l for l in self.logs), self.logs)
        self.assertTrue(any("taken under" in l for l in self.logs), self.logs)    # another machine's work path

    def test_an_upload_cut_before_its_manifest_falls_back_to_previous(self):
        self.save("round", 1)
        self.save("round", 2)
        # the archive of `latest` replaced, its manifest not: the sha256 does not match
        (self.st.root / self.pre / "latest.tar.zst").write_bytes(b"cut")
        got = K.restore(self.st, machine(self.b), self.pre, log=self.logs.append)
        self.assertEqual(got["seq"], 1)
        self.assertEqual(json.loads((self.b / "data/t1/run.json").read_text()), {"round": 1})
        self.assertTrue(any("`latest` is not intact; restoring `previous`" in l for l in self.logs), self.logs)
        (self.st.root / self.pre / "previous.tar.zst").write_bytes(b"cut")
        with self.assertRaises(K.StoreError):
            K.restore(self.st, machine(self.d / "c"), self.pre, log=self.logs.append)
        self.assertFalse((self.d / "c" / "data").exists())             # nothing half-restored

    def test_two_cut_uploads_in_a_row_keep_the_last_intact_checkpoint(self):
        self.save("round", 1)
        real = self.st.put_file

        def cut(key, path):                       # the archive goes up, the worker dies before the manifest
            real(key, path)
            raise K.StoreError("cut")
        self.st.put_file = cut
        for n in (2, 3):
            with self.assertRaises(K.StoreError):
                self.save("round", n)
        self.st.put_file = real
        got = K.restore(self.st, machine(self.b), self.pre, log=self.logs.append)
        self.assertEqual(got["seq"], 1)
        self.assertEqual(json.loads((self.b / "data/t1/run.json").read_text()), {"round": 1})
        self.assertTrue(any("`latest` is not intact (an upload cut); `previous` stays" in l for l in self.logs),
                        self.logs)

    def test_which_copy_wins(self):
        self.save("round", 1)
        la = machine(self.a)
        (self.a / "data/t1/local.txt").write_text("after the upload\n")
        self.assertIsNone(K.restore(self.st, la, self.pre, log=self.logs.append))     # own copy, same seq: kept
        self.assertTrue((self.a / "data/t1/local.txt").exists())
        # incomplete here: restored although the seq is the same
        shutil.rmtree(self.a / "home" / "t1")
        self.assertEqual(K.restore(self.st, la, self.pre, log=self.logs.append)["seq"], 1)
        self.assertFalse((self.a / "data/t1/local.txt").exists())
        # another machine went on (a newer seq): the store's wins; force restores anyway
        m = json.loads(self.st.get(self.pre + "latest.json"))
        self.st.put(self.pre + "latest.json", json.dumps({**m, "seq": 9}).encode())
        self.assertEqual(K.restore(self.st, la, self.pre, log=self.logs.append)["seq"], 9)
        self.assertIsNone(K.restore(self.st, la, self.pre, log=self.logs.append))
        self.assertEqual(K.restore(self.st, la, self.pre, log=self.logs.append, force=True)["seq"], 9)
        self.assertIsNone(K.restore(self.st, la, "ckpt/x/none/"))

    def test_a_cleared_part_goes_with_the_projects_remover(self):
        self.save("round", 1)
        removed: list[Path] = []
        fill(self.b)                                                      # a stale copy with overlay junk
        (self.b / "home" / "t1").rename(self.b / "home" / "gone")          # incomplete: restored
        K.restore(self.st, machine(self.b, removed=removed), self.pre, log=self.logs.append)
        self.assertEqual(removed, [self.b / "state" / "t1"])
        self.assertFalse((self.b / "state/t1/overlay").exists())
        self.assertTrue((self.b / "state/t1/base/HEAD").exists())

    def test_nothing_to_save_and_a_checkpoint_without_a_part(self):
        with self.assertRaises(K.StoreError):
            K.save(self.st, machine(self.d / "empty"), self.pre, "round")
        self.save("round", 1)
        lay = machine(self.b)
        lay.parts.append(K.Part("extra", self.b / "extra"))             # required, not in the archive
        with self.assertRaises(K.StoreError):
            K.restore(self.st, lay, self.pre)

    def test_evidence_goes_up_changed_files_only_and_comes_back_checked(self):
        src = self.d / "raw" / "t1"
        (src / "sub").mkdir(parents=True)
        (src / "a.txt").write_text("a\n")
        (src / "sub" / "b.txt").write_text("b\n")
        ix = self.d / "notes" / "evidence" / "t1.json"
        meta = {"task": "t1", "date": "2026-11-14"}
        self.assertEqual(K.sync_evidence(self.st, src, "evidence/t1/", ix, meta), 2)
        self.assertEqual(self.st.calls[-1], ("put", "evidence/t1/_index.json"))   # the index last
        self.st.calls.clear()
        self.assertEqual(K.sync_evidence(self.st, src, "evidence/t1/", ix, meta), 0)
        self.assertEqual(self.st.calls, [])                                       # unchanged: nothing sent
        (src / "a.txt").write_text("a2\n")
        self.assertEqual(K.sync_evidence(self.st, src, "evidence/t1/", ix, meta), 1)
        ix.unlink()                                                               # another host: the store's index
        self.assertEqual(K.sync_evidence(self.st, src, "evidence/t1/", ix, {**meta, "date": "2026-11-15"}), 0)
        self.assertEqual(json.loads(self.st.get("evidence/t1/_index.json"))["date"], "2026-11-15")
        desk = self.d / "desk"
        d, n = K.fetch_evidence(self.st, "evidence/t1/", lambda i: desk / i["date"] / i["task"])
        self.assertEqual((d, n), (desk / "2026-11-15" / "t1", 2))
        self.assertEqual((d / "a.txt").read_text(), "a2\n")
        self.assertEqual(K.fetch_evidence(self.st, "evidence/t1/", lambda i: d)[1], 0)     # same files: none
        self.st.put("evidence/t1/sub/b.txt", b"changed while read\n")
        (d / "sub" / "b.txt").unlink()
        with self.assertRaises(K.StoreError):
            K.fetch_evidence(self.st, "evidence/t1/", lambda i: d)
        self.assertIsNone(K.fetch_evidence(self.st, "evidence/none/", lambda i: d))

    def test_the_directory_store_refuses_keys_outside_it(self):
        for bad in ("/etc/passwd", "a/../../b"):
            with self.assertRaises(K.StoreError):
                self.st.put(bad, b"x")


class FakeIMDS:
    """IMDSv2 on 127.0.0.1: a token, then 404 until `notice()`."""

    def __init__(self):
        self.action: bytes | None = None
        me = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_PUT(self):
                self.send_response(200 if self.path == "/latest/api/token" else 400)
                self.end_headers()
                self.wfile.write(b"tok")

            def do_GET(self):
                if self.headers.get("X-aws-ec2-metadata-token") != "tok":
                    self.send_response(401)
                    self.end_headers()
                    return
                if self.path == "/latest/meta-data/spot/instance-action" and me.action:
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(me.action)
                    return
                self.send_response(404)
                self.end_headers()

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class SpotNotice(unittest.TestCase):
    def test_the_notice_is_seen_once(self):
        imds = FakeIMDS()
        self.addCleanup(imds.close)
        got = []
        w = aws.NoticeWatch(got.append, base=imds.url, every_s=0.05, log=lambda m: None)
        w.start()
        time.sleep(0.3)
        self.assertEqual(got, [])
        imds.action = b'{"action": "terminate", "time": "2026-11-14T20:00:00Z"}'
        w.join(5)
        self.assertFalse(w.is_alive())
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0].startswith("instance-action: "))

    def test_an_unreachable_imds_keeps_polling(self):
        logs = []
        w = aws.NoticeWatch(lambda why: None, base="http://127.0.0.1:9", every_s=0.02, log=logs.append)
        w.start()
        time.sleep(0.3)
        w.stop()
        w.join(5)
        self.assertGreater(w.fails, 1)
        self.assertEqual(len(logs), 1)
        self.assertIn("IMDS unreachable", logs[0])


class FakeS3:
    """Path-style S3 on 127.0.0.1: PUT/GET objects, a ListObjectsV2 in pages of 2, and the first request fails
    with 500 once when `flaky`."""

    def __init__(self):
        self.objs: dict[str, bytes] = {}
        self.auth: list[str] = []
        self.flaky = False
        me = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _key(self):
                return self.path.split("?")[0].split("/", 2)[2] if self.path.count("/") >= 2 else ""

            def _fail_once(self):
                if me.flaky:
                    me.flaky = False
                    self.send_response(500)
                    self.end_headers()
                    return True
                return False

            def do_PUT(self):
                me.auth.append(self.headers.get("Authorization") or "")
                if self._fail_once():
                    return
                n = int(self.headers.get("Content-Length") or 0)
                src = self.headers.get("x-amz-copy-source")
                me.objs[self._key()] = me.objs[src.split("/", 2)[2]] if src else self.rfile.read(n)
                self.send_response(200)
                self.end_headers()

            def do_GET(self):
                me.auth.append(self.headers.get("Authorization") or "")
                if self._fail_once():
                    return
                if "list-type=2" in self.path:
                    import urllib.parse as up
                    q = dict(up.parse_qsl(self.path.split("?", 1)[1]))
                    keys = sorted(k for k in me.objs if k.startswith(q.get("prefix", "")))
                    start = int(q.get("continuation-token") or 0)
                    page, more = keys[start:start + 2], start + 2 < len(keys)
                    body = ("<ListBucketResult xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\">"
                            + "".join(f"<Contents><Key>{k}</Key></Contents>" for k in page)
                            + f"<IsTruncated>{'true' if more else 'false'}</IsTruncated>"
                            + (f"<NextContinuationToken>{start + 2}</NextContinuationToken>" if more else "")
                            + "</ListBucketResult>").encode()
                else:
                    body = me.objs.get(self._key())
                    if body is None:
                        self.send_response(404)
                        self.end_headers()
                        self.wfile.write(b"<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>")
                        return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class S3(unittest.TestCase):
    def test_signed_requests_pages_retries_and_refusals(self):
        s3 = FakeS3()
        self.addCleanup(s3.close)
        creds = aws.Credentials(env={"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret"})
        st = aws.S3Store("bucket", "eu-north-1", s3.url, creds=creds, retry_wait_s=0)
        for i in range(5):
            st.put(f"p/{i}", f"v{i}".encode())
        st.copy("p/0", "q/0")
        self.assertEqual(st.list("p/"), [f"p/{i}" for i in range(5)])          # three pages
        self.assertEqual(st.get("q/0"), b"v0")
        s3.flaky = True                                                         # a 500 is retried
        self.assertEqual(st.get("p/1"), b"v1")
        with self.assertRaises(K.StoreError):                                   # a 404 is not
            st.get("p/none")
        self.assertTrue(all(a.startswith("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") and "Signature=" in a
                            for a in s3.auth))
        self.assertFalse(any("secret" in a for a in s3.auth))


if __name__ == "__main__":
    unittest.main()
