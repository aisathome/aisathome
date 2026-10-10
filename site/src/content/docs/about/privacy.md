---
title: Privacy
description: How AIs@home handles your messages.
---

AIs@home is an experiment: chat with an open AI model that runs on volunteers' GPUs instead
of a big company's data centre. Read this before you type anything you care about.

**Where your message goes.** Your message travels encrypted to our gateway (a server in
Stockholm), which passes it to a volunteer's computer (a "node"). The node runs the model
and sends the answer back the same way.

**Who can see it.**
- **The person running the node** that answers you can technically read the conversation
  their computer processes. We cannot prevent that: home GPUs have no way to hide what they
  compute. To limit it, chats only go to nodes run by people we know ("trusted nodes"), and
  nodes never learn who you are: no name, no email, no account id.
- **We, who run the gateway,** could see messages while they pass through. The gateway does
  not store them.

**What we store.**
- Your account: GitHub’s numeric user id or Google’s account identifier (“sub”), depending
  on which you sign in with; its state (active or waiting) and when it was created. No
  email, no name, no password: we ask Google only to confirm who you are (the “openid”
  scope). API keys are stored only as a hash.
- Chats: temporary by default. Chats you save are kept until you delete them.
- For each request: time, a pseudonymous user id, which node answered, token counts and
  duration. Never the content. Kept as long as your account exists.

**What we never do.** We don't sell data, don't show ads, don't train models on your
chats, and run no analytics or tracking.

**A safety net, not a guarantee.** We automatically hide well-known secret formats (API
keys, private keys) from nodes, and messages containing things like email addresses or
card and account numbers only go to trusted nodes. Detection is incomplete. **Do not paste passwords, secrets or other people's personal data.** You can see the code for this in the [repository](https://github.com/aisathome/aisathome).

**This service is not confidential.** It is hardened, not private. If you need
confidentiality, run a model on your own machine. AIs@home's software is open source, so
you can.

**Limits.** The number of accounts is limited while the network is small (new accounts wait
in line), and each account has a daily usage limit.
