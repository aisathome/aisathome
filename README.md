# AIs@home

**Experimental.** Volunteer-run LLM inference in the spirit of BOINC: people contribute
GPUs, and anyone can run a project server.

| Directory | What | Licence |
|---|---|---|
| `node/` | `aisathome-node`: runs next to a local llama-server on a volunteer's machine and connects **outward** to one or more gateways. Linux install in `node/packaging/linux/` | Apache-2.0 |
| `gateway/` | `aisathome-gateway`: a project server with an OpenAI-compatible API, routing with queueing and failover, metering | AGPL-3.0-or-later |
| `protocol/` | The gateway ↔ node protocol (`PROTOCOL.md`) and its Go types, importable by other implementations | Apache-2.0 |
| `e2e/` | End-to-end tests (user → gateway → node → mock engine) | Apache-2.0 |

## Status

Not production-ready. **Hardened, not confidential:** whoever runs a node can see the
prompts it serves. The gateway does not store prompts.

## Quick start (local)

```bash
go build ./node/cmd/aisathome-node ./gateway/cmd/aisathome-gateway
./aisathome-gateway -config gateway.json   # see examples/
./aisathome-node -config node.json         # next to llama-server on :8000
curl -H "Authorization: Bearer <api key>" http://localhost:8080/v1/chat/completions \
  -d '{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## Licences

Each directory carries its own `LICENSE`, and each source file an SPDX header. Full texts
are also in `LICENSES/`.
