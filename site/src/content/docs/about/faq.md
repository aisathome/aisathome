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

Qwen3.8 27B, an open-weight model, quantised to about 4 bits (UD-IQ4_XS, 14.3 GB) so that it fits a
16 GB GPU with 80k context, or a 24 GB GPU with 192k.

## Do volunteers get paid?

No. There is no token and no payment; running a node is voluntary.

## Can I run a node on Windows or a smaller GPU?

Windows: not yet, the node package is Linux only. Smaller GPUs: a 16 GB card works if no screen is
connected to it (for example an RTX 4080 or 5070 Ti); cards under 16 GB, and 16 GB cards that are
too slow (the installer tests the speed), cannot join. [What a node needs](/node/).

## Can I use it with BOINC at the same time?

Pause one while the other runs: the model uses the GPU fully while it serves. BOINC can do
the pausing itself: see [Running BOINC on the same GPU](/node/operate/#running-boinc-on-the-same-gpu).
Running both side by side automatically is planned.

## Why “AIs@home”?

After SETI@home and Einstein@Home: big computations shared across home computers.
