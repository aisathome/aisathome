# SPDX-License-Identifier: Apache-2.0
"""A project adapter for aisathome_harness: copy this file and PROJECT.md next to it, then fill in the
steps marked "...". The harness runs each task's session in the sandbox, its fix rounds and its evidence
(task.run_task); the adapter says what a task is, how its tree is set up, what the model sees and how the
result is judged.

    python3 adapter.py run TASK_ID --base-url https://api.example.org/v1 --key-file ~/.key --model MODEL

Configuration comes from the environment, nothing is fixed to one machine: MYPROJECT_WORK (trees, homes,
state; default ./work), MYPROJECT_RAW (evidence; default ./raw), MYPROJECT_IMAGE (the sandbox image:
opencode as its entrypoint plus the project's toolchain), and the host's shared state
(AISATHOME_HARNESS_STATE, see host.py).
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
from pathlib import Path

from aisathome_harness import loop
from aisathome_harness.host import Host
from aisathome_harness.project import Project, Task, Verdict, Workspace
from aisathome_harness.sandbox.container import Recipe, Sandbox
from aisathome_harness.task import Caps, Endpoint, run_task

HERE = Path(__file__).resolve().parent
WORK = Path(os.environ.get("MYPROJECT_WORK") or HERE / "work")      # host side: trees, homes, state
RAW = Path(os.environ.get("MYPROJECT_RAW") or HERE / "raw")         # evidence, one directory per task
IMAGE = os.environ.get("MYPROJECT_IMAGE") or "my-sandbox-image"
CAPS = Caps(steps=30, minutes=30, fix_rounds=2, fix_steps=20, fix_minutes=20)


class MyProject(Project):
    name = "my-project"

    def __init__(self, host: Host | None = None):
        super().__init__(HERE / "PROJECT.md")
        self.host = host or Host.sized()
        self.recipe = Recipe(image=IMAGE, knowledge=HERE / "knowledge")     # + toolchain parts, e.g. LeanToolchain

    def select(self) -> list[Task]:
        """The tasks, e.g. from a list kept beside this file."""
        return [Task("example-1", {"goal": "Prove `foo` in Foo.lean."})]

    def workspace(self, task: Task) -> Workspace:
        b = WORK / task.id
        return Workspace(tree=b / "tree", home=b / "home", raw=RAW / task.id, state=b / "state")

    def prepare(self, task: Task) -> Workspace:
        """A fresh tree (one commit, no remote) and an empty HOME; the base snapshot last."""
        ws = self.workspace(task)
        for d in (ws.tree, ws.home):
            if d.exists():
                shutil.rmtree(d)
            d.mkdir(parents=True)
        ws.raw.mkdir(parents=True, exist_ok=True)
        # ... put the project's files into ws.tree here ...
        for c in (["git", "init", "-q"], ["git", "add", "-A"], ["git", "commit", "-qm", "base", "--allow-empty"]):
            subprocess.run(c, cwd=ws.tree, check=True, capture_output=True,
                           env={**os.environ, "GIT_AUTHOR_NAME": "base", "GIT_AUTHOR_EMAIL": "base@localhost",
                                "GIT_COMMITTER_NAME": "base", "GIT_COMMITTER_EMAIL": "base@localhost"})
        loop.snapshot_base(ws.tree, ws.state)
        return ws

    def session(self, task: Task, ws: Workspace, e: Endpoint) -> loop.Session:
        sb = Sandbox(self.recipe, self.host, f"{self.name}/{task.id}", ws.tree, ws.home, ws.state,
                     e.base_url, e.key_file, e.model)
        return loop.Session(task_id=task.id, tree=ws.tree, raw=ws.raw, home=ws.home, state=ws.state, sandbox=sb,
                            prompt=lambda steps, minutes: self.prompt(task, steps, minutes), collect=("NOTES.md",))

    def prompt(self, task: Task, steps: int, minutes: int) -> str:
        return (f"{task.data['goal']}\n\nThe repository is your working directory; you have no network. "
                f"Build after every edit. At most {steps} steps and {minutes} minutes of your own time. "
                "End with a short report in ~/NOTES.md.")

    def check(self, task: Task, ws: Workspace, run: dict, round: int) -> Verdict:
        """The machine verdict on the tree. Never trust the model's own report; copy the files you judge
        into a fresh host-side workspace first (the tree is the model's)."""
        return Verdict("no_solution", detail="not implemented")

    def feedback(self, task: Task, verdict: Verdict, round: int, total: int) -> str | None:
        if verdict.outcome == "proved":
            return None
        return f"Our check says {verdict.outcome}: {verdict.detail}. Fix it; the task is as before."

    def _submit(self, task: Task, ws: Workspace):
        raise NotImplementedError("submitting is the project's own step, behind the submit lock")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["run"])
    ap.add_argument("task")
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--key-file")
    ap.add_argument("--model", required=True)
    a = ap.parse_args()
    p = MyProject()
    task = next(t for t in p.select() if t.id == a.task)
    print(run_task(p, task, Endpoint(a.base_url, a.model, a.key_file), CAPS)["final"])
