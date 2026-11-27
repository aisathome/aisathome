# SPDX-License-Identifier: Apache-2.0
"""A model's proof file cannot run code on the host or change what its check means.

- proofscan: the allow-list refuses every way to extend Lean or run code while a file compiles (the attacks of
  the 2026-11-24 audit as inputs: elaborators registered through attributes, `@[init]`, instance and notation
  hijacks), and lets ordinary proofs through.
- sandbox.jail: the throwaway container's command line (no network, read-only root, a uid that is not the
  invoking user, no inherited environment, limits) and its timeout and output cap; with AISATHOME_TEST_DOCKER=1,
  the same against a real Docker daemon.
- lean.toolchain / lean.pkgsets: what a jail may write in a workspace, and the package set readable for its uid.
- lean.replay: the trusted snapshot and the proof's .olean are never read through a symlink; the check program
  never imports the proof's module; a recursive definition's `_unsafe_rec` is left out of the replay, nothing else
  that is not safe (with AISATHOME_TEST_LEAN=1, the program run by a real `lean` on PATH, no jail).
"""
from __future__ import annotations

import os
import stat
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest import mock

from aisathome_harness import safefs
from aisathome_harness.lean import axioms, pkgsets, proofscan, replay
from aisathome_harness.lean.toolchain import JAIL_ELAN, JAIL_WS, LeanToolchain, open_build_dirs
from aisathome_harness.sandbox import jail as J
from aisathome_harness.sandbox.container import Mount

HEAD = "import Mathlib\nimport Definitions.Def_X\n\nopen X\n\n"
GOOD = HEAD + textwrap.dedent("""\
    namespace P2MHole01_ab
    universe u
    variable {α : Type u}

    /-- A helper. -/
    noncomputable def step (a : ℕ) : ℕ :=
      3 * a

    private lemma step_pos (a : ℕ) (h : 0 < a) : 0 < step a := by
      unfold step; omega

    def f : ℕ → ℕ
    | 0 => 1
    | n + 1 => f n

    theorem solution (s : Finset ℕ) (h : #s = 2) : 0 < #s := by
      set_option maxHeartbeats 400000 in
      omega
    end P2MHole01_ab

    set_option linter.unusedVariables false
    open scoped Classical in
    theorem solution (a b : ℝ) (h : a ≤ b) : a ≤ b :=
      h
    """)

# The audit's attack sketches (H1, H2), as a model could write them
ATTACKS = {
    "command_elab": HEAD + 'syntax (name := k) "p2m_run" : command\n'
                           '@[command_elab k] def runIt : Lean.Elab.Command.CommandElab := fun _ => pure ()\n'
                           "p2m_run\ntheorem solution : True := trivial\n",
    "term_elab": HEAD + "theorem solution : True := trivial\n"
                        '  syntax (name := t) "p2m_t" : term\n  @[term_elab t] def e : TermElab := fun _ _ => default\n',
    "tactic": HEAD + '@[tactic k] def t : Tactic := fun _ => pure ()\ntheorem solution : True := trivial\n',
    "init": HEAD + "def go : IO Unit := pure ()\n@[init go] opaque hook : Unit\ntheorem solution : True := trivial\n",
    "attribute_command": HEAD + "attribute [command_elab k] runIt\ntheorem solution : True := trivial\n",
    "by_elab": HEAD + "theorem solution : True := by_elab return default\n",
    "instance_hijack": HEAD + "instance (priority := high) hij : LE ℝ := ⟨fun _ _ => True⟩\n"
                              "theorem solution (a b : ℝ) (h : a ≤ b) : b ≤ a := trivial\n",
    "notation_hijack": HEAD + 'notation:50 a " ≤ " b => True\ntheorem solution (a b : ℝ) (h : a ≤ b) : b ≤ a := trivial\n',
    "local_notation_midline": HEAD + 'theorem h : True := trivial local notation "≤" => True\n',
    "unif_hint": HEAD + "unif_hint (a : ℝ) where\n  a =?= 0 ⊢ a + 0 =?= a\ntheorem solution : True := trivial\n",
    "eval": HEAD + "theorem solution : True := trivial\n#eval IO.Process.run {cmd := \"id\"}\n",
    "eval_midline": HEAD + "theorem solution : True := trivial #eval (1 : Nat)\n",
    "run_cmd": HEAD + "theorem solution : True := by\n  run_tac pure ()\n  trivial\n",
    "simproc": HEAD + "  simproc p (_) := fun _ => pure .continue\ntheorem solution : True := trivial\n",
    "skip_kernel": HEAD + "set_option debug.skipKernelTC true\ntheorem solution : False := sorry\n",
    "open_lean": HEAD + "open Lean in\ntheorem solution : True := trivial\n",
    "io_name": HEAD + "def x := IO.FS.readFile \"/etc/passwd\"\ntheorem solution : True := trivial\n",
    "unsafe": HEAD + "theorem solution : True := trivial\n  unsafe def x : Nat := unsafeCast ()\n",
    "implemented_by": HEAD + "def x : Nat := 0\n  partial def y : Nat → Nat := y\ntheorem solution : True := trivial\n",
    "quotation": HEAD + "def q := `(term| 1)\ntheorem solution : True := trivial\n",
    "import_lean": "import Lean\n" + HEAD + "theorem solution : True := trivial\n",
    "import_target": "import Targets.Sol_x\n" + HEAD + "theorem solution : True := trivial\n",
    "late_import": HEAD + "theorem solution : True := trivial\nimport Mathlib.Tactic\n",
    "column_zero_command": HEAD + "deriving instance Repr for Nat\ntheorem solution : True := trivial\n",
    "structure": HEAD + "structure S where\n  x : Nat\ntheorem solution : True := trivial\n",
    "interpolation": HEAD + 'def s := s!"{IO.println 1}"\ntheorem solution : True := trivial\n',
}


