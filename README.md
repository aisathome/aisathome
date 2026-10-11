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

The release also has the prebuilt node bundle `aisathome-node-bundle-linux-amd64.tar.xz`:
the installer, `aisathome-node` and `llama-server` (llama.cpp v0.5.0, CUDA 12.8, RTX 30/40/50)
with its CUDA libraries, so a volunteer needs only the NVIDIA driver. `scripts/build-llama.sh`
builds `llama-server` in NVIDIA's CUDA image (an estimated 1 to 2 hours of CI on a tag); details in
`node/packaging/linux/README.md`. Running the release workflow by hand builds everything
without publishing.

## Running a gateway

Config fields beyond the example (`examples/gateway.json`):

| Field | What |
|---|---|
| `state_dir` | Directory for state that survives restarts (daily token budgets and the credit ledger `credit.json`, saved every 5 s and on shutdown). Without it a restart gives every key a fresh day and forgets credit. A corrupt budgets file is logged and ignored; a corrupt ledger is set aside as `credit.json.bad`. |
| `tokenizer_file`, `tokenizer_sha256` | The model's `tokenizer.json`, so the gateway counts tokens for credit itself (see [Credit](#credit)); the gateway refuses to start if the file does not match the SHA-256. |
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

### A chat front end (Open WebUI)

A key with `"forward_users": true` lets a trusted front end speak for its users: the gateway
takes the user from Open WebUI's `X-OpenWebUI-User-Email` / `X-OpenWebUI-User-Id` headers and
applies that user's limits (and, for signed-in accounts, the account's budget). Such a key is
accepted **only** on the private listener `frontend_addr` (e.g. `":8081"`), never on the
public one, so a leaked key can neither pose as other users nor be used as an ordinary key.
Point Open WebUI at `http://<gateway>:8081/v1` over a private network and never publish that
port. The front-end key's `min_trust` and `only_operator` also bind the accounts it carries.

With accounts, Open WebUI logs users in from headers set by the gateway's
`/account/auth` (`X-Auth-Email`, `X-Auth-Role`). That is safe only if the reverse proxy
removes any client-sent copies of these headers and Open WebUI is reachable only through
that proxy; the ops repo's Caddyfile does both, and its browser tests check it.

### Accounts (sign-in, self-service keys)

An `accounts` section turns on sign-in with GitHub, Google and/or email, up to `max_keys` API
keys per account and a waitlist beyond the active-account limit. Set `file`, `public_url`,
`session_secret` (32+ characters) and at least one way to sign in; a provider is on when
both its client id and secret are set:

| Provider | Fields | Redirect URI to register |
|---|---|---|
| GitHub (OAuth app, no scopes) | `github_client_id`, `github_client_secret` | `<public_url>/account/callback` |
| Google (OpenID Connect, scope `openid` only) | `google_client_id`, `google_client_secret` | `<public_url>/account/callback/google` |

