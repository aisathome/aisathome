---
title: Model settings
description: "The one setting every AIs@home node uses: 1 request at a time, 128k context."
---

One setting for the whole network: **1 request at a time with a 128k context**, KV cache
`q4_0`. Every node takes the same requests, and the model check compares against one
reference. The installer uses it without asking; there is nothing to choose.

| Setting | Measured on an RTX 4090 with Qwen3.8 27B `UD-Q4_K_M` |
|---|---|
| 1 × 128k context, KV `q4_0` | 18,306 MiB VRAM; hard needles at 120k: 118/120 (q8: 119/120 at 20,354 MiB); first 120k prompt 82 s; decode 46.6 tokens/s short, 32.5 at 120k |

Source: `bench/kvquant/RESULTS.md` in the research repo (llama.cpp v0.5.0, 2026-10-09).

What this means:
- **One request at a time per GPU.** A long prompt keeps the GPU busy before the first word
  (about 80 seconds at 120k); short chats are quick. Daily token limits keep very long
  requests rare.
- **`q4_0` KV cache:** its mean divergence from full precision is about twice that of `q8_0`
  (KLD 0.0044 vs 0.0020), with no task loss measured, including at 120k. It is what lets
  128k fit in 24 GB.
- **A desktop on the same GPU** needs extra memory; the numbers above are for a card
  without a display. If the model does not fit, see
  [Troubleshooting](/node/operate/#troubleshooting).

For testing only, `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE`, `REASONING_BUDGET` (`-1` =
unlimited), `CACHE_RAM_MIB` and `MODEL_ALIAS` override the setting, e.g.
`CTX_PER_SLOT=65536 sudo -E ./install.sh`. A node with a smaller context gets only requests
that fit it.
