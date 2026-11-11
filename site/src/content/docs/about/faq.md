---
title: FAQ
description: Common questions about AIs@home.
---

## Is it free?

Yes. Each account has a daily limit so that the volunteers’ GPUs are shared fairly.

## Is it private?

No. It is **hardened, not confidential**: the volunteer whose GPU answers you can
technically read the conversation. We limit what they learn (no identity, secrets hidden,
personal data only to the maintainer's own nodes), but we can’t make it confidential. See
[Privacy](/about/privacy/).

## Which model runs?

Qwen3.8 27B, an open-weight model, in two files: quantised to about 4 bits (UD-IQ4_XS,
14.3 GB) on 24 GB GPUs, with 192k context, and to about 3 bits (UD-Q3_K_XL, 13.1 GB) on
16 GB GPUs, with 80k, which leaves room for a light desktop on the card. The 3-bit file scores
a little lower (HumanEval 92.7% against 96.3%). They answer under their own names
(`qwen3.8-27b`, `qwen3.8-27b-q3`), every answer says which one wrote it, and a conversation
stays on the one it started on. [Model settings](/node/presets/).

## Do volunteers get paid?

No. There is no token and no payment; running a node is voluntary.

## Can I run a node on Windows or a smaller GPU?

Windows: not yet, the node package is Linux only. Smaller GPUs: a 16 GB card can join (for
example an RTX 4080 or 5070 Ti), with at most a light desktop on it (about 1.5 GB of its memory);
its settings were measured on an RTX 5070 Ti, but nobody has run the 16 GB tier through the
installer yet. Cards under 16 GB, and 16 GB cards that are too slow (the installer tests the
speed), cannot join.
[What a node needs](/node/).

## Can I use it with BOINC at the same time?

Pause one while the other runs: the model uses the GPU fully while it serves. BOINC can do
the pausing itself: see [Running BOINC on the same GPU](/node/operate/#running-boinc-on-the-same-gpu).
Running both side by side automatically is planned.

## Why “AIs@home”?

After SETI@home and Einstein@Home: big computations shared across home computers.
