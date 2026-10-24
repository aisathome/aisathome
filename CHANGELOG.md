# Changelog

What changed in each release, for people who run a node or use the API. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the git tags. Each
entry says where it applies: **(node)** needs the new release on your machine, **(gateway)**
and **(site)** are live on aisathome.org once deployed.

## [Unreleased]

### Added

- **Protocol versions between node and gateway.** A node says which protocol version it speaks
  when it connects; nodes up to v0.3.0 send none and count as version 1. The gateway accepts
  its own version and the one before it (`node_protocol_min`, `node_protocol_max`), so the
  current and the previous node release keep working, logs each node's version and shows it on
  the account page, and refuses other versions with a message that says what to do.
  (gateway, live since 2026-10-24)
- **The node agent speaks protocol version 2.** If a gateway refuses its version, the agent
  shows the gateway's reason in its local UI and in `aisathome-node status`, and tries again
  after 10 minutes instead of every few seconds. It still connects to gateways from before
  protocol versions. (node)

### Fixed

- **A chat no longer fails while the network does science.** A chat or agent request that has
  waited 8 seconds for a node now stops the most recent science request on a node it can use and
  takes its place; before, a chat could fail with "all nodes serving deep are busy" while the
  only node ran a long science step. People's requests are never stopped. A preempted science
  request without output yet is sent again by the gateway; otherwise its stream ends with a
  retryable error (`code` 503, `reason` "preempted"). `/v1/stats` counts `preempted` per class
  and project. Setting `preempt_after_seconds` (default 8, `-1` off). (gateway, live since
  2026-10-24)
- **`logprobs` in non-streaming answers.** A request with `"logprobs": true` now gets them in
  `choices[0].logprobs.content`, as OpenAI returns them; the gateway used to drop them
  (streamed answers already carried them). Answers to requests that did not ask for logprobs
  no longer carry any a node sent anyway. (gateway, live since 2026-10-24)
- The gateway's own token count of an answer (for credit) no longer stops early when the
  answer carries logprobs. (gateway, live since 2026-10-24)

## [v0.3.0] — 2026-10-24

One model for every node, 16 GB cards can join, and the network starts doing science.

### Added

- **16 GB cards can join.** Nodes now run Qwen3.8 27B `UD-IQ4_XS` (14.3 GB) instead of
  `UD-Q4_K_M` (16.5 GB), with no measurable loss in our tests. The context follows the card:
  **80k on 16 GB cards** (RTX 4080, 5070 Ti, 5080 class), which must be **headless** (no screen
  on that card, or under about 650 MB of desktop), and **192k on 24 GB and more** (was 128k).
  (node)
- **Science on the network:** [aisathome.org/science/tauceti/](https://aisathome.org/science/tauceti/)
  lists every pull request Qwen3.8 27B has made to [Tau Ceti](https://taucetiproject.org/), an
  open library of formal mathematics, grouped by type, with Tau Ceti's review as the judge,
  tokens and GPU time per PR, failures included, and the data as JSON. The home page shows the
  merged count. (site)
- **Workload classes:** every request is `chat`, `agent` (your API key) or `science` (an
  operator-run project). When the network is busy, waiting chat requests go first, then agents,
  then science; science never takes a node a waiting person could use, and running requests
  are never stopped. **Under load, API requests now wait behind chat requests.** (gateway)
- **Science projects:** operator-configured projects with their own keys, their own daily
  quota instead of an account's, a cap on their share of nodes, and their own counts in
  `/v1/stats`. (gateway)
- **Daily history** at `/v1/stats/history` (requests, tokens, per class, nodes online), shown
  as charts on [the network page](https://aisathome.org/network/). (gateway, site)
- **Speed test at install:** after a 32,000-token prompt the card must write at least 20
  tokens/s, or the installer stops the node and says why. Slow 16 GB cards (RTX 4060 Ti,
  5060 Ti) don't pass. (node)
- **Per-request thinking limits:** an API request can set `thinking_budget_tokens` (or
  `reasoning_budget_tokens`, 0 to 32,768) and `reasoning_budget_message`. Invalid values get
  `400`. (gateway)
- **Community:** [r/aisathome](https://www.reddit.com/r/aisathome/), linked from the footer,
  the About page and every dev log post. (site)
- **Dev log** at [aisathome.org/devlog/](https://aisathome.org/devlog/), with an RSS feed, and
  visitor statistics without cookies (counted from the web server's log; no IP addresses or
  browser names kept). (site)

### Changed

- **The installer checks free GPU memory for the tier** and stops with a clear message if a
  screen or programs use too much; it never shrinks the context. `--model` accepts only the
  network's pinned file. (node)
- **Easier updates:** `install.sh --keep-token` (or Enter at the token prompt) keeps the token,
  node id and gateway. The update to this release downloads the new 14.3 GB model while the
  node keeps serving (about 16 GB free in `/var/lib` needed), then restarts. (node)
- **More nodes per volunteer:** up to 10 per account (was 3), 10 connected from one address
  (was 3), 25 in the network (was 10). (gateway)
- **Account page:** a new key or node token gets its own page with Copy and Done; at a limit
  the page says so instead of hiding the form. Your nodes show their version. (gateway)

### Fixed

- **The installer's checks for "ran on the CPU" and "out of GPU memory"** could pass silently
  when the service log was long. (node)
- **Updates on machines with two GPUs** could pick the wrong card and refuse the update. (node)
- **The gateway warns** when two clients keep replacing each other on one node token. (gateway)

### Updating a node

**Every node must update:** the network does not mix model files, and nodes on the old model
fail the model check.

```
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.3.0/aisathome-node-bundle-linux-amd64.tar.xz
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.3.0/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
sudo ./install.sh --keep-token
```

The update keeps your token and node id, downloads the new model while the node keeps serving,
and then restarts. Step by step: [Update your node](https://aisathome.org/node/update/).

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
