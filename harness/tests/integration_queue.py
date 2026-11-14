# SPDX-License-Identifier: Apache-2.0
"""An integration run of queue, worker and checkpoint against a real gateway (not part of the offline suite):
a toy task (count to N, one step a second, a checkpoint after each step) is created on a project's queue;
worker A claims it, renews its lease, is stopped halfway, takes its checkpoint and gives the task back; worker B,
with its own state and work directories (another machine) and a shared checkpoint store, claims it, restores
the checkpoint, finishes from where A stopped and reports the result. The task is marked exported at the end.

    python3 tests/integration_queue.py --gateway https://gateway.example --key-file project.key --project p \\
        [--store DIR] [--steps 8] [--prefix itest]

Use a test gateway, never one whose queue real workers serve: the run refuses to start while the project has
other queued tasks of kind `other` (a worker here could take one of them).
"""
from __future__ import annotations

import argparse
import json
import shutil
import socket
import sys
import tempfile
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from aisathome_harness import checkpoint as K  # noqa: E402
from aisathome_harness import queue as Q  # noqa: E402
from aisathome_harness import worker as W  # noqa: E402


# ----------------------------------------------------------------------------- the toy project's child

def layout(spec: dict) -> K.Layout:
    work = Path(spec["work"])
    d = work / "tasks" / spec["task"]
    return K.Layout(task=spec["task"], parts=[K.Part("count", d)], notes=work / "notes", needs=(d / "count.json",),
                    work=str(work))


def run(spec: dict) -> dict:
    st, lay, pre = K.DirStore(Path(spec["store"])), layout(spec), f"ckpt/{spec['task']}/"
    m = K.restore(st, lay, pre, log=lambda s: print(s, flush=True))
    f = lay.parts[0].path / "count.json"
    f.parent.mkdir(parents=True, exist_ok=True)
    state = json.loads(f.read_text()) if f.exists() else {"n": 0, "by": []}
    start = state["n"]
    while state["n"] < int(spec["steps"]):
        time.sleep(1)
        state["n"] += 1
        state["by"].append(spec["worker"])
        f.write_text(json.dumps(state))
        K.save(st, lay, pre, "step", worker=spec["worker"], meta={"n": state["n"]}, log=lambda s: print(s, flush=True))
        print(f"[{spec['task']}] step {state['n']}", flush=True)
    return {"outcome": "counted", "checks_passed": True, "steps": state["n"], "prompt_tokens": 0, "completion_tokens": 0,
            "model_min": 0, "awake_min": 0, "score": {"resumed_from": start, "restored_seq": (m or {}).get("seq"),
                                                       "workers": sorted(set(state["by"]))}}


def checkpoint(spec: dict) -> int:
    lay = layout(spec)
    if not lay.complete():
        return 0
    K.save(K.DirStore(Path(spec["store"])), lay, f"ckpt/{spec['task']}/", spec.get("why") or "give-back",
           worker=spec["worker"], log=lambda s: print(s, flush=True))
    return 0


def cleanup(spec: dict):
    shutil.rmtree(Path(spec["work"]) / "tasks" / spec["task"], ignore_errors=True)
    K.forget(layout(spec))


# ----------------------------------------------------------------------------- the run

