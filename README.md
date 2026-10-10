# AIs@home

**Experimental.** Volunteer-run LLM inference in the spirit of BOINC: people contribute
GPUs, and anyone can run a project server.

| Directory | What |
|---|---|
| `node/` | `aisathome-node`: runs next to a local llama-server on a volunteer's machine and connects **outward** to one or more gateways. Linux install in `node/packaging/linux/` |
| `gateway/` | `aisathome-gateway`: a project server with an OpenAI-compatible API, routing with queueing and failover, metering |
| `protocol/` | The gateway ↔ node protocol (`PROTOCOL.md`) and its Go types, importable by other implementations |
| `e2e/` | End-to-end tests (user → gateway → node → mock engine) |

## Status

Not production-ready. **Hardened, not confidential:** whoever runs a node can see the
prompts it serves. The gateway does not store prompts.

## Quick start (local)

```bash
go build -o aisathome-node ./node/cmd/aisathome-node
go build -o aisathome-gateway ./gateway/cmd/aisathome-gateway
./aisathome-gateway -config gateway.json   # see examples/
./aisathome-node -config node.json         # next to llama-server on :8000
curl -H "Authorization: Bearer <api key>" http://localhost:8080/v1/chat/completions \
  -d '{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`: the CI checks (`ci.yml`: gofmt,
`go vet`, `go test -race`, installer tests), then static binaries of `aisathome-node`
(linux amd64/arm64, windows amd64) and `aisathome-gateway` (linux amd64/arm64) with a
`SHA256SUMS` file, attached to a GitHub Release. The same build runs locally with
`./scripts/build-release.sh v0.1.0 dist`; `aisathome-node -version` prints the version.
The Linux node installer can use them (`--node-release latest`).

## Running a gateway

Config fields beyond the example (`examples/gateway.json`):

| Field | What |
|---|---|
| `state_dir` | Directory for state that survives restarts (daily token budgets, saved every 5 s and on shutdown). Without it a restart gives every key a fresh day. A corrupt state file is logged and ignored. |
| `keys_file` | Per-user API keys, managed with `aisathome-gateway keys` (use absolute paths: relative ones resolve from the working directory). The gateway rereads it every 2 s, so no restart or reload is needed. |

Keys in `api_keys` and `keys` (in the config, in clear) still work and reload on SIGHUP.
The keys file stores only a SHA-256 hash and a short prefix of each key:

```bash
aisathome-gateway keys add bob -config gateway.json     # prints the key once
aisathome-gateway keys add bob -tokens-per-day 500000 -rpm 10 -min-trust any
aisathome-gateway keys list -config gateway.json        # prefixes, users, limits
aisathome-gateway keys revoke ak-AbCd1234 -config gateway.json
```

New keys default to 2,000,000 weighted tokens per UTC day, 30 requests per minute, 4 in
flight, and trusted nodes only (`-min-trust any` to allow every node). Secrets in prompts
are always redacted for them; `restore_secrets` stays off unless you edit the file. Run
the command as the gateway's user (in a container: `podman exec <name> aisathome-gateway
keys ...`) and keep `keys_file` on a writable volume. Run one `keys` command at a time.

Users point any OpenAI-compatible client (opencode, Open WebUI, `curl`) at
`https://<gateway>/v1` with the key as the bearer token, and use a model or role name from
`/v1/models`.

## Licence

Apache-2.0 (see `LICENSE`). Each source file carries an SPDX header. The name "AIs@home" is
not licensed with the code.
