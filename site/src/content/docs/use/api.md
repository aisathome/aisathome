---
title: API and opencode
description: Use AIs@home from your own tools through an OpenAI-compatible API.
---

The gateway speaks the OpenAI chat-completions API at **`https://api.aisathome.org/v1`**.
Any client that lets you set a base URL and a key works: [opencode](https://opencode.ai),
the OpenAI SDKs, `curl`.

## Get a key

Sign in with GitHub or Google on **[your account page](/account)** and create a key there (up to three).
A key is shown once; copy it then. Keys are personal and share your account’s daily limit
with the chat. If a key leaks, revoke it on the same page.

## Models

| Name | What |
|---|---|
| `deep` | Qwen3.8 27B, general chat |
| `agent` | The same model, for coding agents and tool calls |

`GET /v1/models` lists what the network serves right now.

## Try it with curl

```bash
export AISATHOME_API_KEY=ak-...
curl https://api.aisathome.org/v1/chat/completions \
  -H "Authorization: Bearer $AISATHOME_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"deep","messages":[{"role":"user","content":"What is BOINC?"}]}'
```

## opencode

Add a provider to `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "aisathome": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "AIs@home",
      "options": {
        "baseURL": "https://api.aisathome.org/v1",
        "apiKey": "{env:AISATHOME_API_KEY}"
      },
      "models": {
        "agent": {
          "name": "AIs@home agent (Qwen3.8 27B)",
          "limit": { "context": 32768, "output": 8192 }
        }
      }
    }
  }
}
```

Set `context` to the `context_length` that `/v1/models` reports for `agent` (the context every
node serving it can take, for example 32768):

```bash
curl -s https://api.aisathome.org/v1/models -H "Authorization: Bearer $AISATHOME_API_KEY"
```

Then `export AISATHOME_API_KEY=ak-...` and run `opencode -m aisathome/agent`. Tool calls
(reading and editing files, running commands) work.

## Good to know

- **Secrets: a safety net, not a guarantee.** A simple scan tries to replace well-known key
  formats with a placeholder like `sk-proj-AISX…XXXX` before a node sees them, and does
  **not put them back** into the answer or into tool calls, so a file the agent writes may
  contain the placeholder instead of your real key. Unusual formats get through: treat the
  service as untrusted and keep secrets out of what the agent can read.
- **Who serves you.** Chats and plain API requests may be answered by any volunteer's node;
  anything that looks like personal data goes only to the maintainer's nodes. Requests with
  tools (agents) go to volunteers' nodes only if you ticked **Let volunteers run my agents**
  when creating the key; without it, only to your own node or the maintainer's, which are
  often offline.
- **Run agents carefully, with any provider.** A model can propose a harmful command, and a
  malicious node could alter one; volunteers' answers pass simple checks but are not
  verified yet. In opencode, keep approval of commands on, and **run it isolated**: in a
  dedicated Docker container or virtual machine with only the project mounted, no SSH keys,
  cloud credentials or browser profile, and nothing you could not afford to lose.
- **Context and answer length.** A request must fit the context of a node that serves it:
  the prompt (everything sent, including tool definitions and earlier turns) plus the room
  for the answer. `/v1/models` reports `context_length`, the context every node serving the
  model can take right now. A request larger than any node can take fails at once with
  `400 context_length_exceeded`, naming the largest available. Answers are capped at
  32,768 tokens; without `max_tokens`, at 16,384.
- Limits: a daily token budget, requests per minute and concurrent requests per key. Over
  the limit you get `429` with a `Retry-After` header.