class Counting:
    """The client, counting the heartbeats the gateway took."""

    def __init__(self, c: Q.QueueClient):
        self.c, self.beats = c, 0

    def __getattr__(self, k):
        return getattr(self.c, k)

    def heartbeat(self, *a, **k):
        r = self.c.heartbeat(*a, **k)
        self.beats += r[0] == 200
        return r


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--gateway", required=True)
    ap.add_argument("--key-file", required=True)
    ap.add_argument("--project", required=True)
    ap.add_argument("--store", help="the checkpoint store directory (default: a scratch directory)")
    ap.add_argument("--steps", type=int, default=8)
    ap.add_argument("--prefix", default="itest")
    ap.add_argument("--distinct-b", action="store_true",
                    help="worker B under its own name even without hold_seconds (it waits out the 10-minute hold)")
    a = ap.parse_args(argv)
    c = Q.QueueClient(a.gateway, a.key_file, a.project, timeout=20)
    queued = [t["id"] for t in c.list(state="queued", kind="other").check().get("tasks") or []]
    if queued:
        print(f"refused: the project has queued tasks of kind other ({', '.join(queued[:5])}); a worker here could take one")
        return 2
    hold = c.claim(f"{socket.gethostname().split('.')[0]}-itest-probe", 30, kinds=["other"], hold_seconds=0)
    if hold.status == 200:                       # queued meanwhile: back at once, with its try
        c.give_back(hold.body["task"]["id"], hold.body["lease"]["worker"], hold.body["lease"]["id"], "integration probe")
        print("refused: a task was queued while probing")
        return 2
    hold_ok = hold.status == 204 and not c.hold_refused      # the client retries without the field on an older gateway
    root = Path(tempfile.mkdtemp(prefix="itest-"))
    store = Path(a.store) if a.store else root / "store"
    tid = f"{a.prefix}-{time.strftime('%Y%m%d-%H%M%S', time.gmtime())}"
    host = socket.gethostname().split(".")[0]
    name_a = f"{host}-itest-a"
    # without hold_seconds a given-back task waits 10 minutes for its worker: B takes A's name (a restart on
    # another machine), unless --distinct-b
    name_b = f"{host}-itest-b" if hold_ok or a.distinct_b else name_a
    c.create({"id": tid, "kind": "other", "title": f"harness integration {tid}", "created_by": "harness-integration",
              "max_tries": 2, "order": -1000, "spec": {"toy": True, "steps": a.steps}}).check(200, 201)
    print(f"created {tid} on {c.base} (hold_seconds {'supported' if hold_ok else 'not supported'})")
    child = [sys.executable, "-u", str(Path(__file__).resolve()), "child"]

    def on_claim_for(work: Path):
        def on_claim(task):
            if task["id"] != tid:
                raise RuntimeError(f"claimed {task['id']}, not {tid}")   # never fail another's task: stop here
            return {**task["spec"], "task": tid, "work": str(work), "store": str(store)}
        return on_claim

    logs: list[str] = []
    say = lambda m: (logs.append(m), print(m, flush=True))      # noqa: E731
    ca = Counting(c)
    wa = W.Worker(ca, name_a, root / "a" / "state", child, on_claim=on_claim_for(root / "a" / "work"), kinds=["other"],
                  heartbeat_s=2, poll_s=1, report_wait_s=2, checkpoints=True, hold_s=0 if hold_ok else None, log=say)
    th = threading.Thread(target=wa.run_forever)
    th.start()
    seen = None
    for _ in range(120):
        m = K.remote_manifest(K.DirStore(store), f"ckpt/{tid}/") if store.exists() else None
        if m and m.get("n", 0) >= a.steps // 2:
            seen = m
            break
        time.sleep(0.5)
    wa.stop()
    th.join(120)
    after_a = c.get(tid).check()["task"]
    print(f"A stopped at checkpoint {seen and seen['seq']}; the task: {after_a['state']}, tries {after_a['tries']}, "
          f"interrupted {after_a.get('interrupted')}, heartbeats taken {ca.beats}")
    wb = W.Worker(c, name_b, root / "b" / "state", child, on_claim=on_claim_for(root / "b" / "work"), kinds=["other"],
                  heartbeat_s=2, poll_s=1, report_wait_s=2, checkpoints=True, hold_s=0 if hold_ok else None, log=say)
    t0 = time.monotonic()
    claimed_at: list[float] = []

    def say_b(m: str):
        if f"[{tid}] claimed" in m and not claimed_at:
            claimed_at.append(round(time.monotonic() - t0, 1))
        say(m)
    wb.log = say_b
    while True:
        wb.run_forever(once=True)
        t = c.get(tid).check()["task"]
        if t["state"] != "queued" or time.monotonic() - t0 > 900:
            break
        time.sleep(20)
    res = t.get("result") or {}
    score = res.get("score") or {}
    out = {"task": tid, "gateway": c.base, "hold_seconds": hold_ok, "a": name_a, "b": name_b,
           "after_a": {k: after_a.get(k) for k in ("state", "tries", "interrupted", "interruptions", "prefer_worker")},
           "heartbeats_a": ca.beats, "b_claimed_after_s": claimed_at[0] if claimed_at else None, "final": {k: t.get(k) for k in ("state", "tries", "interruptions")},
           "result": {k: res.get(k) for k in ("outcome", "steps")}, "score": score,
           "b_log_restore": [l for l in (root / "b" / "state" / "logs" / f"{tid}.log").read_text().splitlines()
                             if "restored checkpoint" in l] if (root / "b" / "state" / "logs" / f"{tid}.log").exists() else []}
    ok = (after_a["state"] == "queued" and after_a.get("interrupted") == "given_back" and ca.beats >= 1
          and t["state"] == "done" and t["tries"] == 1 and t.get("interruptions") == 1 and res.get("steps") == a.steps
          and 0 < int(score.get("resumed_from") or 0) < a.steps and score.get("restored_seq"))
    if t["state"] in ("done", "failed", "cancelled"):
        c.exported(tid, t["updated_at"])
    else:
        c.cancel(tid, "integration run ended")
    print(json.dumps(out, indent=1))
    print("PASS" if ok else "FAIL")
    shutil.rmtree(root, ignore_errors=True)
    return 0 if ok else 1


if __name__ == "__main__":
    if sys.argv[1:2] == ["child"]:
        sys.exit(W.child_main(sys.argv[2:], run=run, cleanup=cleanup, checkpoint=checkpoint))
    sys.exit(main())
