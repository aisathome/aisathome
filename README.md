# AIs@home

**Experimental, in pilot.** Open-weight AI models served by volunteers' GPUs instead of a big
company's data centre. Volunteers run a node on their own GPU; a gateway (the project
server) takes requests from the chat and the OpenAI-compatible API and routes them to
nodes. Anyone can run a gateway. Site and docs: <https://aisathome.org>.

**Hardened, not confidential.** Whoever runs a node can read the prompts it serves. The
gateway strips identities, hides well-known secret formats, sends requests that look like
personal data only to trusted nodes, and stores no prompts or answers. Not
production-ready.

## Use it

- **Chat:** [chat.aisathome.org](https://chat.aisathome.org). Accounts are limited during the
  pilot; new ones get in as room appears.
- **API:** OpenAI-compatible at `https://api.aisathome.org/v1`, with a key from your
  [account page](https://aisathome.org/account). Models: `deep` (Qwen3.8 27B, general chat) and
  `agent` (the same model, for coding agents and tool calls). Each account has a daily limit.
  Guide: [aisathome.org/use/api](https://aisathome.org/use/api/).

## Science

The network can also run an open model as an agent on outside projects whose results a machine
can check, such as Lean proofs. The agent harness is in [`harness/`](harness/README.md).

## Repository

| Path | What |
|---|---|
| `node/` | `aisathome-node`: runs next to a local `llama-server` on a volunteer's machine and connects **outward** to a gateway (no open ports). Local status page and `aisathome-node status`. Linux installer in `node/packaging/linux/` |
| `gateway/` | `aisathome-gateway`: the OpenAI-compatible API, routing with queueing and failover, accounts and sign-in, self-registered volunteer nodes, metering and credit |
| `protocol/` | The gateway ↔ node protocol (`PROTOCOL.md`) and its Go types |
| `site/` | The website and docs (Astro + Starlight) |
| `harness/` | `aisathome_harness` (Python): runs an open model behind any OpenAI-compatible endpoint as an agent on outside projects' tasks, checks the results mechanically and keeps the evidence ([harness/README.md](harness/README.md)) |
| `e2e/` | End-to-end tests (user → gateway → node → mock engine) |
| `tools/sitestats/` | The website's visitor statistics: daily counts from the web server's log, no cookies, no IPs kept (what it counts: `doc.go`) |
| `docs/` | For operators and maintainers: [running a gateway](docs/gateway.md), [credit](docs/credit.md), [releasing](docs/RELEASING.md) |
| `scripts/`, `examples/` | Release and bundle builds, test fixtures; example configs |

## Run a node

Linux, an NVIDIA GPU with 16 GB or more and the NVIDIA driver. Get a node token on your
[account page](https://aisathome.org/account), download the prebuilt bundle from the releases
and run `sudo ./install.sh`. Every node runs one request at a time; the model file and the
context follow the card: `UD-Q3_K_XL` with 80k on 16 GB cards (a light desktop may stay on
them), `UD-IQ4_XS` with 192k on 24 GB and more. Step by step:
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
the harness: `cd harness && python3 -m unittest discover -s tests` (Python 3.12, standard library).

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
