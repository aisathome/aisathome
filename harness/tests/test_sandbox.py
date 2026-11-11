# SPDX-License-Identifier: Apache-2.0
"""The sandbox with Docker stubbed: endpoints by name only, one forwarder per host and port, one internal
network per session, the container recipe (names, mounts, limits, the build-slot wrapper), and nothing the
host writes into the model's HOME except through safefs."""
from __future__ import annotations

import json
import os
import shutil
import subprocess
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
        self.root_rm = []
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
            for c in self.containers.values():
                c["networks"].pop(a[2], None)
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
        if a[0] == "ps":
            want = a[a.index("--filter") + 1].split("=", 1)[1]
            return ok("\n".join(n for n, c in self.containers.items() if want in c["labels"]))
        if a[0] == "run" and "--entrypoint" in a and a[a.index("--entrypoint") + 1] == "rm":
            # remove_as_root: root in the container may enter and empty mode-000 directories
            self.root_rm.append(a)
            src = next(x for x in a if x.startswith("type=bind,")).split(",")[1].split("=", 1)[1]
            target = Path(src) / a[-1].removeprefix("/drop/")
            if target.exists():
                os.chmod(target, 0o700)
                for d, dirs, _ in os.walk(target):
                    for x in dirs:
                        os.chmod(os.path.join(d, x), 0o700)
                shutil.rmtree(target)
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

    def test_long_host_names_with_one_prefix_do_not_share_a_forwarder(self):
        common = "inference-gateway-of-a-rather-long-organisation-name"       # longer than any slug cut
        a, b = f"{common}.one.example", f"{common}.two.example"
        self.assertNotEqual(sandbox.forwarder_name(a, 443), sandbox.forwarder_name(b, 443))


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
        fa, fb = sandbox.forwarder_for("https://one.example/v1"), sandbox.forwarder_for("https://two.example/v1")
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
        # the last session of an endpoint closes: its forwarder (and the key it holds) goes too
        sandbox.remove_session_network("p/C")
        self.assertNotIn(fa, self.d.containers)
        self.assertIn(fb, self.d.containers)

    def test_long_host_names_keep_their_own_live_forwarders(self):
        """Two endpoints whose names share a long prefix: the second never replaces the first's forwarder."""
        common = "inference-gateway-of-a-rather-long-organisation-name"
        na, nb = sandbox.ensure_session_network("p/A"), sandbox.ensure_session_network("p/B")
        sandbox.ensure_egress(f"https://{common}.one.example/v1", "img", na)
        sandbox.ensure_egress(f"https://{common}.two.example/v1", "img", nb)
        sandbox.ensure_egress(f"https://{common}.one.example/v1", "img", na)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)
        self.assertEqual(len(self.d.containers), 2)

    def test_the_key_goes_to_the_forwarder_and_each_key_has_its_own(self):
        """The forwarder mounts the key file and runs as the calling user (to read a 0600 file); another key
        for the same endpoint, or a changed key, gets a forwarder of its own; no key: no mount, nobody."""
        k1, k2 = Path(self.tmp.name) / "k1", Path(self.tmp.name) / "k2"
        k1.write_text("first-secret-key\n")
        k2.write_text("second-secret-key\n")
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n, str(k1))
        f1 = self.d.containers[sandbox.forwarder_for("https://gw.example/v1", str(k1))]["argv"]
        self.assertIn(f"{k1}:{sandbox.KEY_IN_FORWARDER}:ro", f1)
        self.assertEqual(f1[f1.index("--key-file") + 1], sandbox.KEY_IN_FORWARDER)
        self.assertEqual(f1[f1.index("-u") + 1], f"{os.getuid()}:{os.getgid()}")
        self.assertNotIn("first-secret-key", " ".join(f1))
        self.assertNotIn("first-secret-key", json.dumps(self.d.containers))     # nor in a label
        sandbox.ensure_egress("https://gw.example/v1", "img", n, str(k2))
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(len({sandbox.forwarder_for("https://gw.example/v1", k) for k in (str(k1), str(k2), None)}), 3)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 3)
        f0 = self.d.containers[sandbox.forwarder_for("https://gw.example/v1")]["argv"]
        self.assertNotIn("--key-file", f0)
        self.assertEqual(f0[f0.index("-u") + 1], "65534:65534")
        old = sandbox.forwarder_for("https://gw.example/v1", str(k1))
        k1.write_text("rotated-secret-key\n")
        self.assertNotEqual(sandbox.forwarder_for("https://gw.example/v1", str(k1)), old)

    def test_model_url_is_plain_http_to_the_forwarder(self):
        self.assertEqual(sandbox.model_url("https://api.example.org/v1"), "http://api.example.org/v1")
        self.assertEqual(sandbox.model_url("https://api.example.org:8443/x/v1/"), "http://api.example.org/x/v1")
        self.assertEqual(sandbox.model_url("http://gw.example:8080"), "http://gw.example")
        for bad in ("https://u:p@api.example.org/v1", "https://api.example.org/v1?key=1"):
            with self.assertRaises(ValueError, msg=bad):
                sandbox.model_url(bad)

    def test_same_host_other_port_gets_its_own_forwarder(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        sandbox.ensure_egress("https://gw.example:8443/v1", "img", n)
        f443, f8443 = (self.d.containers[sandbox.forwarder_for(u)] for u in ("https://gw.example/v1", "https://gw.example:8443/v1"))
        self.assertIn("gw.example:443", f443["argv"])
        self.assertIn("gw.example:8443", f8443["argv"])
        self.assertIn("0.0.0.0:80", f8443["argv"])          # plain HTTP from the sandbox, TLS onwards
        self.assertEqual(f8443["argv"][f8443["argv"].index("--scheme") + 1], "https")

    def test_a_forwarder_running_for_another_target_is_replaced(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        name = sandbox.forwarder_for("https://gw.example/v1")
        self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.target"] = "https://elsewhere.example:443/v1"
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)
        self.assertEqual(self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.target"], "https://gw.example:443/v1")

    def test_a_forwarder_for_another_target_or_version_is_replaced(self):
        n = sandbox.ensure_session_network("p/A")
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        name = sandbox.forwarder_for("https://gw.example/v1")
        self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.version"] = "old"
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 2)
        self.d.containers[name]["labels"][f"{sandbox.PREFIX}.egress.key"] = "another-key"
        sandbox.ensure_egress("https://gw.example/v1", "img", n)
        self.assertEqual(sum(1 for c in self.d.calls if c[0] == "run"), 3)


class Recipe(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        r = Path(self.tmp.name)
        self.r = r
        self.d = FakeDocker()
        self.patches = [mock.patch.object(sandbox, "_docker", self.d),
                        mock.patch.object(sandbox, "LOCK", r / "egress.lock"),
                        mock.patch.object(container.subprocess, "run", self.docker_cli)]
        for p in self.patches:
            p.start()
        self.host = Host(state=r / "host", slots=3, cpus=4, memory_gb=6, pids=500, build_min_free_gb=3)
        self.elan = r / "elan"
        (self.elan / "bin").mkdir(parents=True)
        (r / "know").mkdir()
        (r / "key").write_text("sk-the-endpoint-key-123\n")

    stuck: set = set()      # containers `docker rm -f` cannot remove
    daemon_down = ""        # the CLI's error when it cannot reach Docker

    def docker_cli(self, argv, **kw):
        """The docker CLI as container.py calls it: rm succeeds, and inspect finds only a stuck container."""
        if argv[:3] == ["docker", "container", "inspect"]:
            if self.daemon_down:
                return mock.Mock(returncode=1, stdout="", stderr=self.daemon_down)
            if argv[-1] in self.stuck:
                return mock.Mock(returncode=0, stdout="abc\n", stderr="")
            return mock.Mock(returncode=1, stdout="", stderr=f"Error: No such container: {argv[-1]}")
        return mock.Mock(returncode=0, stdout="", stderr="")

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
        sb = self.sandbox("proj/A01", "A01")
        argv = sb.command("hello", "ses_1")
        s = " ".join(argv)
        self.assertEqual(argv[:6], ["docker", "run", "--rm", "--name", sb.name, "-u"])
        self.assertEqual(argv[-3:], ["--session", "ses_1", "hello"])
        for want in ("--cap-drop=ALL", "no-new-privileges", f"--network {sb.network}", "--memory 6g", "--pids-limit 500",
                     "--cpus 4", "--add-host code.example:127.0.0.9", "--add-host api.example.org:10.0.0.",
                     f"-v {sb.home}:/home/sbx", f"-v {sb.tree}:{sb.tree}", f"-v {self.elan}:{self.elan}:ro",
                     f"{sb.tree}/.lake/packages:{sb.tree}/.lake/packages:ro",
                     f"-v {self.host.slot_dir}:/run/harness/slots:ro", "/run/harness/buildslot.sh:ro",
                     f"{self.r / 'know'}:/home/sbx/knowledge:ro", "LEAN_NUM_THREADS=3", "BUILD_MIN_FREE_GB=3",
                     "PATH=/run/harness/bin:" + str(self.elan / "bin")):
            self.assertIn(want, s)
        wrapper = (sb.state / "bin" / "lake").read_text()
        self.assertIn(f"/run/harness/buildslot.sh /run/harness/slots {self.elan}/bin/lake", wrapper)
        self.assertEqual(sorted(p.name for p in self.host.slot_dir.iterdir()), ["slot.1", "slot.2", "slot.3"])
        self.assertEqual(sorted(p.name for p in sb.home.iterdir()), ["knowledge"])   # the only host write in HOME

    def test_the_container_runs_as_the_user_who_owns_the_tree(self):
        """The project's workspace is made by the invoking user: by default the model's container (and the
        export) runs as that user, not a fixed 1000 (Codex review, 2026-11-10); root is refused."""
        with mock.patch.object(container.os, "getuid", return_value=1234), \
                mock.patch.object(container.os, "getgid", return_value=1235):
            sb = self.sandbox("proj/A01", "A01")
            self.assertEqual(container.Recipe(image="img:1").user, "1234:1235")
        argv = sb.command("hello", None)
        self.assertEqual(argv[argv.index("-u") + 1], "1234:1235")
        for root in ("0:0", "0", "root"):
            with self.assertRaises(ValueError, msg=root):
                container.Sandbox(container.Recipe(image="img:1", user=root), self.host, "proj/B", sb.tree, sb.home,
                                  sb.state, "https://api.example.org/v1", None, "m")

    def test_a_container_that_cannot_be_removed_stops_the_session(self):
        """A leftover container that `docker rm -f` did not remove could still change the tree: before()
        raises, so no segment, checkpoint or export runs on it (Codex review, 2026-11-10)."""
        sb = self.sandbox("proj/A01", "A01")
        sb.before()
        self.stuck = {sb.name}
        with self.assertRaises(RuntimeError):
            sb.before()
        with self.assertRaises(RuntimeError):
            sb.command("hello", None)
        self.stuck = set()
        for down in ("Cannot connect to the Docker daemon at unix:///var/run/docker.sock.",
                     "unable to resolve docker endpoint: open /home/u/.docker/contexts/meta/x/meta.json: "
                     "no such file or directory",
                     "Error: No such container: another-container"):
            self.daemon_down = down                        # nothing confirms that this container is gone
            with self.assertRaises(RuntimeError, msg=down):
                sb.before()

    def test_the_key_never_enters_the_model_s_container(self):
        """No mount of the key file and no key text in the model's `docker run`; the forwarder has it."""
        sb = self.sandbox("proj/A01", "A01")
        argv = " ".join(sb.command("hello", None))
        self.assertNotIn(str(self.r / "key"), argv)
        self.assertNotIn("sk-the-endpoint-key-123", argv)
        self.assertNotIn("/run/harness/key", argv)
        fw = self.d.containers[sandbox.forwarder_for(sb.base_url, str(self.r / "key"))]["argv"]
        self.assertIn(f"{self.r / 'key'}:{sandbox.KEY_IN_FORWARDER}:ro", fw)
        self.assertEqual(sb.model_url(sb.base_url), "http://api.example.org/v1")

    def test_a_key_file_inside_the_model_s_tree_or_home_is_refused(self):
        sb = self.sandbox("proj/A01", "A01")
        (sb.home / "key").write_text("sk-in-home-000000\n")
        sb.key_file = str(sb.home / "key")
        with self.assertRaises(OSError):
            sb.command("m", None)
        sb.key_file = str(self.r / "missing-key")
        with self.assertRaises(OSError):
            sb.command("m", None)

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

    def _overlay_residue(self, sb) -> Path:
        """What a spot reboot mid-session leaves: overlayfs's work/work, mode 000 with a file in it (root's
        on the runner; here the test user's, whom mode 000 locks out of listing it just the same)."""
        w = sb.state / "overlay" / "0" / "work" / "work"
        (w / "incompat").mkdir(parents=True)
        (w / "incompat" / "volatile").write_text("x")
        (sb.state / "overlay" / "0" / "upper").mkdir(parents=True)
        os.chmod(w, 0)
        self.addCleanup(lambda: w.exists() and os.chmod(w, 0o755))
        return w

    def test_drop_removes_root_owned_overlay_leftovers(self):
        """2026-11-10, prove2me O02: after a spot reboot, prepare's removal of the state directory hit the
        root-owned work/work that drop left behind (PermissionError), and the whole run ended. drop removes
        it as root, in a container with only DAC_OVERRIDE, no network, and only the state directory mounted."""
        sb = self.sandbox("proj/O02", "O02")
        self._overlay_residue(sb)
        sb.drop()
        self.assertFalse((sb.state / "overlay").exists())
        shutil.rmtree(sb.state)                 # what prepare does next: no PermissionError
        (a,) = self.d.root_rm
        self.assertEqual(a[a.index("--network") + 1], "none")
        self.assertEqual(a[a.index("-u") + 1], "0:0")
        self.assertIn("--cap-drop=ALL", a)
        self.assertEqual([x for x in a if x.startswith("--cap-add")], ["--cap-add=DAC_OVERRIDE"])
        self.assertIn("no-new-privileges", a)
        self.assertIn(f"type=bind,src={sb.state.resolve()},dst=/drop", a)
        self.assertEqual(a[-4:], ["-rf", "--one-file-system", "--", "/drop/overlay"])
        self.assertEqual(a[a.index("--entrypoint") + 2], "img:1")

    def test_drop_without_leftovers_needs_no_root(self):
        sb = self.sandbox("proj/O03", "O03")
        (sb.state / "overlay" / "0" / "upper").mkdir(parents=True)
        sb.drop()
        self.assertFalse((sb.state / "overlay").exists())
        self.assertEqual(self.d.root_rm, [])

    def test_drop_overlays_without_an_image_leaves_the_residue(self):
        sb = self.sandbox("proj/O04", "O04")
        w = self._overlay_residue(sb)
        sandbox.drop_overlays([], sb.state / "overlay")
        self.assertTrue(os.path.lexists(w))
        self.assertEqual(self.d.root_rm, [])

    def test_remove_as_root_unlinks_a_symlink_and_never_follows_it(self):
        target = self.r / "elsewhere"
        target.mkdir()
        (target / "keep").write_text("k")
        link = self.r / "state-link"
        link.symlink_to(target)
        self.assertTrue(sandbox.remove_as_root(link, "img:1"))
        self.assertFalse(os.path.lexists(link))
        self.assertTrue((target / "keep").exists())
        self.assertEqual(self.d.root_rm, [])

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

    @unittest.skipUnless(shutil.which("flock"), "needs flock(1)")
    def test_slots_work_read_only_and_the_host_follows_no_link(self):
        """The sandboxes see the slot directory read-only, so the model can neither replace a lock file nor
        plant a link there (Codex review, 2026-11-10): buildslot.sh locks with a read descriptor, against the
        host's own lock."""
        script = Path(container.__file__).with_name("buildslot.sh")
        with tempfile.TemporaryDirectory() as d:
            h = Host(state=Path(d), slots=1, build_min_free_gb=0)
            slots = h.slot_dir
            os.chmod(slots / "slot.1", 0o444)
            os.chmod(slots, 0o555)                         # as the container sees it (a read-only mount)
            try:
                env = {**os.environ, "BUILD_MIN_FREE_GB": "0"}
                run = lambda **kw: subprocess.run(["sh", str(script), str(slots), "sh", "-c", "echo built"], env=env,
                                                  capture_output=True, text=True, **kw)
                r = run(timeout=10)
                self.assertEqual((r.returncode, r.stdout.strip()), (0, "built"), r.stderr)
                with h.slot():                              # the host holds the only slot: the build waits
                    with self.assertRaises(subprocess.TimeoutExpired):
                        run(timeout=1.5)
            finally:
                os.chmod(slots, 0o755)

    def test_the_host_opens_no_slot_file_through_a_link(self):
        """A link in the slot directory (planted while it was writable, by an older harness's sandbox) is
        refused, never followed: the host creates and locks nothing elsewhere (Codex review, 2026-11-10)."""
        with tempfile.TemporaryDirectory() as d:
            h = Host(state=Path(d), slots=1, build_min_free_gb=0)
            slots = h.slot_dir
            victim = Path(d) / "victim"
            (slots / "slot.1").unlink()
            os.symlink(victim, slots / "slot.1")
            with self.assertRaises(OSError):
                h.slot_dir
            with self.assertRaises(OSError), h.slot():
                pass
            self.assertFalse(victim.exists())

    def test_sized(self):
        h = Host.sized(Path("/tmp/x"), slots=4)
        self.assertEqual(h.slots, 4)
        self.assertGreaterEqual(h.memory_gb, 2)
        with self.assertRaises(KeyError):
            Host.sized(Path("/tmp/x"), nope=1)


if __name__ == "__main__":
    unittest.main()
