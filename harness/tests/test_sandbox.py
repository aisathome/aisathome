# SPDX-License-Identifier: Apache-2.0
"""The sandbox with Docker stubbed: endpoints by name only, one forwarder per host and port, one internal
network per session, the container recipe (names, mounts, limits, the build-slot wrapper), and nothing the
host writes into the model's HOME except through safefs."""
from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import loop
from aisathome_harness.host import Host
from aisathome_harness.lean.toolchain import LeanToolchain
from aisathome_harness.sandbox import container, sandbox


class FakeDocker:
    """Enough of Docker's CLI for the sandbox: networks, containers, their attachments and labels."""

    def __init__(self):
        self.calls, self.nets, self.containers = [], {}, {}
        self.ip = 10

    def __call__(self, *args, check=True):
        self.calls.append(args)
        a = list(args)
        ok = lambda out="": mock.Mock(returncode=0, stdout=out, stderr="")
        missing = mock.Mock(returncode=1, stdout="", stderr="no such object")
        if a[:2] == ["network", "inspect"]:
            n = self.nets.get(a[2])
            if n is None:
                return missing
            if "{{.Internal}}" in a:
                return ok("true" if n["internal"] else "false")
            return ok(json.dumps({c: {"Name": c} for c in n["members"]}))
        if a[:2] == ["network", "create"]:
            self.nets[a[-1]] = {"internal": "--internal" in a, "members": set(),
                                "bridge": next(x.split("=", 1)[1] for x in a if x.startswith("com.docker.network.bridge.name="))}
            return ok()
        if a[:2] == ["network", "connect"]:
            self.nets[a[2]]["members"].add(a[3])
            self.containers[a[3]]["networks"][a[2]] = f"10.0.0.{self.next_ip()}"
            return ok()
        if a[:2] == ["network", "disconnect"]:
            self.nets[a[3]]["members"].discard(a[4])
            self.containers.get(a[4], {"networks": {}})["networks"].pop(a[3], None)
            return ok()
        if a[:2] == ["network", "rm"]:
            self.nets.pop(a[2], None)
            return ok()
        if a[0] == "inspect":
            c = self.containers.get(a[-1])
            if c is None:
                return missing
            return ok(f"{'true' if c['running'] else 'false'}\t{json.dumps({k: {'IPAddress': v} for k, v in c['networks'].items()})}"
                      f"\t{json.dumps(c['labels'])}")
        if a[0] == "rm":
            self.containers.pop(a[-1], None)
            return ok()
        if a[0] == "run":
            name = a[a.index("--name") + 1]
            labels = dict(x.split("=", 1) for x in (a[i + 1] for i, v in enumerate(a) if v == "--label"))
            net = a[a.index("--network") + 1]
            self.containers[name] = {"running": True, "labels": labels, "networks": {net: f"10.0.0.{self.next_ip()}"},
                                     "argv": a}
            self.nets.setdefault(net, {"internal": False, "members": set()})["members"].add(name)
            return ok()
        return ok()

    def next_ip(self):
        self.ip += 1
        return self.ip


class Endpoints(unittest.TestCase):
    def test_names_only(self):
        self.assertEqual(sandbox.target_of("https://api.example.org/v1"), ("api.example.org", 443))
        self.assertEqual(sandbox.target_of("http://gw.example:8080/v1"), ("gw.example", 8080))
        for bad in ("https://203.0.113.5/v1", "http://127.0.0.1:9/v1", "https://[2001:db8::1]:8443/v1",
                    "http://localhost:8000/v1"):
            with self.assertRaises(ValueError, msg=bad) as e:
                sandbox.target_of(bad)
            self.assertIn("host name", str(e.exception))
        with self.assertRaises(ValueError):
            sandbox.target_of("ftp://x")

    def test_one_forwarder_per_host_and_port(self):
        self.assertNotEqual(sandbox.forwarder_name("gw.example", 443), sandbox.forwarder_name("gw.example", 8443))