class AllowList(unittest.TestCase):
    def test_every_attack_is_refused(self):
        for name, text in ATTACKS.items():
            self.assertTrue(proofscan.refusals(text), name)

    def test_the_audits_elaborator_attacks_by_reason(self):
        r = proofscan.refusals(ATTACKS["command_elab"])
        self.assertIn(proofscan.ATTRIBUTE, r)
        self.assertIn(proofscan.SYNTAX, r)
        self.assertEqual(r[proofscan.STRUCTURE], [6, 7, 8])       # `syntax`, the attribute, the new command
        self.assertIn(proofscan.ATTRIBUTE, proofscan.refusals(ATTACKS["init"]))
        self.assertIn(proofscan.LOWLEVEL, proofscan.refusals(ATTACKS["init"]))
        self.assertIn(proofscan.INSTANCE, proofscan.refusals(ATTACKS["instance_hijack"]))
        self.assertIn(proofscan.SYNTAX, proofscan.refusals(ATTACKS["notation_hijack"]))
        self.assertIn(proofscan.SYNTAX, proofscan.refusals(ATTACKS["local_notation_midline"]))
        self.assertIn(proofscan.META, proofscan.refusals(ATTACKS["eval_midline"]))
        self.assertIn(proofscan.OPTION, proofscan.refusals(ATTACKS["skip_kernel"]))
        self.assertIn(proofscan.IMPORT, proofscan.refusals(ATTACKS["import_target"]))
        self.assertIn(proofscan.IMPORT, proofscan.refusals(ATTACKS["late_import"]))
        self.assertIn(proofscan.LEAN_NAME, proofscan.refusals(ATTACKS["io_name"]))

    def test_an_ordinary_proof_passes(self):
        self.assertEqual(proofscan.refusals(GOOD), {})

    def test_comments_and_strings_do_not_count(self):
        text = HEAD + "/-- uses @[simp], `instance`, #eval and syntax in prose -/\ntheorem solution : True := by\n" \
                      "  -- @[command_elab] instance notation\n  have : \"#eval instance\" = \"#eval instance\" := rfl\n" \
                      "  trivial\n"
        self.assertEqual(proofscan.refusals(text), {})

    def test_the_tasks_own_lines_are_given(self):
        pre = "import Mathlib\n\nlocal notation \"K\" => Fintype.card κ\nattribute [-instance] Ideal.Quotient.commSemiring\n"
        text = pre + "\ntheorem solution : K = K := rfl\n"
        self.assertTrue(proofscan.refusals(text))
        self.assertEqual(proofscan.refusals(text, proofscan.given_lines(pre)), {})
        # given verbatim only: another notation is still refused
        other = pre + "local notation \"L\" => True\ntheorem solution : K = K := rfl\n"
        self.assertIn(proofscan.SYNTAX, proofscan.refusals(other, proofscan.given_lines(pre)))

    def test_classical_local_instances_only(self):
        ok = HEAD + "attribute [local instance] Classical.propDecidable Classical.decEq\ntheorem solution : True := trivial\n"
        self.assertEqual(proofscan.refusals(ok), {})
        bad = HEAD + "attribute [local instance] myInstance\ntheorem solution : True := trivial\n"
        self.assertIn(proofscan.ATTRIBUTE, proofscan.refusals(bad))

    def test_allowed_options(self):
        for opt in ("maxHeartbeats 0", "linter.style.haveILetI false", "autoImplicit true", "pp.all true",
                    "synthInstance.maxHeartbeats 1"):
            self.assertEqual(proofscan.refusals(HEAD + f"set_option {opt}\ntheorem solution : True := trivial\n"), {}, opt)
        for opt in ("debug.skipKernelTC true", "compiler.extract_closed false", "interpreter.prefer_native true"):
            self.assertIn(proofscan.OPTION, proofscan.refusals(HEAD + f"set_option {opt}\ntheorem solution : True := trivial\n"))

    def test_every_import_token_is_read(self):
        for text in ("  import Lean\ntheorem solution : True := trivial\n",                  # indented
                     "import Mathlib import Lean\ntheorem solution : True := trivial\n",      # two on a line
                     "import Mathlib.«..».«..».ws.Solutions.Evil\ntheorem solution : True := trivial\n",
                     "import Mathlib\ntheorem solution : True := trivial\n  import Mathlib.Tactic\n",
                     "module\nimport Mathlib\ntheorem solution : True := trivial\n",
                     "prelude\ntheorem solution : True := trivial\n"):
            self.assertIn(proofscan.IMPORT, proofscan.refusals(text), text)
        self.assertEqual(proofscan.refusals("\nimport Mathlib\nimport Definitions.X\n\ntheorem solution : True := trivial\n"), {})

    def test_cardinality_notation_is_not_a_command(self):
        self.assertEqual(proofscan.refusals(HEAD + "theorem solution (s : Finset ℕ) : #s = #s := rfl\n"), {})
        self.assertIn(proofscan.META, proofscan.refusals(HEAD + "theorem solution : True := trivial #print axioms x\n"))


