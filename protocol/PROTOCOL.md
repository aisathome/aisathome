# AIs@home gateway ↔ node protocol, version 2

SPDX-License-Identifier: Apache-2.0

A **node** runs next to a local OpenAI-compatible inference engine (llama-server) and
opens **one outbound WebSocket** to a **gateway** (an AIs@home project server). Nodes never
accept inbound connections, so they work behind NAT and home routers. A node may connect
to several gateways (one connection each, chosen by the volunteer). Gateways never share
users, keys or billing.

## Connection

- `GET wss://<gateway>/v1/node/connect`
- Header: `Authorization: Bearer <node token>`: a token from the gateway's account page
  (`nt-<node id>.<secret>`; `node_id` in hello must be that id) or one issued by the gateway
  operator.
- Every frame is one UTF-8 JSON object with a `type` field. Unknown fields must be ignored.

## Protocol versions

The protocol version is a small integer; it goes up by one when the messages change in a way
the other side must know about. The node says which version it speaks in `hello`
(`protocol_version`); a `hello` without it comes from a node released before versions existed
and speaks **version 1**. The gateway accepts a range of versions (config `node_protocol_min`,
`node_protocol_max`; by default its own version and the one before it, so the current and the
previous node release both work) and answers with `welcome`, which names the version it speaks
with this node and the range. A version outside the range is refused: instead of `welcome` the
gateway closes the connection with code **4001** and a reason for the volunteer (at most 123
bytes), e.g. `node protocol version 1 not supported (gateway supports 2-3): update the node
agent`. The reference node logs the reason, shows it in its local UI and `aisathome-node
status`, and tries again only after 10 minutes. Old versions are retired on a published
schedule, by raising `node_protocol_min`.

The `protocol` field (`"0.1"`) is frozen: gateways before versions refuse a node whose
`protocol` does not start with `0.`, so nodes keep sending it; gateways keep sending it in
`welcome`, and compare versions only by `protocol_version`.

| version | nodes | what changed |
|---|---|---|
| 1 | v0.1.0 to v0.3.0 (no `protocol_version`) | the messages below, without the version fields |
| 2 | from the release after v0.3.0 | `hello.protocol_version`; `welcome.protocol_version`, `protocol_min`, `protocol_max`; refusal with close code 4001 |

## Messages

| type | direction | fields | meaning |
|---|---|---|---|
| `hello` | node → gw | `node_id`, `protocol`, `protocol_version`, `agent`, `models[]` | first frame. `models[]` = `{name, roles[], slots, ctx}`; `protocol_version` absent = 1 |
| `welcome` | gw → node | `gateway_id`, `protocol`, `protocol_version`, `protocol_min`, `protocol_max` | accepted, speaking `protocol_version` (the node's). An unsupported version is closed with code 4001 instead (see above) |
| `heartbeat` | node → gw | `active`, `paused` | every ~10 s, and at once when `paused` changes. Gateways drop nodes silent for more than 45 s. `paused` (optional, default `false`): send no new requests; requests in flight finish |
| `request` | gw → node | `req_id`, `body` | an OpenAI `/v1/chat/completions` body; `model` is a model name from `hello`, and `stream` is always `true` |
| `cancel` | gw → node | `req_id` | the gateway gave up on the request (client gone, stall, too much output, guard); stop work, then end the request with `error` (or `done` if it had finished) once the engine has let go of it |
| `chunk` | node → gw | `req_id`, `data` | one OpenAI `chat.completion.chunk`, passed through unchanged |
| `done` | node → gw | `req_id`, `usage` | finished. `usage` = `{prompt_tokens, completion_tokens, cached_tokens, duration_ms}` |
| `error` | node → gw | `req_id`, `error` | failed. Before any `chunk`, the gateway may retry on another node |

## Gateway behaviour (reference implementation)

- **Routing:** users may name a node model or a **role** (`fast`, `deep`, `agent`). The
  gateway picks a node with a free slot, round-robin, and retries once on another node if
  the first fails before streaming any output. Among the nodes a request may use, a free node
  that recently served a long prefix of the same conversation for the same requester comes
  first (its engine has that prefix cached); a busy one is not waited for.
- **Slots:** a node's slot is busy from `request` until the node's `done` or `error`. After a
  `cancel` it stays busy until that `error` (or `done`) arrives, because the engine may still
  be working on the request (llama-server notices a closed connection within about a second
  and stops at the end of its current batch); at most 120 s
  (`cancel_ack_timeout_seconds`), after which the gateway frees it anyway and logs a warning.
  The reference node waits for its engine's `/slots` to show no more busy slots than it still
  has requests open, at most 90 s; with an engine without `/slots` it ends the request as
  soon as its HTTP request to the engine has ended. Nodes released before this rule (v0.1.0)
  send the `error` at once; they stay compatible.
- **Pause:** a node whose last heartbeat said `paused: true` gets no new requests until a
  heartbeat says otherwise. A paused node answers any request that still reaches it (sent
  before the heartbeat arrived, or by a gateway that predates `paused`) with an `error`
  `node paused` before any `chunk`, so the gateway retries it on another node.
- **Metering:** one usage record per completed request (user, node, operator, model,
  tokens, duration), with tokens as reported by the node. Verification of reported usage
  comes later.
- Users get OpenAI-compatible streaming (SSE) or non-streaming responses.

## Not yet (planned)

- Sampled verification of node output.
- Honeytokens.
- Reputation.
- Signed node identity (gateway key pinning).
- Batch jobs.
- BOINC-format stats export.
- Per-gateway resource shares on nodes.
