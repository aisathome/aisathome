# AIs@home

Volunteer computing for open-weight LLMs, in the spirit of BOINC: people run Qwen3.8 27B on their own
NVIDIA GPUs at home, and the network uses it for maths that a machine checks, a free chat and an
OpenAI-compatible API. Non-commercial (no project token, nobody is paid), open source (Apache-2.0), and
an early experiment: a small network, not production-ready.

[aisathome.org](https://aisathome.org) · [Run a node](#run-a-node) ·
[prove2me results](https://aisathome.org/science/prove2me/) · [Dev log](https://aisathome.org/devlog/) ·
[Agent harness](harness/README.md) · [Architecture](docs/architecture.md)

## What the GPUs do

Qwen3.8 27B (Unsloth `UD-IQ4_XS`, 192k context, llama.cpp) works as an agent on
[prove2me](https://prove2.me/), which states lemmas from real maths papers in Lean 4 and checks every
proof it gets with Lean itself. As of 25 November 2026 prove2me has accepted 16 proofs from the
network: the model proved 12 of the 42 open lemmas it tried and closed 3 more by reusing a proof
already on prove2me. All runs together took 239 GPU-hours on RTX 3090 and 4090 cards. With the
current pipeline it proves most of the easier lemmas it tries (finite sums, induction, some linear
algebra); of the harder ones (analysis, long chains of reasoning) it has proved none of 14 so far.

- The live count, a chart and every attempt: [aisathome.org/science/prove2me](https://aisathome.org/science/prove2me/)
- The setup, what a proof costs and what fails: [devlog 5](https://aisathome.org/devlog/5-qwen-proofs-on-consumer-hardware/)
- How an attempt runs (an outline, one agent task per `sorry`, assembly, Lean and kernel checks):
  [`harness/`](harness/README.md#how-the-prove2me-runs-use-it)

## Run a node

An NVIDIA GPU with 24 GB (RTX 3090, 4090, 5090 class) or 16 GB (RTX 4080, 5070 Ti, 5080 class; slower
16 GB cards such as the RTX 4060 Ti fail the speed test), on Linux or Windows 10/11. No account needed
to start: a node without an account does science only, never people's chats. Link it to an account
later and it serves chats too and earns credit (recognition, not money).

**Linux.** [Read the script](site/public/node.sh) first: it downloads the latest release, checks it
against its `SHA256SUMS`, checks your driver, card and disk, and asks before it installs anything
(it needs sudo).

```bash
curl -fsSL https://aisathome.org/node.sh | sh
```

**Windows.** [AIsAtHome-node-setup.exe](https://github.com/aisathome/aisathome/releases/latest/download/AIsAtHome-node-setup.exe)
from the latest release; on its setup page choose **Start without an account (science only)** or
link it to your account. It is not code-signed yet, so Windows warns about an unknown publisher.

Step by step, what it installs, and how to stop or remove it: [aisathome.org/node](https://aisathome.org/node/).

## Use it

- **Chat:** [chat.aisathome.org](https://chat.aisathome.org), sign in with GitHub or Google. Accounts
  are limited while the network is small; new ones get in as room appears.
- **API:** OpenAI-compatible at `https://api.aisathome.org/v1`, with a key from your
  [account page](https://aisathome.org/account). Models: `deep` (Qwen3.8 27B, general chat) and
  `agent` (the same model, for coding agents such as opencode, and tool calls). Each account has a
  daily limit. Guide: [aisathome.org/use/api](https://aisathome.org/use/api/).

![How a proof runs on the network: volunteer GPU nodes, the gateway, Lean runners and prove2me](site/public/images/devlog/5-qwen-proofs-pipeline.svg)

*How a prove2me proof runs on the network ([devlog 5](https://aisathome.org/devlog/5-qwen-proofs-on-consumer-hardware/)).*

## How it works

Volunteers run a node on their own GPU; a gateway (the project
server) takes requests from the chat and the OpenAI-compatible API and routes them to
nodes. Anyone can run a gateway. Site and docs: <https://aisathome.org>.

**Hardened, not confidential.** Whoever runs a node can read the prompts it serves. The
gateway strips identities, hides well-known secret formats, sends requests that look like
personal data only to the requester's own nodes (otherwise it refuses them), and stores no prompts or answers (only the short piece of
a volunteer's answer that a guard stopped or flagged, for 14 days).

## Science

The network can run an open model as an agent on outside projects whose results a machine can
check, such as Lean proofs, and submits only where the project's maintainers have agreed. prove2me
is the current one; before it, Tau Ceti (finished, at its maintainers' request). The agent harness
is in [`harness/`](harness/README.md); every project and its results:
[aisathome.org/science](https://aisathome.org/science/).

## Run the agent harness

The harness runs any OpenAI-compatible endpoint as an agent in a sandbox and checks what it wrote; a
Lean example runs end to end on a CPU with a small model in Docker, no GPU and no account:

```bash
git clone --depth 1 --branch harness-v0.3.0 https://github.com/aisathome/aisathome
python3.12 -m venv .venv && . .venv/bin/activate && pip install ./aisathome/harness
cp -r aisathome/harness/examples/lean lean-example && cd lean-example
```

then Lean (elan), the sandbox image, an endpoint and one command:
[harness/README.md, "Run one task"](harness/README.md#run-one-task).

## Repository

| Path | What |
|---|---|
| `node/` | `aisathome-node`: runs next to a local `llama-server` on a volunteer's machine and connects **outward** to a gateway (no open ports). Local status page and `aisathome-node status`. Linux installer in `node/packaging/linux/` |
| `gateway/` | `aisathome-gateway`: the OpenAI-compatible API, routing with queueing and failover, accounts and sign-in, self-registered volunteer nodes, metering and credit |
| `protocol/` | The gateway ↔ node protocol (`PROTOCOL.md`) and its Go types |
| `site/` | The website and docs (Astro + Starlight) |
| `harness/` | `aisathome_harness` (Python): runs an open model behind any OpenAI-compatible endpoint as an agent on outside projects' tasks, checks the results mechanically and keeps the evidence ([harness/README.md](harness/README.md)) |
| `boinc/` | The node agent as a BOINC app, and `aisathome-bridge`, which connects a BOINC project to a gateway; first version, not yet live ([boinc/README.md](boinc/README.md)) |
| `e2e/` | End-to-end tests (user → gateway → node → mock engine) |
| `tools/sitestats/` | The website's visitor statistics: daily counts from the web server's log, no cookies, no IPs kept (what it counts: `doc.go`) |
| `docs/` | [How the pieces fit together](docs/architecture.md) (components, request and task flows, trust boundaries); for operators and maintainers: [running a gateway](docs/gateway.md), [credit](docs/credit.md), [releasing](docs/RELEASING.md) |
| `scripts/`, `examples/` | Release and bundle builds, test fixtures; example configs |

## Contributing

Run a node, report bugs, or send code: [CONTRIBUTING.md](CONTRIBUTING.md) has the dev setup,
the tests, the pull-request flow and how to report a security problem. Start with
[docs/architecture.md](docs/architecture.md) for how the gateway, node, protocol, harness and
site fit together, and who can see what.

## The node in detail

Every node runs one request at a time with llama.cpp's `llama-server`, and the model file and
the context follow the card: `UD-Q3_K_XL` with 80k on 16 GB cards (a light desktop may stay on
them), `UD-IQ4_XS` with 192k on 24 GB and more, both with a `q4_0` KV cache. From v0.7.0, 24 GB
cards also run MTP speculative decoding (Qwen3.8's own multi-token-prediction head; about
1.7-1.9x output speed, the same output distribution) unless you turn it off. The installer pins
each model file to a Hugging Face commit and its SHA-256, and `aisathome-node modelcheck` checks
your engine's answers against a published reference. The Linux installer, its options (from an
unpacked bundle, a node token for scripts, speculation) and removal:
[node/packaging/linux/README.md](node/packaging/linux/README.md); Windows and everything else:
[aisathome.org/node](https://aisathome.org/node/).

## Quick start (local development)

```bash
go build -o aisathome-node ./node/cmd/aisathome-node
go build -o aisathome-gateway ./gateway/cmd/aisathome-gateway
./aisathome-gateway -config examples/gateway.json
./aisathome-node -config examples/node.json   # next to llama-server on :8000
curl -H "Authorization: Bearer dev-user-key-change-me" http://localhost:8080/v1/chat/completions \
  -d '{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Tests: `go vet ./... && go test -race ./...`; the installer has `node/packaging/linux/install_test.sh`;
the harness: `cd harness && python3 -m unittest discover -s tests -t .` (Python 3.12, standard library).
Everything CI runs: [CONTRIBUTING.md](CONTRIBUTING.md#tests).

## Run a gateway

Config, API keys, accounts and sign-in, volunteer nodes and their protections, email
sign-in: [docs/gateway.md](docs/gateway.md). Credit and the BOINC-format statistics:
[docs/credit.md](docs/credit.md).

## Releases

A `v*` tag builds the node and gateway binaries, `llama-server` with CUDA, the node bundle
and `SHA256SUMS` into a draft GitHub release; a manual run of the release workflow builds
without publishing. The harness is versioned apart, with `harness-v*` tags that build nothing.
What a release contains and how it is built:
[aisathome.org/node/binaries](https://aisathome.org/node/binaries/); how to make one:
[docs/RELEASING.md](docs/RELEASING.md).

## Licence

Apache-2.0 (see `LICENSE`). Each source file carries an SPDX header. The prebuilt bundle
also contains third-party software under its own terms (`node/packaging/linux/THIRD_PARTY_NOTICES.md`).
The name "AIs@home" is not licensed with the code.
