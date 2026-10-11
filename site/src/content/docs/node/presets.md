---
title: Presets
description: Pick slots, context length and KV cache type for your GPU.
---

`--preset <name>` (or `PRESET=<name>`) picks the slots, the context per slot and the KV
cache type. Every slot has its own context (`--kv-unified-per-slot`), so one long request
cannot starve another.

| Preset | Slots × context | KV cache | Measured on an RTX 4090 with Qwen3.8 27B `UD-Q4_K_M` |
|---|---|---|---|
| `balanced` (default) | 2 × 32k | q8_0 | 1 × 64k q8: 17,858 MiB VRAM; mean KLD against f16 0.0020; no needle loss up to 60k |
| `low-vram` | 2 × 32k | q4_0 | 1 × 64k q4: 16,834 MiB (1.0 GB less than q8); no needle loss up to 60k; mean KLD 0.0044 (2.16× q8) |
| `long-context` | 1 × 128k | q4_0 | 18,306 MiB; hard needles at 120k 118/120 (q8: 119/120 at 20,354 MiB); first 120k prompt 82 s; decode 46.6 tok/s short, 32.5 at 120k |

Source: `bench/kvquant/RESULTS.md` in the research repo (llama.cpp v0.5.0, 2026-10-09).

Tested installs: an RTX 4090 (the owner's node) and a fresh RTX 3090 VM (Ubuntu 22.04,
driver 535, 2026-10-10): `balanced` used 17,972 MiB and decoded about 42 tokens/s for a
single 400-token answer measured through the gateway (one run).
Caveats:
- The 2 × 32k presets were not measured as such: the numbers are for one 64k slot, which
  has the same total context.
- The 128k rows were measured on a headless card. A desktop on the same GPU needs extra
  VRAM.
- q8 is the default because q4's mean KLD is 2.16× q8's, just over the 2× rule set before
  the benchmark. q4 showed no task loss, including at 120k.

Explicit variables override the preset: `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE` (`q8_0`, `q4_0`
or `f16`), `REASONING_BUDGET` (`-1` = unlimited), `CACHE_RAM_MIB` and `MODEL_ALIAS`, e.g.
`KV_TYPE=q8_0 sudo -E ./install.sh --preset long-context ...`.