class Denylist(unittest.TestCase):
    def test_attribute_registered_elaborators(self):
        """The audit's finding: `axioms.scan` caught `elab` and `macro` by keyword but not the attribute path."""
        for name in ("command_elab", "term_elab", "tactic", "init", "attribute_command"):
            self.assertIn("elaborator extension", axioms.scan(ATTACKS[name]), name)
        self.assertIn("meta-level command", axioms.scan(ATTACKS["by_elab"]))
        self.assertIn("native_decide", axioms.scan(HEAD + "example : 2 + 2 = 4 := by decide +native\n"))
        self.assertEqual(axioms.scan(HEAD + "@[simp] theorem a : True := trivial\n"), [])


class JailArgs(unittest.TestCase):
    def jail(self, **kw):
        return J.Jail("img:1", mounts=[Mount(Path("/h/elan"), "/opt/elan"), Mount(Path("/h/ws"), "/ws", ro=False)],
                      env={"ELAN_HOME": "/opt/elan"}, workdir="/ws", **kw)

    def test_hardened_command_line(self):
        with mock.patch.dict(os.environ, {"AWS_SECRET_ACCESS_KEY": "s3cr3t", "P2M_API_KEY_FILE": "/k"}):
            a = self.jail(cpu_s=60, cpus=2).args(["lake", "build", "X"], "harness-jail-x")
        line = " ".join(a)
        for want in ("--rm", "--network none", "--log-driver none", "-e HTTP_PROXY= ", "-e https_proxy= ", "--cap-drop=ALL", "no-new-privileges", "--read-only", "--init",
                     f"-u {J.JAIL_USER}", "--tmpfs /tmp:rw,nosuid,nodev", "--memory 8g", "--memory-swap 8g",
                     "--pids-limit 512", "--cpus 2", "--ulimit cpu=60:60", "--ulimit core=0",
                     "type=bind,src=/h/elan,dst=/opt/elan,readonly", "type=bind,src=/h/ws,dst=/ws",
                     "-e HOME=/tmp", "-e ELAN_HOME=/opt/elan", "--entrypoint /bin/sh img:1"):
            self.assertIn(want, line)
        self.assertNotIn("dst=/ws,readonly", line)
        self.assertNotIn("s3cr3t", line)
        self.assertNotIn("P2M_API_KEY_FILE", line)
        self.assertEqual(a[-4:], ["jail", "lake", "build", "X"])
        self.assertIn("umask 0000", a[-5])

    def test_never_root_and_never_the_invoking_user(self):
        for user in ("0:0", "root", f"{os.getuid()}:{os.getgid()}", "nobody"):
            with self.assertRaises(ValueError, msg=user):
                self.jail(user=user)
        self.assertNotEqual(J.JAIL_UID, os.getuid())

    def test_mount_paths_cannot_add_options(self):
        with self.assertRaises(ValueError):
            J.Jail("img", mounts=[Mount(Path("/h/a,readonly=false"), "/x")])


