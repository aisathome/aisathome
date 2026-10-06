# AIs@home

**Experimental.** Volunteer-run LLM inference in the spirit of BOINC: people contribute
GPUs, and anyone can run a project server.

- `aisathome-node`: runs next to a local llama-server on a volunteer's machine and connects
  **outward** to one or more gateways. Apache-2.0.
- `aisathome-gateway`: an AIs@home project server with an OpenAI-compatible API, routing with
  failover and metering. AGPL-3.0-or-later.
- `docs/protocol.md`: the gateway ↔ node protocol. Apache-2.0.

## Status

Milestone 1: one gateway and nodes relaying chat completions with metering. Not
production-ready. **Hosts may be able to see prompts; this network does not offer
confidentiality.**

## Quick start (local)

```bash
go build ./cmd/...
./aisathome-gateway -config gateway.json   # see examples/
./aisathome-node -config node.json         # next to llama-server on :8000
curl -H "Authorization: Bearer <api key>" http://localhost:8080/v1/chat/completions \
  -d '{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## Licences

Each source file carries an SPDX header. Gateway code (`internal/gateway`,
`cmd/aisathome-gateway`) is AGPL-3.0-or-later. Everything else is Apache-2.0. Full texts are
in `LICENSES/`.