Only the provider's user id is stored: `gh:<numeric id>` or `google:<sub>`. No email, name or
token. Google's ID token is checked against Google's published keys (RS256, issuer,
audience, expiry, nonce) and the flow uses state and PKCE. The same person signing in with
GitHub and with Google gets two separate accounts. Manage accounts with
`aisathome-gateway accounts list|limit|close|open|add|activate|waitlist|ban` (nodes: see
below). Run it as the
gateway's own user (in a container: `podman exec aisathome-gateway aisathome-gateway accounts
...`), otherwise the file may become unwritable for the gateway. `waitlist` holds an account
back until you `activate` it. The usage log names accounts by a pseudonym (an HMAC of the id),
never by their GitHub, Google or email id.

### Volunteer nodes (self-registered)

**Trust model.** "Trusted" means *operated by the maintainer* (node tokens in the gateway
config with `"trust": "trusted"`), not "secure": the maintainer's nodes are partly rented,
and whoever controls a machine can read what it serves. Every other node is an untrusted
volunteer. Nothing here is confidential; the measures below make keeping or misusing data
harder and more visible ("hardened, not confidential").

Signed-in active accounts add nodes on the account page ("Your nodes"): up to 3 node tokens
per account, shown once, stored as SHA-256 hashes in the accounts file, revocable there. A
token looks like `nt-<node id>.<secret>`; the gateway assigns the node id (`n-<8 hex>`), and
the node agent takes it from the token, so a node needs only its token. Config tokens keep
working; ids of the form `n-<8 hex>` are reserved for account nodes. In the usage log an
account node's operator is the account's pseudonym, and `self_served` marks requests an
account's own node answered.

Routing and protections:

| What | Rule |
|---|---|
| Personal data (email addresses, card numbers, IBANs, Norwegian ID numbers) | trusted nodes only (and the requester's own nodes) |
| Requests with `tools` (agents) | trusted nodes and the requester's own, unless the API key was created with "allow volunteer nodes for agents" (`agents` on the key; `volunteer_agents` for config keys) **and** the node's account is past probation. The chat (Open WebUI) never carries consent |
| Probation | `accounts.node_probation` (Go duration, default `168h`) from the account's first node token; revoking and recreating does not reset it |
| Share cap | no untrusted operator gets more than about a third of the requests placed in the last 10 minutes while other nodes are free (soft: if only its nodes are free, they serve) |
| Per address | at most 3 connected account nodes per client address (IPv6 per /64; the rightmost `X-Forwarded-For` from Caddy) |
| Own nodes | per account: "my own nodes first" (default), "only my own nodes" or "any node", for its chat and API requests |
| Cut-off | revoking a token, `accounts ban` or `accounts node-revoke` drops the node within ~2 s (no restart); requests in flight on it fail over to another node if no output was sent yet |
| Guards | answers from untrusted nodes (not the requester's own): tool-call arguments are held back until the call is complete and checked for download-and-execute (`curl … \| sh`), credential paths (`~/.ssh`, `~/.aws`, `.env`, `id_rsa`; excused if the request names them), encoded commands, file uploads and web addresses that appear nowhere in the request. A flagged call is never released: the request is retried on a free trusted node or fails with 502, the node is **held** (no requests until `accounts node-clear`), and the event is logged without content (usage log `outcome: blocked`, `guard: <rule>`). Unknown addresses in plain text are only flagged (`guard: unknown-address-in-text`). These are tripwires for crude attacks, not answer checking |
| Context | a request goes only to nodes whose announced context fits its estimated prompt (bytes/3) plus `max_tokens`; if no node fits that, prompt plus 4,096 tokens is enough (llama-server ends the answer at the context limit). If none fits even that: 400 `context_length_exceeded`. `/v1/models` advertises per model the smallest context among connected nodes (ignoring announcements below 8,192 when larger ones exist) |

The account page shows each node as online, busy, offline, probation, paused (by the
volunteer in the local node UI) or held (by a guard), with a generic reason only.

Operator commands (the gateway applies them within seconds):

```bash
aisathome-gateway accounts nodes                  # node tokens, held nodes, settings
aisathome-gateway accounts nodes close|open       # stop or allow new node tokens
aisathome-gateway accounts nodes limit 10         # node tokens over all accounts (max_nodes, default 10)
aisathome-gateway accounts node-revoke n-1a2b3c4d # delete one node token
aisathome-gateway accounts node-clear n-1a2b3c4d  # let a held node serve again
aisathome-gateway accounts routing trusted-only   # emergency: everything to trusted nodes; "any" to undo
aisathome-gateway accounts ban gh:12345           # also deletes the account's node tokens
```

Beyond `max_nodes` the page says "the network is full for now" (there is no waitlist for
nodes).

#### Email sign-in (one-time code and link)

An `email` section inside `accounts` adds "Email me a code" (no passwords). All fields are
required:

```json
"email": {
  "smtp_host": "email-smtp.eu-north-1.amazonaws.com",
  "smtp_port": 587,
  "smtp_user": "<SES SMTP user>",
  "smtp_password": "<SES SMTP password>",
  "from": "AIs@home <noreply@aisathome.org>",
  "email_keys": [{"id": "2026-10", "secret": "<32+ random characters>"}]
}
```

- **What is stored.** Never the address: an email account gets a random id (`em:<hex>`) and
  `email_macs`, the normalised address as HMAC-SHA256 under a key of `email_keys` (still
  personal data). Normalisation: trim, lowercase, and for gmail.com/googlemail.com without
  dots and `+tag`. Version 1 takes ASCII addresses only (no NFC/IDNA needed; a Unicode
  domain can be typed in its `xn--` form). Codes and link tokens live only in memory, as
  SHA-256 hashes; a restart drops pending sign-ins.
- **Key rotation.** Put a new key first in `email_keys` and keep the old ones after it. A
  sign-in found under an old key is rewritten under the current one; drop an old key once
  the accounts that matter have signed in again (accounts still under it can no longer sign
  in by email).
- **The flow.** `POST /account/email` always answers the same and sets a 10-minute `pending`
  cookie; the mail goes out from a queue afterwards (SMTP with STARTTLS required). It holds a
  6-digit code and a link `<public_url>/account/email#t=<token>` (token in the fragment, so
  it never reaches a server log). The code or link works only in the browser that asked
  (the pending cookie), once, for 10 minutes; mail scanners opening the link change nothing.
- **Limits** (in memory). Sends: per address 1/minute, 5/hour, 10/day; per client 10/hour,
  30/day (rightmost `X-Forwarded-For` when the connection comes from Caddy on loopback or a
  private network; IPv6 per /64); 300/day in all. Wrong codes: 5 end an attempt; 20 per
  address in 24 h pause email sign-in for that address for an hour (a new code does not
  reset the count). Requests over a limit, for throwaway domains, or during a pause get the
  same answer and no mail.
