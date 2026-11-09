---
title: Model settings
description: "The two settings AIs@home nodes use: UD-Q3_K_XL with 80k context on 16 GB cards, UD-IQ4_XS with 192k on 24 GB and more."
---

Every node runs Qwen3.8 27B, one request at a time. The size of the card sets everything
else (the tier): the model file, the name it answers under, and the context. The installer
picks the tier from your card; there is nothing to choose.

| Tier | Cards | Model file | Model name in the API | Context | KV cache | What the card needs |
|---|---|---|---|---|---|---|
| **Standard** | 16 GB (RTX 4080, 5070 Ti, 5080 class) | `UD-Q3_K_XL` (13.1 GB) | `qwen3.8-27b-q3` | 80k (81,920 tokens) | `q4_0` | 14,300 MiB free: room for a light desktop, about 1.5 GB in all for the screen and other programs |
| **Big** | 24 GB and more (RTX 3090, 4090, 5090 class) | `UD-IQ4_XS` (14.3 GB) | `qwen3.8-27b` | 192k (196,608 tokens) | `q4_0` | 18,500 MiB free |

Measured by the project with llama.cpp v0.5.0 (the benchmark notes are not public yet):

| | Standard (`UD-Q3_K_XL`; RTX 5070 Ti, 2026-10-31) | Big (`UD-IQ4_XS`; RTX 3090, 2026-10-22; memory: RTX 4090, 2026-10-23) |
|---|---|---|
| GPU memory at the peak of a long prompt | about 14,030 MiB at 80k (measured: 13,659 at a 64k context, 14,211 at 88k, each with a prompt filling 95% of it) | 192k: up to 18,246 MiB (190k prompt) |
| With a desktop on the card | ran with 1.5 GB held by other programs (at 88k, more than 80k needs); failed at start with 2.0 GB | |
| Hard needles (40 similar keys, 8 asked) | 77/80 at 32k, 76/80 at 60k (RTX 4080; `UD-IQ4_XS` on the same prompts: 80/80 at 32k) | 119/120 at 120k, 77/80 at ~195k |
| Decode, tokens/s | 49 short, 30 at 32k, 20 at 64k, 17 at 80k | 3090: 44 short, 22 at 125k, 17 at 195k; 4090: 49 at 32k, 32 at 190k |
| First long prompt | about 70 s for 80k | 5 min for 195k |

What this means:

- **The tier must fit; it is never shrunk.** The installer checks the card's free memory
  before it starts the model. If a screen or programs use too much of it, it stops and says
  what to do: close programs that use the GPU, or connect the monitor to the motherboard's
  graphics (integrated GPU). On a 16 GB card a desktop with a browser fits if it stays under
  about 1.5 GB of GPU memory; a heavy desktop of 2 GB does not.
- **A speed test at install.** After the model has loaded, the installer sends a 32,000-token
  prompt and measures how fast the card writes after it. Below **20 tokens/s** it stops the
  node: users would wait too long on long prompts. Measured at 32k: RTX 4080 36, RTX 3090
  35, RTX 5070 Ti 29 (30 with `UD-Q3_K_XL`), RTX 5060 Ti 14 tokens/s. Stop BOINC GPU tasks or
  games during the install, or the test measures them too.
- **One request at a time per GPU.** A long prompt keeps the GPU busy before the first word
  (about 5 minutes at 192k on an RTX 3090); short chats are quick. A request longer than 80k
  goes only to Big nodes.
- **Two model files, one per tier.** `UD-IQ4_XS` replaced `UD-Q4_K_M` in v0.3.0: it is
  2.2 GB smaller and showed no measurable loss on our tests (coding, maths, tool calls,
  long-context needles, agent tasks). `UD-Q3_K_XL` is smaller again, so that a 16 GB card
  keeps about 1 GB more for a desktop (`UD-IQ4_XS` at 80k left about 640 MiB), and it does
  lose a little: HumanEval 92.7% against 96.3% for `UD-IQ4_XS` (the same 164 problems), hard
  needles 77/80 against 80/80 at 32k, and a larger divergence from the 8-bit model (mean KLD
  0.0967 against 0.0585). Maths (GSM8K) and tool calls showed no difference at our sample
  sizes. It buys memory, not quality or speed.
- **A conversation stays on one model.** Both files serve the roles `deep` and `agent`, under
  their own names. Once a chat or an agent session has an answer from one of them, the
  gateway sends its later requests only to nodes running the same file; if none is online, it
  says so rather than switching models in the middle. Every answer names the model that
  answered in its `model` field. To choose a file, ask for its name instead of a role.
- **The installer accepts only the tier's pinned file.** The model check cannot tell close
  files apart reliably, so the network never mixes them under one name.
- **`q4_0` KV cache:** its mean divergence from full precision is about twice that of `q8_0`
  (KLD 0.0044 vs 0.0020), with no task loss measured, including at 120k. With `UD-IQ4_XS`,
  `q8_0` did not fit 80k in 16 GB.

Where it is set: the tiers and the speed test in
[`install.sh`](https://github.com/aisathome/aisathome/blob/main/node/packaging/linux/install.sh)
(`tier_model`, `tier_alias`, `tier_ctx`, `tier_min_free`, `speed_check`).

For testing only, `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE`, `REASONING_BUDGET` (`-1` =
unlimited), `CACHE_RAM_MIB`, `MODEL_ALIAS` and `MIN_DECODE_TPS` override the setting, e.g.
`CTX_PER_SLOT=65536 sudo -E ./install.sh` (the free-memory check is then skipped). A node with
a smaller context gets only requests that fit it.
