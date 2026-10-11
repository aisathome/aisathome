# AIs@home gateway ↔ node protocol, v0.1

SPDX-License-Identifier: Apache-2.0

A **node** runs next to a local OpenAI-compatible inference engine (llama-server) and
opens **one outbound WebSocket** to a **gateway** (an AIs@home project server). Nodes never
accept inbound connections, so they work behind NAT and home routers. A node may connect
to several gateways (one connection each, chosen by the volunteer). Gateways never share
users, keys or billing.

## Connection

- `GET wss://<gateway>/v1/node/connect`
- Header: `Authorization: Bearer <node token>`, a token issued by the gateway operator.
- Every frame is one UTF-8 JSON object with a `type` field. Unknown fields must be ignored.

## Messages

| type | direction | fields | meaning |
|---|---|---|---|
| `hello` | node → gw | `node_id`, `protocol`, `agent`, `models[]` | first frame. `models[]` = `{name, roles[], slots, ctx}` |
| `welcome` | gw → node | `gateway_id`, `protocol` | accepted. A different major protocol version closes the connection |
| `heartbeat` | node → gw | `active`, `paused` | every ~10 s, and at once when `paused` changes. Gateways drop nodes silent for more than 45 s. `paused` (optional, default `false`): send no new requests; requests in flight finish |
| `request` | gw → node | `req_id`, `body` | an OpenAI `/v1/chat/completions` body; `model` is a model name from `hello`, and `stream` is always `true` |
| `cancel` | gw → node | `req_id` | the client went away; stop work |
| `chunk` | node → gw | `req_id`, `data` | one OpenAI `chat.completion.chunk`, passed through unchanged |
| `done` | node → gw | `req_id`, `usage` | finished. `usage` = `{prompt_tokens, completion_tokens, cached_tokens, duration_ms}` |
| `error` | node → gw | `req_id`, `error` | failed. Before any `chunk`, the gateway may retry on another node |

## Gateway behaviour (reference implementation)

- **Routing:** users may name a node model or a **role** (`fast`, `deep`, `agent`). The
  gateway picks a node with a free slot, round-robin, and retries once on another node if
  the first fails before streaming any output.
- **Pause:** a node whose last heartbeat said `paused: true` gets no new requests until a
  heartbeat says otherwise. A paused node answers any request that still reaches it (sent
  before the heartbeat arrived, or by a gateway that predates `paused`) with an `error`
  `node paused` before any `chunk`, so the gateway retries it on another node.
- **Metering:** one usage record per completed request (user, node, operator, model,
  tokens, duration), with tokens as reported by the node. Verification of reported usage
  comes later.
- Users get OpenAI-compatible streaming (SSE) or non-streaming responses.

## Not in v0.1 (planned)

- Sampled verification of node output.
- Honeytokens.
- Reputation.
- Signed node identity (gateway key pinning).
- Batch jobs.
- BOINC-format stats export.
- Per-gateway resource shares on nodes.
