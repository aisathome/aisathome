<!-- SPDX-License-Identifier: Apache-2.0 -->
# aisathome_harness

**Experimental.** Turns an open model behind any OpenAI-compatible endpoint into machine-checked
contributions to outside projects: it runs each task as an agent session, checks the result
mechanically, keeps the evidence, and submits only where the project's maintainers have agreed.

It is the agent side of AIs@home: the node serves the model, the gateway routes requests to it, and
this package lets that model do checkable work. It works with any OpenAI-compatible endpoint, not only
an AIs@home gateway. Node operators do not need it.

## How the prove2me runs use it

The proofs on [aisathome.org/science/prove2me](https://aisathome.org/science/prove2me/) come from this
harness and a prove2me adapter on top of it. What runs, and where it is:

**The model.** Qwen3.8 27B, Unsloth's `UD-IQ4_XS` GGUF (about 14.3 GB), on llama.cpp's `llama-server`
on volunteers' 24 GB cards (RTX 3090/4090 so far): one request at a time, 196,608 tokens (192k) of
context, a `q4_0` KV cache, prompt caching on. On a long-context needle test recall was 77/80 at about
195k tokens and 62/80 at about 247k, hence the cap. From node v0.7.0 the 24 GB cards also run MTP
speculative decoding (Qwen3.8's own multi-token-prediction head, 2 draft tokens a step, about
1.7-1.9x output speed). The exact command, the model files' Hugging Face revision and their SHA-256 are in
[`node/packaging/linux/install.sh`](../node/packaging/linux/install.sh) (`set_engine`, `tier_model`);
for a 24 GB card it comes down to:

```bash
llama-server -m Qwen3.8-27B-UD-IQ4_XS.gguf --alias qwen3.8-27b -ngl all -fa on \
  -np 1 --kv-unified-per-slot 196608 -ctk q4_0 -ctv q4_0 --cache-prompt \
  --reasoning-budget 8192 --no-reasoning-preserve \
  --spec-type draft-mtp -md mtp-Qwen3.8-27B-Q4_0.gguf --spec-draft-n-max 2
```

The agent reaches it through the gateway's OpenAI-compatible API (model `agent`), so a task can run on
any node that is free.

**The agent.** [opencode](https://opencode.ai) in the sandbox below (`loop.py`, `sandbox/`): Lean 4 and
Mathlib at prove2me's versions, the model endpoint as its only way out, web tools off, a step cap and
a cap on the model's own time, fix rounds with the compiler's errors (`rounds.py`). One task is often
40-130 agent steps; about 96% of the prompt tokens come from llama.cpp's prompt cache, which is what
makes this affordable on one GPU.

**One proof, many tasks.**

1. **Outline.** A session writes the whole proof as a Lean file that compiles with a `sorry` for each
   step it has not proved yet.
2. **Holes.** `lean/holes.py` turns each `sorry` into a stand-alone lemma (Mathlib's `extract_goal`
   with its whole local context), checks that each lemma compiles on its own and that the outline
   still compiles with the lemmas in place (a "dry assembly"); a hole that does not extract is proved
   in place instead.
3. **One task per hole.** Each lemma goes to the gateway's task queue as a task of its own
   (`queue.py`, `worker.py`) and gets a fresh agent session; a lease, a checkpoint after every round
   and a give-back on a stop let a task move between machines.
4. **Assembly and checks.** `holes.assemble` puts the proofs back into the outline. Then the full
   check: a text scan and an allow-list for the proof file before anything compiles (`lean/axioms.py`,
   `lean/proofscan.py`), the build in a throwaway container with no network (`sandbox/jail.py`), and a
   kernel replay of the compiled proof against the theorem compiled without it, allowing only Lean's
   standard axioms (`lean/replay.py`). A hole that fails sends the lemma back to a new outline.
5. **Submit.** Only a proof that passes goes to prove2me, which checks it again with Lean.

**What is not in this repository yet:** the prove2me adapter itself (its prompts and playbooks
beyond the generic one in `lean/playbooks/`, the choice of lemmas, the scheduling of outline and
hole tasks, the submission client) and its sandbox image with Mathlib. So you can read and test every
piece above and run your own Lean tasks with it (below, with a worked example), but not rerun a prove2me
trial from this repository alone.

## What it does

- **One task to its end** (`task.py`): `run_task(project, task, endpoint, caps)` prepares the task, runs
  its session, checks each round and gives feedback for fix rounds, with all state in the task's evidence
  directory, so the same call continues after a stop. It is the seam a queue or scheduler calls; there is
  no global state, and tasks run side by side (see "Several sessions at once").
- **A queue worker** (`queue.py`, `worker.py`): a client for a gateway's science task queue (create, list,
  claim with a lease, heartbeat, complete, fail or give back, cancel, the public queue; the gateway URL, key
  and project passed explicitly; typed errors) and the worker's lifecycle around it: held tasks first after a
  stop, the lease renewed while a child process runs the task, the result or the crash reported, a stop
  (SIGTERM or a spot notice) that stops the child, takes its checkpoint and gives the task back with its try,
  a pause after three crashes in a row. The project supplies the child's callbacks (`child_main`: run,
  cleanup, checkpoint).
