---
title: Model settings
description: "The two settings AIs@home nodes use: 80k context on 16 GB cards, 192k on 24 GB and more."
---

Every node runs the same model, Qwen3.8 27B `UD-IQ4_XS` (14.3 GB), one request at a time.
The context depends only on the size of the card, so there are two settings (tiers). The
installer picks the tier from your card; there is nothing to choose.

| Tier | Cards | Context | KV cache | What the card needs |
|---|---|---|---|---|
| **Standard** | 16 GB (RTX 4080, 5070 Ti, 5080 class) | 80k (81,920 tokens) | `q4_0` | 15,200 MiB free: **no screen on the card** (or a very light desktop, under about 650 MB) |
| **Big** | 24 GB and more (RTX 3090, 4090, 5090 class) | 192k (196,608 tokens) | `q4_0` | 18,500 MiB free |

Measured by the project with llama.cpp v0.5.0 (the benchmark notes are not public yet):

| | Standard (RTX 5070 Ti, 2026-10-22) | Big (RTX 3090, 2026-10-22; memory: RTX 4090, 2026-10-23) |
|---|---|---|
| GPU memory, after load / at the peak of a long prompt | 15,070 / 15,098 MiB (RTX 4080 SUPER: 15,110) | 192k: 17,844-18,060 / up to 18,246 MiB (190k prompt) |
| Hard needles (40 similar keys, 8 asked) | 24/24 at 75k | 119/120 at 120k, 77/80 at ~195k |
| Decode, tokens/s | 47 short, 29 at 32k, 17 at 78k (RTX 4080 SUPER: 36 at 32k, 30 at 78k) | 3090: 44 short, 22 at 125k, 17 at 195k; 4090: 49 at 32k, 32 at 190k |
| First long prompt | 67 s for 77k | 5 min for 195k |

What this means:

- **The tier must fit; it is never shrunk.** The installer checks the card's free memory
  before it starts the model. If a screen or programs use too much of it, it stops and says
  what to do: on a 16 GB card, connect the monitor to the motherboard's graphics (integrated
  GPU) or run without a desktop. A desktop with a browser can use 1-2 GB, and then 80k does
  not fit a 16 GB card.
- **A speed test at install.** After the model has loaded, the installer sends a 32,000-token
  prompt and measures how fast the card writes after it. Below **20 tokens/s** it stops the
  node: users would wait too long on long prompts. Measured at 32k: RTX 4080 36, RTX 3090
  35, RTX 5070 Ti 29, RTX 5060 Ti 14 tokens/s. Stop BOINC GPU tasks or games during the
  install, or the test measures them too.
- **One request at a time per GPU.** A long prompt keeps the GPU busy before the first word
  (about 5 minutes at 192k on an RTX 3090); short chats are quick. A request longer than 80k
  goes only to Big nodes.
- **One model file for the whole network.** `UD-IQ4_XS` replaced `UD-Q4_K_M` in v0.3.0: it
  is 2.2 GB smaller, which lets 16 GB cards join, and showed no measurable loss on our tests
  (coding, maths, tool calls, long-context needles, agent tasks). The model check cannot tell
  the two files apart reliably, so the network never mixes them: the installer accepts only
  the pinned file.
- **`q4_0` KV cache:** its mean divergence from full precision is about twice that of `q8_0`
  (KLD 0.0044 vs 0.0020), with no task loss measured, including at 120k. `q8_0` does not fit
  80k in 16 GB.

Where it is set: the tiers and the speed test in
[`install.sh`](https://github.com/aisathome/aisathome/blob/main/node/packaging/linux/install.sh)
(`tier_ctx`, `tier_min_free`, `speed_check`).

For testing only, `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE`, `REASONING_BUDGET` (`-1` =
unlimited), `CACHE_RAM_MIB`, `MODEL_ALIAS` and `MIN_DECODE_TPS` override the setting, e.g.
`CTX_PER_SLOT=65536 sudo -E ./install.sh` (the free-memory check is then skipped). A node with
a smaller context gets only requests that fit it.
