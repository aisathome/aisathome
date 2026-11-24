---
title: Terms of use
description: "Terms for using the AIs@home chat and API and for running a node: an experiment, not confidential, acceptable use, what node operators agree to."
---

*Last changed: 2026-11-24.*

AIs@home is a volunteer-run, non-commercial experiment: open-weight AI models (currently
Qwen3.8 27B) running on volunteers' GPUs instead of a big company's data centre. It is run
by JFR Holding AS (org. no. 937 566 042), Norway ("we"); contact: contact@aisathome.org.
The software is open source (Apache-2.0): https://github.com/aisathome/aisathome. There is
no project token and nobody is paid.

By using the chat (https://chat.aisathome.org), the API (https://aisathome.org/use/api/) or
running a node, you accept these terms. You must be 18 or older. Starting a node without an account, by choosing
it in the installer or on the node's setup page, is running a node too. If you don't accept them, don't use the
service.

## 1. It is an experiment

- The service is provided **as is**, with no warranty and no promised availability. It
  can be slow, go down, lose data or stop at any time, with or without notice.
- Model output can be wrong, invented, outdated or offensive. Check anything that
  matters before you rely on it. You are responsible for how you use the output.

## 2. It is not confidential

**Hardened, not confidential.** Our software doesn't show or save your conversation on the
volunteer's machine, but the machine is theirs: someone who sets out to can read what their GPU
is answering. We make keeping it harder, less valuable, traceable and punishable. Read the
[privacy notice](/about/privacy/) before you type anything you care about. In short:

- **The person running the node** that answers you can technically read the conversation
  their GPU processes. We cannot prevent that.
- Nodes never receive your identity (name, email, account id). Well-known secret formats
  (API keys, private keys) are hidden from nodes and, by default, not put back into the
  answer, so you may see a placeholder instead. Messages containing things like email
  addresses, card or account numbers, IBANs or national ID numbers are refused unless your
  own node answers. **Detection is incomplete.**
- Nodes can be anywhere in the world, including outside Europe.
- **Don't put personal data (yours or other people's) into chats: answers are computed on
  volunteers' machines.** Don't send passwords or secrets either. Do not send anything you
  would not want a stranger to read. If you need confidentiality, run a model on your own
  machine.

## 3. Accounts and API keys (users)

- You sign in with GitHub or Google. The number of accounts is limited; new accounts wait
  in line until there is room. We may close sign-up or refuse an account at any time.
- Your account is personal. On the account page you can create up to 3 API keys and
  revoke them yourself. Keep keys secret and don't share them; if one leaks, revoke it.
- Each account has a daily usage limit, shared by the chat and its API keys. Don't try to
  get around it (several accounts, shared keys, scripts).
- A username you set is public. Don't pick one that is offensive, misleading or someone
  else's name or brand; we may clear it.

## 4. Acceptable use (everyone)

Do not:

- put personal data (yours or other people's) into chats or API requests: answers are
  computed on volunteers' machines;
- use the service for anything illegal, or to create or spread illegal content;
- harass, threaten or harm others, or generate content that sexualises minors;
- attack, overload, probe or disrupt the network, the gateway or anyone's node;
- try to get at other users' data, prompts or answers, or to identify other users;
- run automated mass use beyond your limits, or resell access;
- send unsolicited bulk messages (spam), or build tools for it;
- impersonate AIs@home or claim we endorse what you do with the output.

## 5. Node operators

Running a node is voluntary and unpaid. You add a node on your account page, by linking it with the code it shows,
or by starting it without an account. A node without an account does science work only, earns no credit, and is
deleted 90 days after it last connected unless you link it to an account; credit starts when you link it. It never
answers other people's chats. You keep control of your own machine and power bill. Credit is recognition, not money,
and we may correct or remove credit, for example after abuse. As an operator you agree to:

- **not tamper with answers:** run the model and settings we specify, return the model's
  real output, and don't misrepresent your hardware or what your node does;
- not try to identify users or link requests to people;
- keep your node token secret and tell us if it leaks;
- follow the law where you are, including any rules on processing other people's data.

We may revoke a node token, stop routing to a node, hold a node whose answers a safety
check flagged, or ban an operator at any time, for example on suspicion of a breach.
Running a node may affect your hardware, electricity
costs and other GPU work; that is your choice and your risk.

## 6. Requests your node answers

Users may not put personal data into their chats, and we send your node no account, name, email address or IP address.
Whatever a request contains, you agree to:

1. Use requests only to compute the answer the gateway asks for, with the node software as we provide it.
2. Not log, keep, copy, read or share prompts or answers, and not try to find out who sent them. Nothing from a request
   may outlive the request.
3. Keep your machine and node token secure, and let no one else get at your node's requests.
4. Not let anyone else run your node's work for you or pass requests on.
5. Tell us at contact@aisathome.org without delay, within 24 hours if you can, if you think someone has seen or kept
   requests from your node, or if your token has leaked.
6. Help us, where you can, when someone asks us about their data or when we deal with an incident.
7. Delete anything from requests still on your machine when you stop running a node or when we ask. Our software keeps
   nothing on disk.
8. Let us check your node (model checks, safety checks on answers, our records of what it served), and answer our
   questions about an incident.

Your node answers other people's chats only after you have accepted these terms; until then it does science work only.

## 7. If an operator breaks §6

Keeping, reading for your own use, or sharing other people's conversations, or deliberately altering answers, means a
permanent ban: all your nodes are cut off at once, all your node tokens revoked, your credit removed, and, if you
crunch for our BOINC project, your BOINC account closed. We publish a short note about every confirmed case. Where the
law requires it, we report the incident to the data protection authority and tell the people affected.

On suspicion we may hold a node or stop routing to it while we check. For a node without an account, we cut that node
off and may stop accepting new nodes without an account for a while.

## 8. Suspension

We may suspend or delete any account, API key or node token at any time, with or without
notice, especially for a breach of these terms, abuse, or to protect the service or other
people.

## 9. Models and licences

The models are made by third parties and published under their own licences (Qwen3.8 27B:
Apache-2.0). Your use of the output must also
respect those licences. We don't claim ownership of your prompts or of the output.

## 10. Liability

To the extent the law allows, we are not liable for any loss or damage from using or not
being able to use the service, from model output, or from what a node operator does in
breach of these terms. Nothing here limits rights you have under mandatory law.

## 11. Changes

We may change these terms. We will post the new version here with its date. If you keep
using the service after that, the new terms apply.

## 12. Contact

contact@aisathome.org
