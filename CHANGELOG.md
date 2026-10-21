# Changelog

What changed in each release, for people who run a node or use the API. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the git tags. Each
entry says where it applies: **(node)** needs the new release on your machine, **(gateway)**
and **(site)** are live on aisathome.org once deployed.

## [Unreleased]

- **Dev log** at [aisathome.org/devlog/](https://aisathome.org/devlog/), with numbered posts,
  an RSS feed and the latest posts on the home page. (site, live on aisathome.org)
- **Visitor statistics without cookies:** daily page views and an estimate of visitors, counted
  from the web server's log; no IP addresses or browser names are kept. What is counted is
  described in `tools/sitestats/doc.go`. (site)
- **Node settings explained:** the preset docs link to the exact lines in `install.sh` that set
  each value. (docs)
- **Per-request thinking limits:** an API request can set `thinking_budget_tokens` (or
  `reasoning_budget_tokens`, 0 to 32,768) and `reasoning_budget_message`, the text the model
  is given when the limit is reached. Invalid values get `400`. (gateway)
- **Easier updates:** run the new bundle's `install.sh --keep-token` (or press Enter at the
  token prompt) and it keeps the installed token, node id and gateway; before, a rerun asked
  for the token again and reset the gateway and, for tokens from a gateway's config, the node
  id. New docs page: [Update your node](https://aisathome.org/node/update/). (node, docs)
- **Your nodes shows each node's version** while it is connected, and the gateway logs it.
  (gateway)
- **The gateway warns** when two clients keep replacing each other's connection on one node
  token and id. (gateway)
- **Local UI:** "Time serving" shows "<1s" instead of "0s" for less than a second, and the
  lifetime counters are headed "Total" instead of "Since install". (node)

## [v0.2.0] — 2026-10-20

A release for running nodes reliably and for long agent sessions.

### Added

- **Cache-aware routing:** a conversation goes back to the node that already holds its
  context, so long agent sessions don't re-read 50–100k tokens when they switch node.
  (gateway, live on aisathome.org)
- **The node reports the release version**, shown on the local status page and to the
  gateway. (node)
- **Docs:** running BOINC on the same GPU (`exclusive_gpu_app`), uninstalling, tested
  systems. (node)

### Changed

- **Never left running on the CPU:** if the GPU isn't usable, the node stops instead of
  serving slowly on the CPU. (node)
- **Credit only for what reached you:** a node is credited only for output that was actually
  delivered. (gateway, live on aisathome.org)
- **Safety:** a node flagged by the gateway's checks is held *before* it can take another
  request, and the hold can't be lifted by accident. (gateway, live on aisathome.org)

### Fixed

- **Newer NVIDIA drivers (545 and later) work:** the installer allows the GPU under both its
  old and new device names. Tested on driver 575 (RTX 3090) and 530 (RTX 4090). (node)
- **Installed libraries are readable** whatever the file modes in the bundle (fixes a crash
  loop after installing with a restrictive umask). (node)
- **Reliability:** no lost answers when a node disconnects right after finishing; a cancelled
  request keeps its slot until the node's engine has stopped; clearer errors when an agent
  request has nowhere to go. (gateway, live on aisathome.org)

### Updating a node

```
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.2.0/aisathome-node-bundle-linux-amd64.tar.xz
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.2.0/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
sudo sed -n 's/^AISATHOME_NODE_TOKEN=//p' /etc/aisathome-node/token.env | sudo ./install.sh
```

The last line reuses your existing node token; the model already on disk is reused.

## [v0.1.0] — 2026-10-13

The first release: a volunteer node you can install on a Linux PC with an NVIDIA GPU, and
the gateway that runs aisathome.org.

#### Node
- **Prebuilt bundle:** the node agent and `llama-server` (llama.cpp v0.5.0, CUDA 12.8,
  RTX 30, 40 and 50 series with 24 GB) with the CUDA libraries it needs. Only the NVIDIA
  driver is required (525.60.13 or newer; 570.26 for an RTX 50). Or build llama.cpp
  yourself and install from a release.
- **Installer** (`install.sh`): checks the driver, GPU and disk first (`--dry-run` installs
  nothing), downloads the pinned model (Qwen3.8 27B `UD-Q4_K_M`, 16.5 GB, one exact file,
  checked against its SHA-256, resumes if interrupted) and installs two systemd services
  under their own unprivileged, sandboxed user. `uninstall.sh` removes it (`--purge` the
  model too).
- **One setting for the whole network:** one request at a time with a 128k context
  (KV cache `q4_0`).
- **Outbound only:** the node dials the gateway; nothing listens on a public port.
- **Local status page** at http://127.0.0.1:7381/ and `aisathome-node status`: connection,
  model, GPU memory and temperature, requests and tokens served. Never prompts or answers.
  **Pause** stops new requests while the model stays loaded.
- **Looks after itself:** a watchdog restarts a hung engine, the node stops before suspend
  and starts again after resume. The installer stops with a message if the engine started
  on the CPU instead of the GPU.
- **Privacy hardening:** no core dumps, no swap, a smaller prompt cache in RAM, no prompt
  logging.

#### Gateway (live on aisathome.org)
- **Chat and API:** the chat at chat.aisathome.org and an OpenAI-compatible API at
  `https://api.aisathome.org/v1` (models `deep` and `agent`), with streaming, tool calls,
  a queue when all nodes are busy and failover between nodes.
- **Accounts:** sign in with GitHub, Google or an email code; up to three API keys per
  account; daily limits per account; a waitlist when the network is full.
- **Volunteer nodes:** get a node token on your account page and the node registers itself.
- **Credit and statistics:** credit for the GPU time a node gives, counted by the gateway
  with the model's own tokenizer (BOINC's unit, not money, no token); an opt-in
  leaderboard; statistics files in BOINC's format at `/stats/`.
- **Privacy:** no prompts or answers stored; identities stripped before a node sees a
  request; well-known secret formats replaced by placeholders; requests that look like
  personal data stay on trusted nodes or your own.
- **Safety:** requests with tools reach volunteer nodes only with the key's consent, and
  their answers are checked for crude attacks (download-and-execute, credential paths,
  unknown addresses); a node that trips a check is held.

#### Installing a node
```
base=https://github.com/aisathome/aisathome/releases/download/v0.1.0
curl -LO $base/aisathome-node-bundle-linux-amd64.tar.xz -LO $base/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
./install.sh --dry-run && sudo ./install.sh
```
Paste the node token from your account page when asked. Step by step:
https://aisathome.org/node/

**Full changelog:** https://github.com/aisathome/aisathome/commits/v0.1.0

[Unreleased]: https://github.com/aisathome/aisathome/compare/v0.2.0...main
[v0.2.0]: https://github.com/aisathome/aisathome/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/aisathome/aisathome/commits/v0.1.0
