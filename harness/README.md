<!-- SPDX-License-Identifier: Apache-2.0 -->
# aisathome_harness

**Experimental.** Turns an open model behind any OpenAI-compatible endpoint into machine-checked
contributions to outside projects: it runs each task as an agent session, checks the result
mechanically, keeps the evidence, and submits only where the project's maintainers have agreed.

It is the agent side of AIs@home: the node serves the model, the gateway routes requests to it, and
this package lets that model do checkable work. It works with any OpenAI-compatible endpoint, not only
an AIs@home gateway. Node operators do not need it.

## What it does

- **One task to its end** (`task.py`): `run_task(project, task, endpoint, caps)` prepares the task, runs
  its session, checks each round and gives feedback for fix rounds, with all state in the task's evidence
  directory, so the same call continues after a stop. It is the seam a queue or scheduler calls; there is
  no global state, and tasks run side by side (see "Several sessions at once").
- **One session** (`loop.py`): [opencode](https://opencode.ai) runs in a hardened container and talks to
  the endpoint. The session has a step cap and a cap on the model's own time (from each step's first
  streamed chunk to its end, so time spent waiting in the endpoint's queue does not count). It is
  checkpointed while it runs and can be resumed after a machine stop, a suspend or an endpoint error.
  Two nudges, each sent once: write a first version of the target when none exists after N steps, and
  stop repeating a command that keeps returning the same empty or failing result (`loop_guard.py`).
- **Fix rounds** (`rounds.py`): check, then feedback in the same session, then check again, for a fixed
  number of rounds. Rounds that the endpoint's failures consumed are kept apart from the model's own
  results and run again once.
- **Evidence** (`evidence.py`, `stepmetrics.py`, `behaviour.py`): one directory per task with the
  prompt, opencode's events, per-step metrics, the model's diff, run.json and rounds.json. After each
  session the endpoint's key is masked in every evidence file, as a second wall.
- **The sandbox** (`sandbox/`): per session, an internal Docker network with no route out and no other
  session on it; the only exit is a forwarder to that session's endpoint (one per endpoint and key). The
  model's client speaks plain HTTP to the forwarder, which adds the API key and speaks TLS to the
  endpoint (certificate checked), only to the endpoint's base path: the key never enters the model's
  container. All capabilities dropped, no new privileges, memory and process caps, per-session
  copy-on-write overlays for shared build caches, web tools off. A `Recipe` says what a project needs in
  the container (image, toolchain mounts, environment, commands that take a build slot).
- **The host** (`host.py`): the machine's limits and its shared build slots, used by the sandboxes and by
  host-side builds alike, so concurrent sessions queue for the CPUs instead of overcommitting them.
- **The model's files stay the model's** (`safefs.py`, `hostgit.py`): the host reads and writes the
  sandbox HOME and the task's tree only without following symlinks, and runs git on the tree only with a
  git directory of its own, so neither a symlink nor the tree's own git config, hooks or attributes can
  make the host read, write or run anything outside the sandbox.
- **The Lean kit** (`lean/`): a toolchain description for host builds and sandboxes, Lake package sets
  shared between tasks with hard links, a Lean lexer for the text checks (comments, strings, raw strings,
  char literals, `«»` names, interpolation), a check for `sorry`/`admit`/`native_decide` in the code a diff
  adds, a scan for constructs a proof must not use, a Lean command that checks a theorem's type and
  axioms, and a generic proof playbook.
- **The project contract** (`project.py`): a project is an adapter (select, workspace, prepare, session,
  check, feedback, submit) plus a `PROJECT.md` whose status block says `active`, `paused`, `local-only`
  or `ended`. `submit` refuses unless the status is `active` **and** a maintainers' agreement is recorded.

## Several sessions at once

Nothing assumes one session, one GPU or one model server. Every name and path a session uses is derived
from its task: the container and network names from the sandbox key (`<project>/<task>`), the overlay
volumes and generated files from the task's host-only `state` directory, the evidence from its `raw`
directory (one session per evidence directory at a time, enforced by a lock). Endpoints are per call, so
tasks can use different endpoints and models. What several sessions on one host share is deliberate:
the build slots and the egress forwarders (one per endpoint), under the host's state directory
(`AISATHOME_HARNESS_STATE`, default `~/.local/state/aisathome-harness`).

On a shared host, a firewall should also drop traffic from the session bridges (`hsb*`) to the host and to
the cloud's instance metadata service; the sandbox's own network has no route out either way.

## Run one task

You need Python 3.12, Docker, an image with opencode, python3, CA certificates and your project's
toolchain, and an endpoint: its base URL (e.g. `https://api.example.org/v1`), a model name, and a file
holding the API key. The key file is mounted only into the egress forwarder's container, which runs as
your user (so a key file only you can read works) and adds the key to each request; it never enters the
model's container and is never written to logs or config.

1. Copy `template/adapter.py` and `template/PROJECT.md` into your project's own folder and fill in the
   steps: what the tasks are, how to set up the tree, the prompt, the check. The endpoint must be a host
   name, not an IP address (the sandbox reaches it through its forwarder, by name).
2. Make the package importable: `pip install "aisathome-harness @ git+https://github.com/aisathome/aisathome@harness-v0.1.2#subdirectory=harness"`
   or put this folder on `PYTHONPATH`.
3. Run a task:

   ```bash
   python3 adapter.py run example-1 --base-url https://api.example.org/v1 --key-file ~/.config/endpoint.key --model MODEL
   ```

   The evidence lands in `raw/example-1/`; `rounds.json` ends with the task's final verdict.

The template keeps the project in `local-only` status: it never submits. Set `status: active` and
record the agreement only when the project's maintainers have said yes to agent contributions.

## Tests

Offline, standard library only:

```bash
cd harness && uv run --no-project --python 3.12 python -m unittest discover -s tests -t .
```

`tests/fake_opencode.py` stands in for opencode, so the session loop is tested end to end without a
container, a model or a network (`tests/test_loop_characterisation.py` holds the loop to reference
records of scripted sessions). Tests that need real Docker or Lean belong to a project's own suite.

## Releases

Tags `harness-v<version>` (the node's releases are `v<version>`).
