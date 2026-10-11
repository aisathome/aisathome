---
title: Chat
description: Use the AIs@home chat at chat.aisathome.org.
---

The chat runs [Open WebUI](https://openwebui.com) at
**[chat.aisathome.org](https://chat.aisathome.org)**.

1. **Sign in with GitHub or Google.** Only the account’s identifier is kept (GitHub’s
   numeric user id, or Google’s “sub”): no email, no name, no password. GitHub and Google are
   separate accounts, so use the same one each time.
2. **Wait, if there is a queue.** While the network is small the number of accounts is
   limited; new accounts get in in order as room appears, and your [account page](/account)
   shows your place.
3. **Chat.** The model is `deep` (Qwen3.8 27B on a volunteer’s GPU).

## Temporary by default

New chats are **temporary**: they are not saved, and they are gone when you close them. To
keep one, use **Save chat** or switch temporary mode off. Saved chats are stored by the chat
application in its database on our server (not on the volunteers' nodes) until you delete
them.

## Before you type

- The volunteer whose GPU answers you can technically read the conversation, and any
  volunteer can run a node. See
  [Privacy](/about/privacy/).
- A simple scan tries to replace well-known secret formats (API keys, private keys) with a
  placeholder before a node sees them, so the answer may show the placeholder instead of
  your key. It is a safety net, not a guarantee: treat the system as untrusted and don't
  paste secrets.
- Each account has a daily usage limit.
