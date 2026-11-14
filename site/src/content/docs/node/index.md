---
title: Run a node
description: Give your GPU to AIs@home. What you need, what the node does, and what you are responsible for.
---

A node is a computer with a strong NVIDIA GPU that serves the model to AIs@home users. It
runs two services: `llama-server` (the model, on `127.0.0.1` only) and `aisathome-node`, a
small agent that connects **outward** to the gateway and passes requests through.

Running a node is **voluntary and unpaid**. There is no token and no payment. Your node
earns [credit](/node/credit/) in BOINC's unit: recognition, not money.

## What you need

- An NVIDIA GPU with **16 GB or more**. The context depends on the card
  ([model settings](/node/presets/)):
  - **16 GB** (RTX 4080, 5070 Ti, 5080 class): 80k context with the 3-bit model file
    (`UD-Q3_K_XL`), and at most a **light desktop** on that card: about 1.5 GB of GPU memory
    for the screen and other programs. A heavier desktop needs the screen on the
    motherboard's graphics;
  - **24 GB and more** (RTX 3090, 4090, 5090 class): 192k context with the 4-bit file
    (`UD-IQ4_XS`).
- A card fast enough on long prompts: the installer measures it and needs at least
  20 tokens/s after a 32,000-token prompt (slower 16 GB cards such as the RTX 4060 Ti and
  5060 Ti do not pass).
- Linux with systemd, or Windows 10 or 11 (64-bit), and the NVIDIA driver (`nvidia-smi`
  works).
- About 17 GB of disk for the model. With the prebuilt bundle, the install is mostly the
  13-14 GB download; building llama.cpp yourself takes about an hour.
- A **node token**: sign in on [your account page](/account) and choose **Add a node**
  (up to 10 per account; at most 10 of them connected from one internet address). The network
  takes 25 nodes for now; when it is full, the page says so.

On Windows (new in v0.5.0), one installer sets up the node as a service with a tray icon, and
the node's local page walks through the rest: see [Install on Windows](/node/windows/).

## Tested systems

| GPU | System | Driver | Install | Result |
|---|---|---|---|---|
| RTX 4090 | Ubuntu 22.04.5 (desktop) | 530.30.02 | llama.cpp built by hand | Served as a node 2026-10-09 to 2026-10-13; now the maintainer's development machine |
| RTX 3090 | Ubuntu 22.04 (rented VM) | 535 | llama.cpp built by hand | Install and one chat through the gateway, 2026-10-10 (about 42 tokens/s) |
| RTX 3090 | Ubuntu 22.04 (rented VM) | 535.183.01 | prebuilt bundle | Install, download resume, connect, 1 × 128k in 18,302 MiB, about 42 tokens/s (2026-10-11). Needed a build fix: kernels must not be compressed for drivers before 570 |
| RTX 3090 | Ubuntu 22.04.5 (rented VM) | 575.51.03 | prebuilt bundle | 2026-10-18: the installer's GPU rule did not match this driver, so `llama-server` ran on the CPU and the install stopped. With the rule the installer now adds, set by hand: install, connect, requests and a cancel through the gateway, local UI, uninstall; about 41 tokens/s. The fixed installer itself has not run on this driver yet |
| RTX 4090, RTX 5090 | rented | 595 | the bundle's `llama-server` in containers, not through the installer | Runs; used for the model-check measurement (2026-10-11) |
| RTX 4090 | Windows (desktop) | – | a pre-release build of the Windows installer | 2026-11-13: served on the network, 151 requests, none failed, about 40-46 tokens/s |
| RTX 5070 Ti | rented | – | the 16 GB tier's `llama-server` settings, not through the installer | 2026-10-31: `UD-Q3_K_XL` at 80k ran with 1.5 GB held by other programs, about 30 tokens/s after a 32k prompt. **16 GB tier: not yet run through the installer** |

Other NVIDIA cards with 16 GB or more and other distributions may work; the installer checks what it needs and
says what is missing. Tell us what worked for you: [contact@aisathome.org](mailto:contact@aisathome.org).

## How the node runs

- **No open ports.** The node only dials out; nothing listens on your network.
- **Sandboxed.** Both services run as the unprivileged user `aisathome`: no access to
  `/home`, a read-only system, no new privileges.
- **Fewer traces on disk.** No core dumps, no swap for the services, a bounded prompt cache
  in RAM, no prompt logging.
- **Anonymous requests.** Your node never learns who sent a request.
- **Stop any time.** `sudo systemctl stop aisathome-node aisathome-llama` frees the GPU; on
  Windows, **Stop engine** in the tray menu.

## How new nodes start

- A new node serves requests from day one. Requests with tools (agents such as opencode)
  reach it only from users who allow volunteer nodes for agents.
- Answers from volunteers' nodes pass simple safety checks. If an answer looks like it
  injects a dangerous command, it is stopped and the node is **held** until we have looked
  at it; the account page shows that.
- No single volunteer gets more than about a third of the conversations while others are free.
- Your node earns [credit](/node/credit/), except for requests you send yourself.

## What you are responsible for

- Your machine, its power bill and its other GPU work (BOINC, games). The node uses the GPU
  fully while it serves.
- **Not looking at what you serve:** don’t log, store, copy or inspect prompts or answers,
  and don’t tamper with them. This is a promise to users; see the
  [terms](/about/terms/).
- Keeping your node token secret, and telling us if it leaks.

Next: [install on Linux](/node/install/) or [on Windows](/node/windows/).