- **Throwaway domains.** `gateway/disposable_domains.txt` is the CC0 list from
  [disposable-email-domains](https://github.com/disposable-email-domains/disposable-email-domains),
  embedded at build time. Refresh it with
  `curl -fsSL https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/main/disposable_email_blocklist.conf | grep -E '^[a-z0-9.-]+$'`,
  keeping the header comment (update its commit and date).
- **SES.** Verify the sending domain (DKIM), request production access, and keep
  account-level suppression on (the default): SES then keeps addresses that bounce or
  complain on its suppression list, in clear. The privacy page says so.

## Credit

Volunteers earn credit for the GPU time their nodes give others (decision 0007; owner,
2026-10-11). It is recognition in BOINC's unit, never money, and it is anchored to BOINC's
cobblestone for decode: one output token of a 27B model is about 2 × 27e9 FLOPs, and
4.32e11 FLOPs make one credit.

| Per token | Credit |
|---|---:|
| output (generated: text, reasoning, tool calls, end of turn) | 0.125 |
| new prompt token | 0.0125 |
| cached prompt token | 0.00125 |

- **Counted by the gateway.** It renders the forwarded request with Qwen3.8's chat template
  (`gateway/count.go`) and counts it with its own byte-level BPE tokenizer
  (`gateway/tokenizer.go`); the output from the text and tool calls it relayed. Node-reported
  counts are kept in the usage log (`usage`) but not used: each line has `counted`
  (`"gateway"`, or `"node"` without a tokenizer, when the node's counts are used, clamped as
  for budgets), `tokens` and `credit`.
- **Cache decided by the gateway.** A prompt counts as cached up to the longest prefix the
  same node served the same requester (account, chat user or key) within 10 minutes. Only
  chained hashes of 256-byte blocks are kept, bounded per node (32 conversations of up to
  128k tokens) and in all (256 nodes). The node's cache claim is ignored.
- **No credit** for self-served answers (an account's own node), failed or blocked answers.
  Partial answers the client cancelled count.
- **Ledger** per account, per node and per config-token operator (the maintainer's nodes,
  kept apart from accounts): today (UTC), total and BOINC's `expavg_credit`
  (`update_average` from `lib/util.cpp`, 7-day half-life; the first grant is spread over the
  time since the account or node was created, at least a day).
- **Usernames.** An account may set a username on its account page (3–32 letters, digits,
  spaces, `- _ . '`; unique ignoring case; no web addresses). Setting it is the consent to
  appear publicly; without one an account counts in totals only. `aisathome-gateway accounts
  username-clear <id>` removes an offensive one; `accounts list` shows them.
- **Public statistics** (no authentication): `GET /v1/stats/credit` (leaderboard: top 100
  named volunteers, anonymous and maintainer totals; rebuilt at most every minute), and
  `/stats/tables.xml`, `user.gz`, `host.gz`, `team.gz` in BOINC's `db_dump` format, named
  accounts only, rebuilt at most hourly, with an ETag (Gridcoin's scraper needs one). Node
  ids, labels, account ids and pseudonyms never appear; hosts have numeric ids. No CPIDs yet.
  The reverse proxy must route `/stats/*` to the gateway (like `/account*` and `/v1/*`).

**The tokenizer file.** Qwen3.8's `tokenizer.json` (12.8 MB, not in the repo):

```bash
curl -fLo tokenizer.json \
  https://huggingface.co/Qwen/Qwen3.8-27B/resolve/1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0/tokenizer.json
sha256sum tokenizer.json   # 0997f410c57a1f4e53b09e4be8f4a172d90edd9564368fb0847030937229b9f3
```

Set `"tokenizer_file"` to its absolute path and `"tokenizer_sha256"` to that hash. It uses
about 30 MB of memory and counts roughly 2 million tokens a second. Only the Qwen3.5+
pre-tokenizer is supported (the gateway checks the regex when it loads the file). Like
llama.cpp, the gateway skips the file's NFC normalizer, so text that is not in NFC can count
a token or two differently than Hugging Face does. The tests check the tokenizer against
Hugging Face `tokenizers` (`scripts/tokenizer-fixture.py`) and the template against jinja2
(`scripts/template-fixture.py`); CI uses a cut-down copy of the file
(`gateway/testdata/qwen38-mini-tokenizer.json`), and `AISATHOME_TOKENIZER=/path/to/tokenizer.json
go test ./gateway -run TestTokenizerFullFile` checks the full one.

## Licence

Apache-2.0 (see `LICENSE`). Each source file carries an SPDX header. The name "AIs@home" is
not licensed with the code.