FAKE_DOCKER = r"""#!/usr/bin/env python3
import os, sys, time
a = sys.argv[1:]
open(os.environ["FAKE_DOCKER_LOG"], "a").write(" ".join(a) + "\n")
if a[:1] == ["run"]:
    cmd = a[a.index("jail") + 1:]
    if cmd[:1] == ["sleep"]:
        time.sleep(float(cmd[1]))
    elif cmd[:1] == ["flood"]:
        sys.stdout.write("x" * int(cmd[1])); sys.stdout.flush()
    else:
        print("ran", " ".join(cmd))
    sys.exit(3)
"""


class JailRun(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        b = Path(self.d.name) / "bin"
        b.mkdir()
        (b / "docker").write_text(FAKE_DOCKER)
        (b / "docker").chmod(0o755)
        self.log = Path(self.d.name) / "docker.log"
        self.env = mock.patch.dict(os.environ, {"PATH": f"{b}:{os.environ['PATH']}", "FAKE_DOCKER_LOG": str(self.log)})
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.d.cleanup()

    def test_exit_code_output_and_log(self):
        out = Path(self.d.name) / "run.log"
        rc, o = J.Jail("img").run(["lake", "build"], log=out)
        self.assertEqual((rc, o.strip()), (3, "ran lake build"))
        self.assertIn("$ lake build", out.read_text())
        self.assertIn("[exit 3", out.read_text())

    def test_a_timeout_kills_the_container_by_name(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            J.Jail("img").run(["sleep", "5"], timeout=0.5)
        calls = self.log.read_text().splitlines()
        name = calls[0].split("--name ")[1].split()[0]
        self.assertTrue(name.startswith("harness-jail-"))
        self.assertIn(f"rm -f {name}", calls)

    def test_output_is_capped(self):
        rc, o = J.Jail("img", output_limit=1000).run(["flood", "50000"])
        self.assertTrue(o.startswith("x" * 1000))
        self.assertIn("[output cut: 49000 more bytes]", o)


class Workspace(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.root = Path(self.d.name)
        self.ws = self.root / "ws"
        (self.ws / "Solutions").mkdir(parents=True)
        (self.ws / "Solutions" / "Sol.lean").write_text("theorem solution : True := trivial\n")
        (self.ws / "lakefile.lean").write_text("-- lake\n")
        self.pset = self.root / "set"
        f = self.pset / ".lake" / "packages" / "mathlib" / "M.olean"
        f.parent.mkdir(parents=True)
        f.write_text("olean")
        f.chmod(0o600)
        (self.ws / ".lake").mkdir()
        subprocess.run(["cp", "-al", str(self.pset / ".lake" / "packages"), str(self.ws / ".lake" / "packages")], check=True)

    def tearDown(self):
        self.d.cleanup()

    def mode(self, p: Path) -> int:
        return stat.S_IMODE(os.lstat(p).st_mode)

    def test_only_the_build_state_opens_to_the_jail(self):
        (self.ws / ".lake" / "build" / "lib").mkdir(parents=True)
        (self.ws / ".lake" / "build" / "lib" / "D.olean").write_text("d")
        outside = self.root / "outside.txt"
        outside.write_text("host")
        outside.chmod(0o600)
        (self.ws / ".lake" / "build" / "link").symlink_to(outside)
        open_build_dirs(self.ws)
        self.assertEqual(self.mode(self.ws / ".lake"), 0o777)
        self.assertEqual(self.mode(self.ws / ".lake" / "build" / "lib"), 0o777)
        self.assertEqual(self.mode(self.ws / ".lake" / "build" / "lib" / "D.olean"), 0o666)
        self.assertEqual(self.mode(outside), 0o600)                     # a symlink is not followed
        self.assertEqual(self.mode(self.ws / "Solutions"), self.mode(self.ws) & 0o777)
        self.assertNotEqual(self.mode(self.ws / "Solutions" / "Sol.lean") & 0o002, 0o002)
        self.assertEqual(self.mode(self.ws / ".lake" / "packages" / "mathlib" / "M.olean"), 0o600)

    def test_a_symlinked_build_dir_is_refused(self):
        (self.ws / ".lake" / "build").symlink_to(self.root)
        with self.assertRaises(safefs.UnsafePath):
            open_build_dirs(self.ws)

    def test_a_symlinked_lake_is_refused_and_its_target_left_alone(self):
        """Another workspace's .lake behind a symlink: nothing created there, no mode changed."""
        other = self.root / "other"
        (other / "build").mkdir(parents=True)
        f = other / "build" / "x.olean"
        f.write_text("x")
        f.chmod(0o644)
        before = self.mode(other / "build")
        lake = self.ws / ".lake"
        os.rename(lake, self.root / "lake-real")
        lake.symlink_to(other)
        with self.assertRaises(safefs.UnsafePath):
            open_build_dirs(self.ws)
        self.assertEqual((self.mode(f), self.mode(other / "build"), self.mode(other)), (0o644, before, self.mode(other)))
        self.assertNotEqual(self.mode(other) & 0o777, 0o777)
        self.assertFalse((other / "config").exists())
        tc = LeanToolchain(self.root / "elan")
        with self.assertRaises(safefs.UnsafePath):
            tc.jail("img", self.ws)

    def test_a_symlinked_package_farm_is_never_a_bind_source(self):
        pk = self.ws / ".lake" / "packages"
        os.rename(pk, self.root / "farm")
        pk.symlink_to(self.root / "farm")
        with self.assertRaises(safefs.UnsafePath):
            LeanToolchain(self.root / "elan").jail("img", self.ws)

    def test_execute_bits_survive(self):
        b = self.ws / ".lake" / "build" / "bin"
        b.mkdir(parents=True)
        (b / "tool").write_text("#!/bin/sh\n")
        (b / "tool").chmod(0o755)
        open_build_dirs(self.ws)
        self.assertEqual(self.mode(b / "tool"), 0o777)

    def test_the_package_set_becomes_readable_once(self):
        (self.pset / ".lake" / "packages" / "mathlib").chmod(0o700)
        self.assertEqual(pkgsets.make_readable(self.pset), 2)           # the mode-600 file and the mode-700 directory
        self.assertEqual(self.mode(self.pset / ".lake" / "packages" / "mathlib"), 0o705)
        f = self.ws / ".lake" / "packages" / "mathlib" / "M.olean"   # the farm's hard link: the same inode
        self.assertEqual(self.mode(f), 0o604)
        self.assertEqual(pkgsets.make_readable(self.pset), 0)
        self.assertEqual(pkgsets.make_readable(self.root / "missing"), 0)

    def test_the_lean_jail(self):
        tc = LeanToolchain(self.root / "elan", cache_dir=self.root / "cache", threads=3)
        j = tc.jail("img", self.ws, cpu_s=10)
        ms = {(str(m.source), str(m.target), m.ro) for m in j.mounts}
        self.assertEqual(ms, {(str(self.root / "elan"), JAIL_ELAN, True), (str(self.ws), JAIL_WS, False),
                              (str(self.ws / ".lake" / "packages"), f"{JAIL_WS}/.lake/packages", True)})
        self.assertNotIn("LAKE_CACHE_DIR", j.env)
        self.assertEqual((j.env["ELAN_HOME"], j.env["HOME"], j.env["GIT_CONFIG_VALUE_0"]), (JAIL_ELAN, "/tmp", "*"))
        self.assertTrue(j.env["PATH"].startswith(f"{JAIL_ELAN}/bin:"))
        self.assertEqual((j.workdir, j.cpu_s), (JAIL_WS, 10))
        self.assertTrue(all(m.ro for m in tc.jail("img", self.ws, writable=False).mounts))


class Replay(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.root = Path(self.d.name)
        self.ws = self.root / "ws"
        lib = self.ws / replay.LIB
        for rel in ("Targets/Sol_x.olean", "Definitions/Def_a.olean", "Solutions/Sol_x.olean"):
            (lib / rel).parent.mkdir(parents=True, exist_ok=True)
            (lib / rel).write_text(rel)
        (self.ws / ".lake" / "packages" / "mathlib").mkdir(parents=True)
        (self.ws / "lean-toolchain").write_text("leanprover/lean4:v4.33.1\n")
        self.secret = self.root / "secret"
        self.secret.write_text("SECRET")

    def tearDown(self):
        self.d.cleanup()

    def test_the_snapshot_holds_only_trusted_trees_and_regular_files(self):
        (self.ws / replay.LIB / "Definitions" / "leak.olean").symlink_to(self.secret)
        n = replay.snapshot(self.ws, self.root / "trusted", ("Definitions", "Theorems", "Targets"))
        t = self.root / "trusted"
        self.assertEqual(n, 2)
        self.assertTrue((t / "Targets" / "Sol_x.olean").is_file())
        self.assertFalse((t / "Solutions").exists())
        self.assertFalse((t / "Definitions" / "leak.olean").exists())

    def test_a_symlinked_build_tree_is_never_read(self):
        lib = self.ws / ".lake" / "build"
        os.rename(lib, self.root / "real-build")
        lib.symlink_to(self.root / "real-build")
        with self.assertRaises(safefs.UnsafePath):
            replay.snapshot(self.ws, self.root / "trusted", ("Targets",))

    def run_replay(self, **kw):
        seen = {}

        def execute(jail, argv, timeout, log):
            seen.update(jail=jail, argv=argv)
            return 0, "M_OK same_expr=true axioms=[]"
        rc, out = replay.run(LeanToolchain(self.root / "elan"), "img", ws=self.ws, trusted=self.root / "trusted",
                             module="Solutions.Sol_x", target="p2m_check_target", target_module="Targets.Sol_x",
                             decl="solution", mark="M", allowed=("propext",), work=self.root / "chk", execute=execute,
                             **kw)
        return seen

    def test_the_check_runs_read_only_on_the_proofs_olean(self):
        (self.root / "trusted").mkdir()
        seen = self.run_replay(cpu_s=30)
        j, argv = seen["jail"], seen["argv"]
        self.assertTrue(all(m.ro for m in j.mounts))
        self.assertEqual(argv[:3], ["lean", "--run", f"{replay.CHK}/Replay.lean"])
        self.assertEqual(argv[3:8], ["M", "p2m_check_target", "solution", "Targets.Sol_x", f"{replay.CHK}/Proof.olean"])
        self.assertEqual(argv[-1], "propext")
        self.assertEqual((self.root / "chk" / "Proof.olean").read_text(), "Solutions/Sol_x.olean")
        self.assertEqual(j.env["LEAN_PATH"].split(":"), [replay.TRUSTED, f"{replay.PKGS}/mathlib/.lake/build/lib/lean"])
        self.assertEqual(j.workdir, replay.CHK)
        self.assertEqual(j.cpu_s, 30)
        # nothing of the workspace but its package farm and the copied .olean: the build jail could write the rest
        self.assertEqual(sorted(str(m.target) for m in j.mounts), sorted([JAIL_ELAN, replay.TRUSTED, replay.CHK, replay.PKGS]))
        self.assertNotIn(str(self.ws.resolve()), [str(m.source) for m in j.mounts])

    def test_a_symlinked_package_farm_is_never_mounted(self):
        (self.root / "trusted").mkdir()
        pk = self.ws / ".lake" / "packages"
        os.rename(pk, self.root / "elsewhere")
        pk.symlink_to(self.root / "elsewhere")
        with self.assertRaises(safefs.UnsafePath):
            self.run_replay()

    def test_a_symlinked_olean_is_never_read(self):
        f = self.ws / replay.LIB / "Solutions" / "Sol_x.olean"
        f.unlink()
        f.symlink_to(self.secret)
        with self.assertRaises(safefs.UnsafePath):
            self.run_replay()

    def test_the_program_never_loads_the_proofs_module(self):
        p = replay.PROGRAM
        # each import a plain name under an allowed root, resolving under the search path's own entries; no `module`
        self.assertIn("unless plainName i.module && roots.contains i.module.getRoot do", p)
        self.assertIn("for m in (mod.imports.map (·.module)).push tgtMod do\n    if (← resolvedUnder trusted m).isNone then", p)
        self.assertIn("if mod.isModule then", p)
        self.assertIn("readModuleDataParts #[olean]", p)              # the proof as data
        self.assertIn("mod.imports.push { module := tgtMod }", p)     # its imports and the target, not itself
        self.assertIn("finalizeImport s imports {} 0 false false", p)  # no extensions, no initialisers
        self.assertIn("env.replay news", p)                          # every constant through the kernel
        self.assertIn("Kernel.isDefEqGuarded", p)                    # no instances, notation or hints
        self.assertNotIn("isDefEq solTy", p.replace("Kernel.isDefEqGuarded env {} solTy", ""))
        self.assertIn("unknown constant", p[p.index("def axiomsOf"):p.index("unsafe def check")])   # never skipped

    def test_only_a_safe_definitions_own_unsafe_rec_is_left_out(self):
        p = replay.PROGRAM
        self.assertIn("if d.safety matches .partial && unsafeRecOf mod n then continue", p)
        f = p[p.index("def unsafeRecOf"):p.index("unsafe def check")]
        self.assertIn('unless s == "_unsafe_rec" do return false', f)
        self.assertIn("return d.safety matches .safe", f)              # `f` itself a safe definition of the module
        self.assertIn("unless d.safety matches .safe do", p)            # every other definition still safe


# A recursive helper (Lean adds `pow2._unsafe_rec`) and what must still be refused, against a real Lean.
REPLAY_TARGET = "theorem p2m_check_target : ∀ n : Nat, 0 < 2 ^ n := sorry\n"
REPLAY_SOLUTION = "theorem solution : ∀ n : Nat, 0 < 2 ^ n := fun n => Nat.two_pow_pos n\n"
REPLAY_POW2 = "def pow2 : Nat → Nat\n  | 0 => 1\n  | n + 1 => 2 * pow2 n\n"
REPLAY_PROOFS = {
    "recursive_helper": (REPLAY_POW2 + "theorem pow2_pos (n : Nat) : 0 < pow2 n := by\n"
                         "  induction n with\n  | zero => decide\n  | succ n ih => simp [pow2]; omega\n"
                         + REPLAY_SOLUTION, None),
    "partial": ("partial def loop (n : Nat) : Nat := loop n\n" + REPLAY_SOLUTION, "`loop` is not a theorem"),
    "unsafe": ("unsafe def u : Nat := unsafeCast ()\n" + REPLAY_SOLUTION, "`u` is not a safe definition"),
    "uses_unsafe_rec": (REPLAY_POW2 + "unsafe def sneaky : Nat := pow2._unsafe_rec 3\n" + REPLAY_SOLUTION,
                        "`sneaky` is not a safe definition"),
}


@unittest.skipUnless(os.environ.get("AISATHOME_TEST_LEAN") == "1", "needs `lean` on PATH (AISATHOME_TEST_LEAN=1)")
class RealLeanReplay(unittest.TestCase):
    """replay.PROGRAM run by a real `lean` (the workers' toolchain), without the jail: the proofs above."""

    def test_a_recursive_helper_passes_and_nothing_else_unsafe_does(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            (d / "lib" / "Targets").mkdir(parents=True)
            (d / "lib" / "Targets" / "Sol_x.lean").write_text(REPLAY_TARGET)
            (d / "Replay.lean").write_text(replay.PROGRAM)
            env = dict(os.environ, LEAN_PATH=str(d / "lib"))
            subprocess.run(["lean", "-o", "Targets/Sol_x.olean", "Targets/Sol_x.lean"], cwd=d / "lib", env=env,
                           check=True, capture_output=True)
            for name, (text, refused) in REPLAY_PROOFS.items():
                (d / f"{name}.lean").write_text(text)
                subprocess.run(["lean", "-o", f"{name}.olean", f"{name}.lean"], cwd=d, env=env, check=True,
                               capture_output=True)
                r = subprocess.run(["lean", "--run", "Replay.lean", "M", "p2m_check_target", "solution", "Targets.Sol_x",
                                    f"{name}.olean", "Init,Std,Targets", "propext", "Classical.choice", "Quot.sound"],
                                   cwd=d, env=env, capture_output=True, text=True, timeout=300)
                if refused is None:
                    self.assertEqual(r.returncode, 0, (name, r.stdout, r.stderr))
                    self.assertIn("M_OK", r.stdout, name)
                else:
                    self.assertEqual(r.returncode, 2, (name, r.stdout, r.stderr))
                    self.assertIn("M_FAIL not_allowed: " + refused, r.stdout, name)


@unittest.skipUnless(os.environ.get("AISATHOME_TEST_DOCKER") == "1", "needs Docker (AISATHOME_TEST_DOCKER=1)")
class RealDocker(unittest.TestCase):
    """The jail against a real Docker daemon (AISATHOME_TEST_IMAGE, default ubuntu:24.04)."""

    def test_what_the_jail_lets_through(self):
        img = os.environ.get("AISATHOME_TEST_IMAGE", "ubuntu:24.04")
        with tempfile.TemporaryDirectory() as d:
            ro, rw = Path(d) / "ro", Path(d) / "rw"
            ro.mkdir()
            rw.mkdir()
            rw.chmod(0o777)
            (ro / "f").write_text("data")
            cfg = Path(d) / "docker-config"           # the invoking user's client proxies (with credentials)
            cfg.mkdir()
            (cfg / "config.json").write_text('{"proxies": {"default": {"httpProxy": "http://user:pw@proxy.invalid:3128"}}}')
            script = ("id -u; ls /sys/class/net; env | sort | tr '\\n' ' '; echo; "
                      "touch /ro/x 2>/dev/null && echo RO-WRITABLE; touch /rw/y && echo RW-OK; "
                      "touch /etc/z 2>/dev/null && echo ROOT-WRITABLE; touch /tmp/t && echo TMP-OK")
            with mock.patch.dict(os.environ, {"SECRET_TOKEN": "s3cr3t", "DOCKER_CONFIG": str(cfg)}):
                rc, out = J.Jail(img, mounts=[Mount(ro, "/ro"), Mount(rw, "/rw", ro=False)], memory_gb=1).run(
                    ["sh", "-c", script], timeout=120)
        self.assertEqual(rc, 0, out)
        lines = out.splitlines()
        self.assertEqual(lines[0], str(J.JAIL_UID))
        self.assertEqual(lines[1], "lo")                         # no network but loopback
        self.assertNotIn("s3cr3t", out)
        self.assertNotIn("user:pw", out)
        self.assertNotIn("RO-WRITABLE", out)
        self.assertNotIn("ROOT-WRITABLE", out)
        self.assertIn("RW-OK", out)
        self.assertIn("TMP-OK", out)


if __name__ == "__main__":
    unittest.main()
