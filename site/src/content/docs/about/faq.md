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

Qwen3.8 27B, an open-weight model, quantised to 4 bits so that it fits a 24 GB GPU.

## Do volunteers get paid?

No. There is no token and no payment; running a node is voluntary.

## Can I run a node on Windows or a smaller GPU?

Not yet. The node package is Linux only, and the model needs a 24 GB GPU such as an RTX 3090
or 4090.

## Can I use it with BOINC at the same time?

Pause one while the other runs: the model uses the GPU fully while it serves. Running both
side by side automatically is planned.

## Why “AIs@home”?

After SETI@home and Einstein@Home: big computations shared across home computers.
