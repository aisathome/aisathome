---
title: API and opencode
description: Use AIs@home from your own tools through an OpenAI-compatible API.
---

The gateway speaks the OpenAI chat-completions API at **`https://api.aisathome.org/v1`**.
Any client that lets you set a base URL and a key works: [opencode](https://opencode.ai),
the OpenAI SDKs, `curl`.

## Get a key

Sign in with GitHub on **[your account page](/account)** and create a key there (up to three).
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

Then `export AISATHOME_API_KEY=ak-...` and run `opencode -m aisathome/agent`. Tool calls
(reading and editing files, running commands) work.

## Good to know

- **Secrets in your code are hidden from nodes.** Well-known key formats are replaced by a
  placeholder like `sk-proj-AISX…XXXX` before a node sees them, and they are **not put back**
  into the answer or into tool calls. A file the agent writes may therefore contain the
  placeholder instead of your real key.
- Requests run on trusted nodes only, unless your key says otherwise.
- Limits: a daily token budget, requests per minute and concurrent requests per key. Over
  the limit you get `429` with a `Retry-After` header.
