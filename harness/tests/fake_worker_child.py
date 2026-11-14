# SPDX-License-Identifier: Apache-2.0
"""A toy project's child for the worker tests (test_worker.py): worker.child_main with callbacks driven by the
spec. "progress" stands in for a checkpoint: a first run writes it and works for `sleep` seconds; a run that
finds it resumes and finishes at once.

    python3 fake_worker_child.py run-task SPEC OUT ERR | cleanup-task SPEC | checkpoint-task SPEC
"""
from __future__ import annotations

import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from aisathome_harness import worker as W  # noqa: E402


def run(spec: dict) -> dict:
    mode = spec.get("mode", "ok")
    if mode == "crash":
        raise RuntimeError("boom in the task")
    if mode == "give_back":
        raise W.GiveBack("its checkpoint is unreadable here")
    busy = Path(spec["busy"]).read_text().strip() if spec.get("busy") and Path(spec["busy"]).exists() else None
    prog = Path(spec["progress"]) if spec.get("progress") else None
    if prog and prog.exists():
        return {"outcome": "ok", "resumed": True, "busy": busy, "worker": spec.get("worker")}
    if prog:
        prog.write_text("started\n")
    if spec.get("running"):
        Path(spec["running"]).write_text("running\n")
    time.sleep(float(spec.get("sleep") or 0))
    return {"outcome": "ok", "resumed": False, "busy": busy, "worker": spec.get("worker")}


def cleanup(spec: dict):
    if spec.get("log"):
        with open(spec["log"], "a") as fh:
            fh.write(f"cleanup {spec['task']}\n")


def checkpoint(spec: dict) -> int:
    if spec.get("log"):
        with open(spec["log"], "a") as fh:
            fh.write(f"checkpoint {spec['task']} {spec.get('why')} by {spec.get('worker')}\n")
    return 0


if __name__ == "__main__":
    sys.exit(W.child_main(sys.argv[1:], run=run, cleanup=cleanup, checkpoint=checkpoint))
