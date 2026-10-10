---
title: Terms of use
description: Terms for using AIs@home and for running a node.
banner:
  content: Draft under review. The final text may change.
---

AIs@home is a volunteer-run, non-commercial experiment: open-weight AI models (currently
Qwen3.8 27B) running on volunteers' GPUs instead of a big company's data centre. It is run
by a private person in Norway ("we", "the maintainer"). The software is open source
(Apache-2.0): https://github.com/aisathome/aisathome. There is no project token and nobody
is paid.

By using the chat (https://chat.aisathome.org), the API (https://api.aisathome.org) or
running a node, you accept these terms. If you don't, don't use the service.

## 1. It is an experiment

- The service is provided **as is**, with no warranty and no promised availability. It
  can be slow, go down, lose data or stop at any time, with or without notice.
- Model output can be wrong, invented, outdated or offensive. Check anything that
  matters before you rely on it. You are responsible for how you use the output.

## 2. It is not confidential

AIs@home is **hardened, not confidential**. Read the [privacy notice](/about/privacy/) before you type
anything you care about. In short:

- **The person running the node** that answers you can technically read the conversation
  their GPU processes. We cannot prevent that.
- Nodes never receive your identity (name, email, account id). Well-known secret formats
  (API keys, private keys) are hidden from nodes and, by default, not put back into the
  answer, so you may see a placeholder instead. Messages containing things like email
  addresses, card or account numbers, IBANs or national ID numbers only go to trusted
  nodes. **Detection is incomplete.**
- **Do not send passwords, secrets, or other people's personal data.** Do not send
  anything you would not want a stranger to read. If you need confidentiality, run a model
  on your own machine.

## 3. Accounts and API keys (users)

- You sign in with GitHub or Google. The number of accounts is limited; new accounts wait
  in line until there is room. We may close sign-up or refuse an account at any time.
- Your account and API key are personal. Keep the key secret and don't share it. Tell us
  if it leaks so we can revoke it.
- Each account has a daily usage limit. Don't try to get around it (several accounts,
  shared keys, scripts).

## 4. Acceptable use (everyone)

Do not:

- use the service for anything illegal, or to create or spread illegal content;
- harass, threaten or harm others, or generate content that sexualises minors;
- attack, overload, probe or disrupt the network, the gateway or anyone's node;
- try to get at other users' data, prompts or answers, or to identify other users;
- run automated mass use beyond your limits, or resell access;
- impersonate AIs@home or claim we endorse what you do with the output.

## 5. Node operators

Running a node is voluntary and unpaid. You need a node token from us, and you keep
control of your own machine and power bill. As an operator you agree to:

- **Not log, store, copy or inspect prompts or answers** beyond what running the model
  requires, and not share them with anyone;
- **not tamper with answers:** run the model and settings we specify, return the model's
  real output, and don't misrepresent your hardware or what your node does;
- not try to identify users or link requests to people;
- keep your node token secret and tell us if it leaks;
- follow the law where you are, including any rules on processing other people's data.

We may revoke a node token, stop routing to a node, or ban an operator at any time, for
example on suspicion of a breach. Running a node may affect your hardware, electricity
costs and other GPU work; that is your choice and your risk.

## 6. Suspension

We may suspend or delete any account, API key or node token at any time, with or without
notice, especially for a breach of these terms, abuse, or to protect the service or other
people.

## 7. Models and licences

The models are made by third parties and published under their own licences (Qwen3.8 27B:
Apache-2.0). Your use of the output must also
respect those licences. We don't claim ownership of your prompts or of the output.

## 8. Liability

To the extent the law allows, we are not liable for any loss or damage from using or not
being able to use the service, from model output, or from what a node operator does in
breach of these terms. Nothing here limits rights you have under mandatory law.

## 9. Changes

We may change these terms. We will post the new version here with its date. If you keep
using the service after that, the new terms apply.

## 10. Contact

contact@aisathome.org
