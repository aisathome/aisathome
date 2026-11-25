# SPDX-License-Identifier: Apache-2.0
"""A runnable example of a project adapter: one Lean exercise (exercise/Hello.lean, Lean's core only, no Mathlib)
that the model proves in the sandbox, checked on the host in a jail. It is template/adapter.py filled in; copy this
folder and run it, see ../../README.md ("Run one task").

    python3 adapter.py run hello --base-url http://tiny-llm:8080/v1 --model tiny
    python3 adapter.py clean hello      # its work and evidence, to run it again

Configuration from the environment: LEAN_EXAMPLE_ELAN (an elan home with the toolchain that exercise/lean-toolchain
names; default ~/.elan), LEAN_EXAMPLE_IMAGE (the sandbox image built from ./Dockerfile; default
harness-lean-example), LEAN_EXAMPLE_WORK and LEAN_EXAMPLE_RAW (default ./work and ./raw), and the host's shared
state (AISATHOME_HARNESS_STATE, see host.py).

The check copies the model's Hello.lean into a fresh directory with our own Lake files, refuses it on the text scan
(lean.axioms.scan: sorry, admit, native_decide, axioms, code run while compiling), builds it in a jail (no network,
not the invoking user) and checks the theorem's type and axioms there (lean.axioms.check_command). A real project
also replays the proof through the kernel against a target compiled without it (lean.replay).
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
from pathlib import Path

from aisathome_harness import loop, safefs
from aisathome_harness.host import Host
from aisathome_harness.lean import axioms
from aisathome_harness.lean.toolchain import LeanToolchain, open_build_dirs
from aisathome_harness.project import Project, Task, Verdict, Workspace
from aisathome_harness.sandbox import sandbox
from aisathome_harness.sandbox.container import Recipe, Sandbox
from aisathome_harness.task import Caps, Endpoint, run_task

HERE = Path(__file__).resolve().parent
EXERCISE = HERE / "exercise"
WORK = Path(os.environ.get("LEAN_EXAMPLE_WORK") or HERE / "work")
RAW = Path(os.environ.get("LEAN_EXAMPLE_RAW") or HERE / "raw")
IMAGE = os.environ.get("LEAN_EXAMPLE_IMAGE") or "harness-lean-example"
ELAN = Path(os.environ.get("LEAN_EXAMPLE_ELAN") or Path.home() / ".elan").resolve()
CAPS = Caps(steps=20, minutes=10, fix_rounds=1, fix_steps=10, fix_minutes=5)
THEOREM = "add_comm_example"
TARGET = "theorem check_target (a b : Nat) : a + b = b + a := sorry"    # the statement, as the exercise has it


class LeanExample(Project):
    name = "lean-example"

    def __init__(self, host: Host | None = None):
        super().__init__(HERE / "PROJECT.md")
        # a desktop's share: the sandbox and the jail get at most 4 CPUs and 6 GB
        self.host = host or Host.sized(cpus=min(4, os.cpu_count() or 1), threads=4, memory_gb=6, build_min_free_gb=2)
        self.lean = LeanToolchain(ELAN, threads=self.host.threads)

    def select(self) -> list[Task]:
        return [Task("hello", {"goal": f"Prove `{THEOREM}` in Hello.lean: replace its `sorry` with a proof. "
                                       "Do not change the statement. Build with `lake build`."})]

    def workspace(self, task: Task) -> Workspace:
        b = WORK / task.id
        return Workspace(tree=b / "tree", home=b / "home", raw=RAW / task.id, state=b / "state")

    def prepare(self, task: Task) -> Workspace:
        ws = self.workspace(task)
        for d in (ws.tree, ws.home):
            if d.exists():
                shutil.rmtree(d)
            d.mkdir(parents=True)
        ws.raw.mkdir(parents=True, exist_ok=True)
        for f in ("Hello.lean", "lakefile.toml", "lake-manifest.json", "lean-toolchain"):
            shutil.copy(EXERCISE / f, ws.tree / f)
        (ws.tree / ".gitignore").write_text(".lake/\n")
        for d in (".lake/packages", ".lake/build"):          # mounted (packages) and overlaid (build) by the recipe
            (ws.tree / d).mkdir(parents=True, exist_ok=True)
        for c in (["git", "init", "-q"], ["git", "add", "-A"], ["git", "commit", "-qm", "base", "--allow-empty"]):
            subprocess.run(c, cwd=ws.tree, check=True, capture_output=True,
                           env={**os.environ, "GIT_AUTHOR_NAME": "base", "GIT_AUTHOR_EMAIL": "base@localhost",
                                "GIT_COMMITTER_NAME": "base", "GIT_COMMITTER_EMAIL": "base@localhost"})
        loop.snapshot_base(ws.tree, ws.state)
        return ws

    def session(self, task: Task, ws: Workspace, e: Endpoint) -> loop.Session:
        recipe = Recipe(image=IMAGE).merged(**self.lean.recipe_parts(ws.tree))
        sb = Sandbox(recipe, self.host, f"{self.name}/{task.id}", ws.tree, ws.home, ws.state,
                     e.base_url, e.key_file, e.model)
        return loop.Session(task_id=task.id, tree=ws.tree, raw=ws.raw, home=ws.home, state=ws.state, sandbox=sb,
                            prompt=lambda steps, minutes: self.prompt(task, steps, minutes), collect=("NOTES.md",))

    def prompt(self, task: Task, steps: int, minutes: int) -> str:
        return (f"{task.data['goal']}\n\nThe repository is your working directory; you have no network. "
                f"Build after every edit. At most {steps} steps and {minutes} minutes of your own time. "
                "End with a short report in ~/NOTES.md.")

    def check(self, task: Task, ws: Workspace, run: dict, round: int) -> Verdict:
        try:
            text = safefs.read_text(ws.tree, "Hello.lean")      # never through a symlink the model made
        except (OSError, safefs.UnsafePath) as e:
            return Verdict("no_solution", detail=f"Hello.lean: {e}")
        if text == (EXERCISE / "Hello.lean").read_text():
            return Verdict("no_solution", detail="Hello.lean is unchanged")
        why = axioms.scan(text)
        if why:
            return Verdict("refused", detail="; ".join(why))
        # a fresh workspace: the model's Hello.lean, our own Lake files and check file
        cw = ws.state / f"check-{round}"
        if cw.exists():
            shutil.rmtree(cw)
        cw.mkdir(parents=True)
        for f in ("lakefile.toml", "lake-manifest.json", "lean-toolchain"):   # Lake writes no manifest
            shutil.copy(EXERCISE / f, cw / f)
        (cw / "Hello.lean").write_text(text)
        mark = axioms.new_mark()
        (cw / "Check.lean").write_text(f"import Lean\nimport Hello\n{axioms.check_command(mark=mark, decl=THEOREM)}\n"
                                       f"{TARGET}\n#check_solution check_target\n")
        open_build_dirs(cw)
        jail = self.lean.jail(IMAGE, cw, memory_gb=4, cpus=self.host.cpus)
        rc, out = jail.run(["lake", "build"], timeout=600, log=ws.raw / f"check-{round}.log")
        if rc != 0:
            return Verdict("compile_error", detail=out[-2000:])
        rc, out = jail.run(["lake", "env", "lean", "Check.lean"], timeout=600, log=ws.raw / f"check-{round}.log")
        r = axioms.parse_check(out, mark, rc)
        if not r["ok"]:
            return Verdict("check_failed", detail=f"{r['reason']}: {r['detail']}" if r["reason"] else out[-2000:])
        return Verdict("proved", data={"axioms": r["axioms"]})

    def feedback(self, task: Task, verdict: Verdict, round: int, total: int) -> str | None:
        if verdict.outcome == "proved":
            return None
        return f"Our check says {verdict.outcome}: {verdict.detail}. Fix it; the task is as before."

    def _submit(self, task: Task, ws: Workspace):
        raise NotImplementedError("an example: nothing to submit")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["run", "clean"])
    ap.add_argument("task")
    ap.add_argument("--base-url")
    ap.add_argument("--key-file")
    ap.add_argument("--model")
    a = ap.parse_args()
    p = LeanExample()
    task = next(t for t in p.select() if t.id == a.task)
    if a.cmd == "clean":
        # a finished task does not run again; this removes its work and evidence. overlayfs leaves root-owned
        # directories in the state folder, which go in a throwaway container of the image (sandbox.drop_overlays)
        ws = p.workspace(task)
        sandbox.drop_overlays([], ws.state / "overlay", image=IMAGE)
        for d in (WORK / task.id, ws.raw):
            shutil.rmtree(d, ignore_errors=True)
        print(f"removed {WORK / task.id} and {ws.raw}")
    else:
        if not (a.base_url and a.model):
            ap.error("run needs --base-url and --model")
        print(run_task(p, task, Endpoint(a.base_url, a.model, a.key_file), CAPS)["final"])
