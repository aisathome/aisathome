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

- An NVIDIA GPU with **24 GB** of memory: RTX 3090, 4090 or 5090 class.
- Linux with systemd and the NVIDIA driver (`nvidia-smi` works).
- About 20 GB of disk for the model. With the prebuilt bundle, the install is mostly the
  17 GB download; building llama.cpp yourself takes about an hour.
- A **node token**: sign in on [your account page](/account) and choose **Add a node**
  (up to 3 per account). The network takes 10 nodes for now; when it is full, the page
  says so.

Windows is not supported yet.

## Tested systems

| GPU | System | Driver | Install | Result |
|---|---|---|---|---|
| RTX 4090 | Ubuntu 22.04.5 (desktop) | 530.30.02 | llama.cpp built by hand | The maintainer's node, serving since 2026-10-09 |
| RTX 3090 | Ubuntu 22.04 (rented VM) | 535 | llama.cpp built by hand | Install and one chat through the gateway, 2026-10-10 (about 42 tokens/s) |
| RTX 3090, 4090, 5090 | rented | 535 and newer | prebuilt bundle | Being tested now, before the first release |

Other 24 GB NVIDIA cards and distributions may work; the installer checks what it needs and
says what is missing. Tell us what worked for you: [contact@aisathome.org](mailto:contact@aisathome.org).

## How the node runs

- **No open ports.** The node only dials out; nothing listens on your network.
- **Sandboxed.** Both services run as the unprivileged user `aisathome`: no access to
  `/home`, a read-only system, no new privileges.
- **Fewer traces on disk.** No core dumps, no swap for the services, a bounded prompt cache
  in RAM, no prompt logging.
- **Anonymous requests.** Your node never learns who sent a request.
- **Stop any time.** `sudo systemctl stop aisathome-node aisathome-llama` frees the GPU.

## How new nodes start

- A new node serves **chats** from day one. Requests with tools (agents such as opencode)
  reach it only after a **probation of 7 days**, and only from users who allow volunteer
  nodes for agents.
- Answers from volunteers' nodes pass simple safety checks. If an answer looks like it
  injects a dangerous command, it is stopped and the node is **held** until we have looked
  at it; the account page shows that.
- No single volunteer gets more than about a third of the requests while others are free.
- Your node earns [credit](/node/credit/), except for requests you send yourself.

## What you are responsible for

- Your machine, its power bill and its other GPU work (BOINC, games). The node uses the GPU
  fully while it serves.
- **Not looking at what you serve:** don’t log, store, copy or inspect prompts or answers,
  and don’t tamper with them. This is a promise to users; see the
  [terms](/about/terms/).
- Keeping your node token secret, and telling us if it leaks.

Next: [install on Linux](/node/install/).