- **Checkpoints** (`checkpoint.py`, `aws.py`): a task's state, as the project lays it out (named parts),
  packed and uploaded to a store (a directory, or S3 signed with SigV4 from the standard library) after
  every round and at a give-back, restored on another machine with its hash checked and a fall-back to the
  previous checkpoint after a cut upload; the evidence directory synced file by file; the spot interruption
  notice from the instance metadata service.
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
  endpoint (certificate checked), only for the model calls (`POST <base>/chat/completions`,
  `GET <base>/models`; any other path or method is refused): the key never enters the model's container
  and opens nothing else the endpoint serves, such as a gateway's task API. All capabilities dropped, no new privileges, memory and process caps, per-session
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
  adds, a scan for constructs a proof must not use, an allow-list for a proof file (`lean.proofscan`:
  declarations only, no attribute, syntax extension, instance or code run while it compiles), a Lean
  command that checks a theorem's type and axioms, a kernel replay of a compiled proof against a target
  compiled without it (`lean.replay`), and a generic proof playbook.
- **Builds of untrusted text in a jail** (`sandbox/jail.py`): Lean runs code while it elaborates a file
  (an elaborator, macro or initialiser the file registers itself), so a project's own build or check of a
  model's text runs in a throwaway container: no network, read-only root with a tmpfs at /tmp, every
  capability dropped, a uid that is neither root nor the invoking user, CPU, memory, process and
  file-size limits, no inherited environment, only the mounts the caller lists.
  `LeanToolchain.jail(image, workspace)` sets one up for Lake (elan and the package farm read-only).
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

You need Linux, Python 3.12, Docker (your user in the `docker` group), a sandbox image with opencode v2,
python3 and CA certificates, and an OpenAI-compatible endpoint: its base URL, a model name and, if it
wants one, a file holding the API key. The key file is mounted only into the egress forwarder's
container, which runs as your user (so a key file only you can read works) and adds the key to each
request; it never enters the model's container and is never written to logs or config.

### The Lean example, step by step