class Networks(unittest.TestCase):
    def setUp(self):
        self.d = FakeDocker()
        self.p = mock.patch.object(sandbox, "_docker", self.d)
        self.p.start()
        self.tmp = tempfile.TemporaryDirectory()
        self.lock = mock.patch.object(sandbox, "LOCK", Path(self.tmp.name) / "egress.lock")
        self.lock.start()

    def tearDown(self):
        self.p.stop()
        self.lock.stop()
        self.tmp.cleanup()

    def test_each_session_has_its_own_internal_network(self):
        a, b = sandbox.ensure_session_network("proj/A01"), sandbox.ensure_session_network("proj/A02")
        self.assertNotEqual(a, b)
        self.assertTrue(self.d.nets[a]["internal"] and self.d.nets[b]["internal"])
        bridges = [self.d.nets[n]["bridge"] for n in (a, b)]
        self.assertTrue(all(x.startswith(sandbox.BRIDGE_PREFIX) and len(x) <= 15 for x in bridges), bridges)
        self.assertEqual(len(set(bridges)), 2)

    def test_a_network_that_is_not_internal_is_refused(self):
        name, _ = sandbox.session_network("proj/A01")
        self.d.nets[name] = {"internal": False, "members": set()}
        with self.assertRaises(RuntimeError):
            sandbox.ensure_session_network("proj/A01")

    def test_forwarders_attach_only_to_their_sessions(self):
        na, nb = sandbox.ensure_session_network("p/A"), sandbox.ensure_session_network("p/B")
        ha = sandbox.ensure_egress("https://one.example/v1", "img", na)
        hb = sandbox.ensure_egress("https://two.example/v1", "img", nb)
        fa, fb = sandbox.forwarder_name("one.example", 443), sandbox.forwarder_name("two.example", 443)
        self.assertEqual(self.d.nets[na]["members"], {fa})
        self.assertEqual(self.d.nets[nb]["members"], {fb})
        self.assertEqual(ha, ["--add-host", f"one.example:{self.d.containers[fa]['networks'][na]}"])
        self.assertTrue(hb[1].startswith("two.example:"))
        # a second session on the same endpoint shares the forwarder, on its own network
        nc = sandbox.ensure_session_network("p/C")
        sandbox.ensure_egress("https://one.example/v1", "img", nc)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)
        self.assertIn(nc, self.d.containers[fa]["networks"])
        sandbox.remove_session_network("p/A")
        self.assertNotIn(na, self.d.nets)
        self.assertNotIn(na, self.d.containers[fa]["networks"])

    def test_same_host_other_port_gets_its_own_forwarder(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        sandbox.ensure_egress("https://gw.example:8443/v1", "img", n)
        f443, f8443 = (self.d.containers[sandbox.forwarder_name("gw.example", p)] for p in (443, 8443))
        self.assertIn("gw.example:443", f443["argv"])
        self.assertIn("gw.example:8443", f8443["argv"])
        self.assertIn("0.0.0.0:8443", f8443["argv"])

    def test_a_forwarder_running_for_another_target_is_replaced(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        name = sandbox.forwarder_name("gw.example", 443)
        self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.target"] = "elsewhere.example:443"
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)
        self.assertEqual(self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.target"], "gw.example:443")

    def test_a_forwarder_for_another_target_or_version_is_replaced(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        name = sandbox.forwarder_name("gw.example", 443)
        self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.version"] = "old"
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)


class Recipe(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        r = Path(self.tmp.name)
        self.r = r
        self.d = FakeDocker()
        self.patches = [mock.patch.object(sandbox, "_docker", self.d),
                        mock.patch.object(sandbox, "LOCK", r / "egress.lock"),
                        mock.patch.object(container.subprocess, "run", lambda *a, **k: mock.Mock(returncode=0, stdout="", stderr=""))]
        for p in self.patches:
            p.start()
        self.host = Host(state=r / "host", slots=3, cpus=4, memory_gb=6, pids=500, build_min_free_gb=3)
        self.elan = r / "elan"
        (self.elan / "bin").mkdir(parents=True)
        (r / "know").mkdir()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        self.tmp.cleanup()

    def sandbox(self, key: str, task: str) -> container.Sandbox:
        tree, home, state = self.r / task / "tree", self.r / task / "home", self.r / task / "state"
        for d in (tree / ".lake" / "packages", home):
            d.mkdir(parents=True, exist_ok=True)
        tc = LeanToolchain(self.elan, cache_dir=self.r / "cache", threads=3)
        recipe = container.Recipe(image="img:1", knowledge=self.r / "know", sinkhole=["code.example"]).merged(**tc.recipe_parts(tree))
        return container.Sandbox(recipe, self.host, key, tree, home, state, "https://api.example.org/v1",
                                 str(self.r / "key"), "m")

    def test_command(self):
        (self.r / "key").write_text("k")
        sb = self.sandbox("proj/A01", "A01")
        argv = sb.command("hello", "ses_1")
        s = " ".join(argv)
        self.assertEqual(argv[:6], ["docker", "run", "--rm", "--name", sb.name, "-u"])
        self.assertEqual(argv[-3:], ["--session", "ses_1", "hello"])
        for want in ("--cap-drop=ALL", "no-new-privileges", f"--network {sb.network}", "--memory 6g", "--pids-limit 500",
                     "--cpus 4", "--add-host code.example:127.0.0.9", "--add-host api.example.org:10.0.0.",
                     f"-v {sb.home}:/home/sbx", f"-v {sb.tree}:{sb.tree}", f"-v {self.elan}:{self.elan}:ro",
                     f"{sb.tree}/.lake/packages:{sb.tree}/.lake/packages:ro", f"{self.r / 'key'}:/run/harness/key:ro",
                     f"-v {self.host.slot_dir}:/run/harness/slots", "/run/harness/buildslot.sh:ro",
                     f"{self.r / 'know'}:/home/sbx/knowledge:ro", "LEAN_NUM_THREADS=3", "BUILD_MIN_FREE_GB=3",
                     "PATH=/run/harness/bin:" + str(self.elan / "bin")):
            self.assertIn(want, s)
        wrapper = (sb.state / "bin" / "lake").read_text()
        self.assertIn(f"/run/harness/buildslot.sh /run/harness/slots {self.elan}/bin/lake", wrapper)
        self.assertEqual(sorted(p.name for p in self.host.slot_dir.iterdir()), ["slot.1", "slot.2", "slot.3"])
        self.assertEqual(sorted(p.name for p in sb.home.iterdir()), ["knowledge"])   # the only host write in HOME

    def test_concurrent_sessions_do_not_collide(self):
        a, b = self.sandbox("proj/A01", "A01"), self.sandbox("proj/A02", "A02")
        c = self.sandbox("other/A01", "oA01")
        names = {x.name for x in (a, b, c)} | {x.network for x in (a, b, c)}
        self.assertEqual(len(names), 6)
        self.assertEqual(len(set(a.volumes() + b.volumes() + c.volumes())), 6)
        for x in (a, b):
            x.command("m", None)
        self.assertNotEqual(self.d.nets[a.network]["members"], set() )
        self.assertNotIn(b.name, " ".join(" ".join(c_) for c_ in self.d.calls if a.network in c_ and c_[0] == "run"))

    def test_close_removes_the_network_and_volumes(self):
        sb = self.sandbox("proj/A01", "A01")
        sb.command("m", None)
        sb.close()
        self.assertNotIn(sb.network, self.d.nets)
        self.assertIn(("volume", "rm", "-f", sb.volumes()[0]), [c[:4] for c in self.d.calls])

    def test_mount_sources_in_the_tree_cannot_point_at_the_host(self):
        """The model moves .lake aside and leaves symlinks to host directories where the recipe mounts
        from its tree: the package farm, the overlay's lower layer, a parent of either. Nothing is mounted."""
        host_dir = self.r / "host-home"
        host_dir.mkdir()
        for rel in (".lake/packages", ".lake/build", ".lake"):
            sb = self.sandbox("proj/A01", "A01")
            lake = sb.tree / ".lake"
            if lake.is_symlink() or lake.exists():
                if lake.is_symlink():
                    lake.unlink()
                else:
                    import shutil
                    shutil.rmtree(lake)
            target = sb.tree / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            os.symlink(host_dir, target)
            if rel == ".lake/build":
                (sb.tree / ".lake" / "packages").mkdir(exist_ok=True)
            n = len(self.d.calls)
            with self.assertRaises(OSError, msg=rel):
                sb.command("m", None)
            self.assertFalse([c for c in self.d.calls[n:] if c[:2] == ("volume", "create")], rel)
        self.assertEqual(list(host_dir.iterdir()), [])

    def test_a_host_mount_that_resolves_into_the_tree_is_refused(self):
        sb = self.sandbox("proj/A01", "A01")
        link = self.r / "looks-like-host"
        os.symlink(sb.tree, link)
        sb.recipe.mounts.append(container.Mount(link, "/x"))
        with self.assertRaises(OSError):
            sb.command("m", None)

    def test_a_symlinked_knowledge_mount_point_is_refused(self):
        sb = self.sandbox("proj/A01", "A01")
        os.symlink(self.r / "elsewhere", sb.home / "knowledge")
        with self.assertRaises(OSError):
            sb.command("m", None)


class Host_(unittest.TestCase):
    def test_slots_and_shell(self):
        with tempfile.TemporaryDirectory() as d:
            h = Host(state=Path(d), slots=1, build_min_free_gb=0)
            with h.slot() as i:
                self.assertEqual(i, 1)
            rc, out = h.shell("echo hi; exit 3", log=Path(d) / "log", slot=True)
            self.assertEqual((rc, out.strip()), (3, "hi"))
            self.assertIn("[exit 3", (Path(d) / "log").read_text())

    def test_sized(self):
        h = Host.sized(Path("/tmp/x"), slots=4)
        self.assertEqual(h.slots, 4)
        self.assertGreaterEqual(h.memory_gb, 2)
        with self.assertRaises(KeyError):
            Host.sized(Path("/tmp/x"), nope=1)


if __name__ == "__main__":
    unittest.main()
