# Running a gateway

For operators of a gateway (the project server). Volunteers who want to run a node read
[the site's node docs](https://aisathome.org/node/) instead. The protocol between gateway and
nodes is in `protocol/PROTOCOL.md`.

Config fields beyond the example (`examples/gateway.json`):

| Field | What |
|---|---|
| `state_dir` | Directory for state that survives restarts (daily token budgets, the credit ledger `credit.json` and the daily history `history.json`, saved every 5 s and on shutdown). Without it a restart gives every key a fresh day and forgets credit. A corrupt budgets file is logged and ignored; a corrupt ledger is set aside as `credit.json.bad`. |
| `tokenizer_file`, `tokenizer_sha256` | The model's `tokenizer.json`, so the gateway counts tokens for credit itself (see [credit.md](credit.md)); the gateway refuses to start if the file does not match the SHA-256. |
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

### Which node served a request (operator keys only)

A key with `"served_by": true` (in `keys` in the config, or edited into the keys file), and
every API key that an **admin account** (`accounts.admins`) created on its account page, gets
two headers on every chat answer, streamed or not: `X-AIsAtHome-Served-By` (the node id that
answered; after a failover, the node that actually answered) and `X-AIsAtHome-Request-Id`
(the gateway's id for that attempt). Use it only for your own test runners and dogfooding:
ordinary users must not learn which volunteer served them (it would let them target or map
volunteers). The flag is ignored on `forward_users` keys (the front end's users never see it,
admins included), and other accounts' keys never get the headers. Keep `accounts.admins` to
the operator's own accounts: it also makes them admins in the chat. A key in `api_keys` has no flags; move it to `keys` as
`{"user": "...", "served_by": true}`.

Every attempt is also logged, for every key, as `msg="chat request" req=<request id>
node=<node id> ok=<true|false>` (no user, no content); a failover shows as two lines.

## Workload classes and science projects

Every chat request has a **class**: `chat` (people in the chat front end, through a
`forward_users` key), `agent` (people's own API keys: config keys, keys-file keys and accounts'
keys) or `science` (a project's key, below). When nodes are busy, waiting requests are served
in that order: chat, then agent, then science. A request never takes a node that a waiting
request of a higher class could use, so a science request never delays a person's; within a
class the order is as before. Running requests are never stopped for a higher class. Science
requests wait in their own queue, so they never fill the queue of people's requests
(`max_queued` applies to each). The usage log records `class` (and `project`) for each request.

A **project** is a workload the operator runs (decision 0011; TauCeti first), for example an
agent runner. It exists only in the config, has its own keys, its own daily quota instead of
an account's budget, the lowest priority, and a cap on its share of the nodes:

```json
"projects": [{
  "id": "tauceti",
  "key_sha256": ["<64 hex characters>"],
  "daily_tokens": 0,
  "max_node_share": 0.7,
  "agent_ok": true,
  "queue_timeout_seconds": 300
}]
```

| Field | What |
|---|---|
| `id` | Lower-case letters, digits and dashes. It is public: `/v1/stats` counts requests, tokens and node time per project. |
| `key_sha256` | SHA-256 (hex) of each of the project's keys. Keys are never in the config. |
| `daily_tokens` | The project's budget in weighted tokens per UTC day, shared by all its keys; `0` = no limit. Accounts' budgets never apply to it. Over it, requests get 429 with `Retry-After`. |
| `max_node_share` | At most this share of the nodes that may serve a request work for the project at once (rounded down, at least one node, so a small network still does science when people leave it idle); `0` = no cap. |
| `agent_ok` | The project's requests with tools (agents) may go to volunteer nodes past their probation, as an account key with consent may; without it they go to trusted nodes only. Requests that look like personal data still go only to trusted nodes, and secrets are redacted, as for everyone. |
| `min_trust` | `"trusted"` keeps all its requests on trusted nodes. |
| `rpm`, `concurrent` | Request limits for the project (all keys together); `0` = none. |
| `queue_timeout_seconds` | How long its requests wait for a node (default the gateway's `queue_timeout_seconds`); science waits behind people, so give it more. |

Make a key with:

```bash
aisathome-gateway project-key /path/to/tauceti.key   # writes the key (mode 600), prints its SHA-256
```

The key goes only into the file (an existing file is never overwritten); put the printed hash
into `key_sha256` and give the file to the project's runner. Project keys are the operator's:
their answers carry the served-by headers (above). Projects are read at start and on SIGHUP;
removing a hash revokes the key. A config with an invalid project does not start (on SIGHUP the
old config stays).

## Public statistics

Without authentication, for the website (aggregates only: never users, keys, nodes or
operators):

- `GET /v1/stats`: nodes online, slots, busy and paused now; today's (UTC) requests and tokens,
  in all, per class (`today.classes`) and per project (`today.projects`, with `node_seconds`);
  totals since the usage log began (`total`, with `total.projects`).
- `GET /v1/stats/history`: one entry per UTC day (oldest first, at most 400 days, today
  included so far): `requests`, `completed`, `prompt_tokens`, `completion_tokens`, `classes`
  (per class: `requests` and tokens), and `nodes_peak` / `nodes_avg` (nodes online, sampled
  every minute; missing for days before the gateway kept samples). Kept in
  `state_dir/history.json`; on start the days the usage log still covers are recounted from it.
- `GET /v1/stats/credit` and `/stats/`: credit (see [credit.md](credit.md)).

## A chat front end (Open WebUI)

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

## Accounts (sign-in, self-service keys)

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

## Volunteer nodes (self-registered)

**Trust model.** "Trusted" means *operated by the maintainer* (node tokens in the gateway
config with `"trust": "trusted"`), not "secure": the maintainer's nodes are partly rented,
and whoever controls a machine can read what it serves. Every other node is an untrusted
volunteer. Nothing here is confidential; the measures below make keeping or misusing data
harder and more visible ("hardened, not confidential").

Signed-in active accounts add nodes on the account page ("Your nodes"): up to 10 node tokens
per account (`max_nodes_per_account`), shown once on their own page with a Copy button
(as new API keys are), stored as SHA-256 hashes in the accounts file, revocable there. At
the limit the page says "You have 10 of 10 nodes. Revoke one to add another." The limits
were raised on 2026-10-22 (from 3 per account, 3 connected per address and 10 over all): the
share cap below is what limits how much of the traffic one operator sees, however many nodes it
runs, and low node limits only kept volunteers with several GPUs out. A
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
| Probation | `accounts.node_probation` (Go duration, default `0`: none) from the account's first node token; revoking and recreating does not reset it |
| Share cap | no untrusted operator gets more than about a third of the conversations placed in the last 10 minutes while other nodes are free (soft: if only its nodes are free, they serve). A conversation counts once per window, however many turns go to the node that holds it |
| Per address | at most 10 connected account nodes per client address (IPv6 per /64; the rightmost `X-Forwarded-For` from Caddy) |
| Own nodes | per account: "my own nodes first" (default), "only my own nodes" or "any node", for its chat and API requests; "only my own nodes" applies on top of a front-end key's `only_operator` (if they differ, no node serves) |
| Cut-off | revoking a token, `accounts ban` or `accounts node-revoke` drops the node within ~2 s (no restart); requests in flight on it fail over to another node if no output was sent yet |
| Guards | answers from untrusted nodes (not the requester's own): tool-call arguments are held back until the call is complete and checked for download-and-execute (`curl … \| sh`), credential paths (`~/.ssh`, `~/.aws`, `.env`, `id_rsa`; excused if the request names them), encoded commands, file uploads and web addresses that appear nowhere in the request. The stream itself must be well-formed: tool-call pieces after the calls were completed (`tool-call-after-finish`) or keys that differ from the stream's only in case (`malformed-chunk`) stop the answer as well, and non-streaming answers are checked again as assembled. A flagged call is never released: the request is retried on a free trusted node (keeping all of its restrictions: `only_operator`, only my own nodes) or fails with 502, the node is **held** (no requests until `accounts node-clear`), and the event is logged without content (usage log `outcome: blocked`, `guard: <rule>`). Unknown addresses in plain text are only flagged (`guard: unknown-address-in-text`). These are tripwires for crude attacks, not answer checking |
| Context | a request goes only to nodes whose announced context fits its estimated prompt (bytes/3) plus `max_tokens`; if no node fits that, prompt plus 4,096 tokens is enough (llama-server ends the answer at the context limit). If none fits even that: 400 `context_length_exceeded`. `/v1/models` advertises per model the smallest context among connected nodes (ignoring announcements below 8,192 when larger ones exist) |

The account page shows each node as online, busy, offline, probation, paused (by the
volunteer in the local node UI) or held (by a guard), with a generic reason only.

Operator commands (the gateway applies them within seconds):

```bash
aisathome-gateway accounts nodes                  # node tokens, held nodes, settings
aisathome-gateway accounts nodes close|open       # stop or allow new node tokens
aisathome-gateway accounts nodes limit 25         # node tokens over all accounts (max_nodes, default 25)
aisathome-gateway accounts nodes per-account 10   # node tokens per account (max_nodes_per_account, default 10; "default" unsets it)
aisathome-gateway accounts node-revoke n-1a2b3c4d # delete one node token
aisathome-gateway accounts node-clear n-1a2b3c4d  # let a held node serve again
aisathome-gateway accounts routing trusted-only   # emergency: everything to trusted nodes; "any" to undo
aisathome-gateway accounts ban gh:12345           # also deletes the account's node tokens
```

A guard's hold is written to the accounts file. If that write fails, the node stays held in
memory and the gateway retries every few seconds, logging `HOLD NOT RECORDED` each minute
until it succeeds; `node-clear` works once the hold is in the file. A restart before then
forgets the hold. More than 1024 unrecorded holds hold every node until a restart.

Beyond `max_nodes` the page says "the network is full for now" (there is no waitlist for
nodes).

### Email sign-in (one-time code and link)

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
