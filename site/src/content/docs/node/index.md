---
title: Run a node
description: Give your GPU to AIs@home. What you need, what the node does, and what you are responsible for.
---

A node is a computer with a strong NVIDIA GPU that serves the model to AIs@home users. It
runs two services: `llama-server` (the model, on `127.0.0.1` only) and `aisathome-node`, a
small agent that connects **outward** to the gateway and passes requests through.

Running a node is **voluntary and unpaid**. There is no token and no payment.

## What you need

- An NVIDIA GPU with **24 GB** of memory. Tested: RTX 4090 and RTX 3090.
- Linux with systemd and the NVIDIA driver (`nvidia-smi` works).
- About 20 GB of disk for the model and an hour for the first install.
- A **node token**: email [contact@aisathome.org](mailto:contact@aisathome.org).
  Nodes are added by hand while the network is small.

Windows is not supported yet.

## What you can count on

- **No open ports.** The node only dials out; nothing listens on your network.
- **Sandboxed.** Both services run as the unprivileged user `aisathome`: no access to
  `/home`, a read-only system, no new privileges.
- **Fewer traces on disk.** No core dumps, no swap for the services, a bounded prompt cache
  in RAM, no prompt logging.
- **Anonymous requests.** Your node never learns who sent a request.
- **Stop any time.** `sudo systemctl stop aisathome-node aisathome-llama` frees the GPU.

## What you are responsible for

- Your machine, its power bill and its other GPU work (BOINC, games). The node uses the GPU
  fully while it serves.
- **Not looking at what you serve:** don’t log, store, copy or inspect prompts or answers,
  and don’t tamper with them. This is a promise to users; see the
  [terms](/about/terms/).
- Keeping your node token secret, and telling us if it leaks.

Next: [install on Linux](/node/install/).