[`examples/lean/`](examples/lean/) is the template filled in for one Lean exercise (`a + b = b + a` on
`Nat`, Lean's core only, no Mathlib): the model works on it in the sandbox, and the check builds its file
in a jail and checks the theorem's type and axioms. These steps were run as written on a Linux desktop,
CPU only: with the 0.5B model below every step runs and the model proves nothing (expected at that size);
a scripted endpoint that makes the one edit gets `proved`.

1. **The harness and the example**, at a release tag:

   ```bash
   git clone --depth 1 --branch harness-v0.3.0 https://github.com/aisathome/aisathome
   python3.12 -m venv .venv && . .venv/bin/activate
   pip install ./aisathome/harness
   cp -r aisathome/harness/examples/lean lean-example && cd lean-example
   ```

2. **Lean**: elan, and the toolchain the exercise names, installed now. The sandbox and the jail mount
   elan read-only and have no network, so elan can neither download a toolchain there nor write its
   settings file (hence a default toolchain, which writes it):

   ```bash
   curl -fsSL https://raw.githubusercontent.com/leanprover/elan/master/elan-init.sh \
     | sh -s -- -y --no-modify-path --default-toolchain leanprover/lean4:v4.33.1
   ~/.elan/bin/elan toolchain install leanprover/lean4:v4.33.1
   ```

3. **The sandbox image** (opencode v2.4.1, checked against its release's SHA256SUMS):

   ```bash
   docker build -t harness-lean-example .
   ```

4. **An endpoint.** Here a small model in llama.cpp's server image, on the Docker network the harness
   uses for its egress forwarders, under the host name `tiny-llm` (the network is created the way the
   harness creates it, so it can go first):

   ```bash
   docker network create -o com.docker.network.bridge.name=hsb-egress0 --label harness.network=1 harness-egress-out
   mkdir -p models && curl -fL -o models/qwen2.5-0.5b-instruct-q4_k_m.gguf \
     https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf
   docker run -d --name tiny-llm --network harness-egress-out --network-alias tiny-llm \
     -v "$PWD/models:/models:ro" ghcr.io/ggml-org/llama.cpp:server-b12723 \
     -m /models/qwen2.5-0.5b-instruct-q4_k_m.gguf --alias tiny --host 0.0.0.0 --port 8080 -c 32768 --jinja
   ```

   For real work, the same with your model on a GPU: the `server-cuda-b12723` image, `--gpus all`, and
   the flags of the command at the top of this page.

5. **Run the task:**

   ```bash
   LEAN_EXAMPLE_ELAN=~/.elan python3 adapter.py run hello --base-url http://tiny-llm:8080/v1 --model tiny
   ```

   It prints each round and ends with the verdict (`proved`, `compile_error`, `refused`,
   `check_failed` or `no_solution`). The evidence lands in `raw/hello/` (`rounds.json` ends with the
   final verdict; `events.jsonl` is everything opencode did); the working files in `work/hello/`. A
   finished task does not run again: `python3 adapter.py clean hello` removes both (overlayfs leaves
   root-owned folders in `work/` that a plain `rm -rf` cannot remove).

**How the sandbox reaches the endpoint.** Only through its forwarder, by host name, so a base URL with
an IP address or `localhost` is refused. The forwarder resolves the name through Docker's DNS on
`harness-egress-out`: another container on that network with a `--network-alias` (as above), or a name
in DNS, as the network's own runners reach `https://api.aisathome.org/v1`. A `llama-server` started on
the host with `--host 127.0.0.1` cannot be reached; run it in a container as above. Plain HTTP is fine.
If anyone else can reach the endpoint, give it a key (`llama-server --api-key-file key.txt`, mounted
into its container) and pass the same file with `--key-file`.

**opencode.** The harness runs `opencode run --standalone --auto --format json`, which needs opencode
v2; the v1.x releases lack `--standalone`. v2.4 also offers tools the harness does not turn off
(`execute`, which runs JavaScript, `question`, `subagent`, image and video generation); a small model
may call `execute` with a shell command instead of `shell`.

### Your own project

1. Copy `template/adapter.py` and `template/PROJECT.md` (or `examples/lean/`) into your project's own
   folder and fill in the steps: what the tasks are, how to set up the tree, the prompt, the check.
   A Lean project needs a `lake-manifest.json` in the tree and in the check's workspace (Lake cannot
   write one in a jail) and `import Lean` in its check file (for `lean.axioms.check_command`).
2. Make the package importable: `pip install "aisathome-harness @ git+https://github.com/aisathome/aisathome@harness-v0.3.0#subdirectory=harness"`
   or put this folder on `PYTHONPATH`.
3. Run a task as in step 5, with your endpoint: `--base-url https://api.example.org/v1 --key-file
   ~/.config/endpoint.key --model MODEL`.

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
`tests/integration_queue.py` runs the queue worker and checkpoints against a real (test) gateway: a toy task is
claimed, stopped halfway and given back, then resumed from its checkpoint by a worker with its own directories.

## Releases

Tags `harness-v<version>` (the node's releases are `v<version>`).
