# Running a gateway

For operators of a gateway (the project server). Volunteers who want to run a node read
[the site's node docs](https://aisathome.org/node/) instead. The protocol between gateway and
nodes is in `protocol/PROTOCOL.md`.

Config fields beyond the example (`examples/gateway.json`):

| Field | What |
|---|---|
| `state_dir` | Directory for state that survives restarts (daily token budgets, the credit ledger `credit.json` and the daily history `history.json`, saved every 5 s and on shutdown; the task queues; the science database `science.db`, below). Without it a restart gives every key a fresh day and forgets credit, and the science records live in memory. A corrupt budgets file is logged and ignored; a corrupt ledger is set aside as `credit.json.bad`. |
| `tokenizer_file`, `tokenizer_sha256` | The model's `tokenizer.json`, so the gateway counts tokens for credit itself (see [credit.md](credit.md)); the gateway refuses to start if the file does not match the SHA-256. |
| `node_protocol_min`, `node_protocol_max` | The node protocol versions the gateway accepts (`protocol/PROTOCOL.md`, "Protocol versions"). Default: the gateway's own version and the one before it, so nodes of the current and the previous release connect. A node outside the range is refused with close code 4001 and a reason its volunteer sees, logged as `msg="node refused: unsupported protocol version"`. Every connect logs `protocol=<version>`; the account page shows it next to the node's software version. Max cannot exceed the gateway's own version; raise min only on a published retirement date. |
| `guard_known_addresses` | Web addresses the guards count as known (not flagged) in tool calls from untrusted nodes even when the request does not name them (see "Guards" below): `"host"` (with its subdomains) or `"host/path"` (that host, the path and below, segment by segment), lower case, no scheme. Absent: the built-in list of Lean and Mathlib sources (`github.com/leanprover-community`, `github.com/leanprover`, `leanprover-community.github.io`, `lean-lang.org`, `cache.mathlib.org`, `lakecache.blob.core.windows.net`); `[]` turns it off; a list replaces it. Read at start. |
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

**Logprobs.** `"logprobs": true` with `top_logprobs` (llama-server takes up to 20) returns
the node's token logprobs in OpenAI's shape: per streamed chunk, or joined into
`choices[0].logprobs.content` of a non-streaming answer. Without `"logprobs": true` the
gateway removes logprobs a node sends anyway. They come from the node, unverified, like the
answer. With `restore_secrets` the answer text gets the secrets back but the logprobs keep the
placeholders the node saw. A non-streaming answer is buffered up to 2 MB, about 1,300 tokens
with `top_logprobs` 20: stream longer ones.

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
class the order is as before. Science requests wait in their own queue, so they never fill the
queue of people's requests (`max_queued` applies to each). The usage log records `class` (and
`project`) for each request.

**Preemption.** A chat or agent request that has waited `preempt_after_seconds` (default 8;
`-1` = off) for a node stops a running science request on a node it could use (judged as for
placement, with its own context threshold): the one that started last, so the least work is
lost. The node is told to cancel it, as when a client leaves; once the node confirms, the
waiting request gets the slot. A request preempts one science request at a time (another only
if someone else took the freed slot), and people's requests are never preempted. The science
request:

- if nothing of it reached its client yet (it was still reading the prompt, or the client did
  not ask for a stream), the gateway sends it again itself, behind the people (at most 3 times,
  then 503 below);
- otherwise its stream ends with an error event `{"error": {"message": "preempted for a person:
  ... try again shortly", "type": "server_error", "code": 503, "reason": "preempted"}}`, and a
  request that could not be placed again gets HTTP 503 with `Retry-After: 10` and `"code":
  "preempted"`. Both are retryable for OpenAI-compatible clients; opencode v2 continues the
  step.

A preempted attempt is logged with outcome `preempted`, counted as `preempted` per class and
project in `/v1/stats`, credited to the node for what reached the client (as a cancelled
answer), and charged to the project's quota only if something reached the client.

A **project** is a workload the operator runs (decision 0011), for example an agent runner.
Everything the gateway knows of a project is this data; no project's name or workflow is in the
code (decision 0019). It exists only in the config, has its own keys, its own daily quota instead of
an account's budget, the lowest priority, and a cap on its share of the nodes:

```json
"projects": [{
  "id": "prove2me",
  "key_sha256": ["<64 hex characters>"],
  "daily_tokens": 0,
  "max_node_share": 0.7,
  "agent_ok": true,
  "queue_timeout_seconds": 300,
  "kinds": ["p2m-v7"],
  "public_queue": true,
  "page": "/science/prove2me/",
  "page_anchor": "leaf",
  "public_links": ["https://prove2.me/theorems/"]
}]
```

| Field | What |
|---|---|
| `id` | Lower-case letters, digits and dashes. It is public: `/v1/stats` counts requests, tokens and node time per project. |
| `key_sha256` | SHA-256 (hex) of each of the project's keys. Keys are never in the config. |
| `daily_tokens` | The project's budget in weighted tokens per UTC day, shared by all its keys; `0` = no limit. Accounts' budgets never apply to it. Over it, requests get 429 with `Retry-After`. |
| `max_node_share` | At most this share of the nodes that may serve a request work for the project at once (rounded down, at least one node, so a small network still does science when people leave it idle); `0` = no cap. |
| `agent_ok` | The project's requests with tools (agents) may go to volunteer nodes past their probation, as an account key with consent may; without it they go to trusted nodes only. Requests that look like personal data still go only to trusted nodes, and secrets are redacted, as for everyone. |
| `pilot_probation_ok` | With `agent_ok`: the project's agents may also use connector pilots (below) whose account is still in probation (decision 0020: science, where every proof is checked). Standalone nodes in probation stay excluded. |
| `min_trust` | `"trusted"` keeps all its requests on trusted nodes. |
| `rpm`, `concurrent` | Request limits for the project (all keys together); `0` = none. |
| `queue_timeout_seconds` | How long its requests wait for a node (default the gateway's `queue_timeout_seconds`); science waits behind people, so give it more. |
| `kinds` | The task kinds the project may create besides `other`: one per scaffold generation (`p2m-v7`), 1-32 lower-case letters, digits, `_` or `-`. Its workers claim by kind, so a worker never takes a task of a generation it does not run. None: `other` only. |
| `public_queue` | Publishes the public fields of the project's tasks without a key, at `GET /v1/projects/<id>/queue` and in `GET /v1/tasks` (below). Never the spec, the title, the worker, the lease, the error text or the raw evidence. Built at most every 15 s. The website's prove2me page reads it. Off: 404, and none of its tasks in `/v1/tasks`. |
| `page`, `page_anchor` | The project's page on the website (a path such as `/science/prove2me/`), and a label name whose value places a task on it (`leaf`: `#leaf-<value>`). The account page links a node's science work there. Both optional. |
| `task_compact_days` | Days after a finished task's end until it is compacted (below), 1-3650; default 30. |
| `public_links` | The https prefixes (each ending in `/`, at most 8) that the project's campaign items and external results may link to (below). None: they carry no links. |

Before decision 0019 the gateway also ran TauCeti's review loop (`auto_rounds`, `review_loop`,
`max_review_rounds`, and the `review`, `decide` and `continue` calls) and kept TauCeti's public
results (`/v1/projects/<id>/science`). They are gone; a config that still has those keys starts,
and they are ignored.

Make a key with:

```bash
aisathome-gateway project-key /path/to/prove2me.key   # writes the key (mode 600), prints its SHA-256
```

The key goes only into the file (an existing file is never overwritten); put the printed hash
into `key_sha256` and give the file to the project's runner. Project keys are the operator's:
their answers carry the served-by headers (above). Projects are read at start and on SIGHUP;
removing a hash revokes the key. A config with an invalid project does not start (on SIGHUP the
old config stays).

A task is created with `kind`, `spec` (an object the worker reads, at most 64 KiB) and,
optionally, `order`: after tasks that came back from a lease, lower `order` is claimed first
(default 0), then oldest first. A project that runs a planned sequence sets it, as prove2me's
trials do (calibration leaves first, then a comparison set, then new leaves). The gateway does
nothing at a task's end: its worker reports `complete` or `fail` and that is all, and what comes
next is the project's business.

A task may also carry, written when it is created (decision 0019), the typed fields every project
shares, all of them public:

| Field | What |
|---|---|
| `role` | Its part in the project's pipeline (`outline`, `hole`, `rest`, `assembly`), like a kind. |
| `parent` | The task that defined its place (a hole's outline). |
| `needs` | The tasks whose results it uses (at most 64), for information: the gateway schedules nothing by it. |
| `retry_of` | The try before it. The gateway counts `try` from it: 1, or one more than that task's (2 when it is gone). |
| `replaces` | The task it supersedes (the outline before). |
| `labels` | The project's own short public names (`trial`, `leaf`, `alias`, `outline`, `hole`, ...): at most 16, names of lower-case letters, digits and `_`, values of 1-200 printable bytes, nothing key-shaped and no e-mail address. |
| `hole_id` | The hole it works on (prove2me: `h-` and 12 hex of its ledger key). |

`closed_by` comes with `complete` or `cancel` when a task ended without work because something else
closed it: another task's id, or `reuse:<by>`. A claim names the `kinds` its worker runs (none: any).
A task made before these fields has none of them, and the public views read its spec as before.

**Retention.** The gateway keeps every task: a finished task (done, failed, cancelled) is never
dropped, and only open tasks (queued and leased, at most 500 per project) can make `create` refuse.
A finished task is compacted `task_compact_days` after its end: its spec, events, lease, error text
and live counts go; its public fields, result, `updated_at` and `exported_at` stay, so the public
views show it as before. It keeps `spec_sha256` (the spec's SHA-256), its title and creator, so a
repeated `create` of it still answers 200 (other content: 409). A task made before the typed fields
keeps the part of its spec the public views read. While the finished tasks not yet compacted hold
more than 64 MiB, the oldest are compacted early. A compacted task is about 1 KB plus its result's
`score`. `POST .../tasks/{task}/exported` and `?exported=` remain for a project's own snapshots and
gate nothing.

`GET /v1/tasks`, without a key, lists the tasks of every project with `public_queue` in one schema:
`project, id, kind, role, state, outcome, try, parent, needs, retry_of, replaces, closed_by, labels,
hole_id, steps, model_min, tokens, prompt_tokens, cached_tokens, completion_tokens,
uncached_prompt_tokens, tokens_computed, tokens_with_cache, created_at, started_at, finished_at,
updated_at, node_classes, live` (`node_classes`: the classes, trusted, volunteer or pilot (a connector's pilot, below), of the nodes that
served the requests that named the task; `live`: below). A field a task does not have is `null`.
Query `project`, `state`, `kind`, `role` (comma-separated), `limit` (1-500, default 100) and `after`
(the answer's `next`); newest first.

**Token counts.** A finished task's counts are its worker's. Read the explicit ones:

| Field | What |
|---|---|
| `uncached_prompt_tokens` | Prompt tokens the model computed (cache re-reads left out). |
| `cached_tokens` | Prompt tokens re-read from a node's cache. |
| `completion_tokens` | Tokens written. |
| `tokens_computed` | `uncached_prompt_tokens + completion_tokens`: the work the GPUs did. |
| `tokens_with_cache` | `tokens_computed + cached_tokens`: every token the model read or wrote. |

The explicit fields are `null` when the worker did not report `cached_tokens` (prove2me's tasks before
2026-11-14): then nobody knows what its `prompt_tokens` held. `prompt_tokens` and `tokens`
(`prompt_tokens + completion_tokens`) keep their old meaning for existing readers: for a task that
reported `cached_tokens` they leave the cache out (so `tokens` equals `tokens_computed`), for one
that did not they are an incomplete count. New readers should not use them.

A project's model request may name the task it belongs to in an `X-Aisathome-Task` header, and the
gateway uses that name only when it is one of the project's running tasks (any other name is
ignored, and the request is served as usual). It tells a node's operator, on the account page, which
task the node is working on (without the header, a project with one running task is taken to be
working on that one), and, for a project with `public_queue`, it counts the task's progress live.

**Live counts.** While a task runs, `GET /v1/tasks` and `GET /v1/projects/{id}/queue` show what the
gateway itself relayed for it so far, from the model calls that named it and ended with a whole
answer (streamed or not):

```json
"live": {"requests": 41, "prompt_tokens": 1520330, "cached_tokens": 1398112,
         "completion_tokens": 18227, "uncached_prompt_tokens": 122218, "tokens_computed": 140445,
         "tokens_with_cache": 1538557, "model_s": 1310.4, "updated_at": "2026-11-15T14:02:11Z"}
```

| Field | What |
|---|---|
| `requests` | Model calls that ended ok (close to the worker's steps; a failed, cancelled or preempted call does not count). |
| `prompt_tokens` | Prompt tokens of those calls, the cached ones included, as the node reported them (clamped like credit). |
| `cached_tokens` | The part of `prompt_tokens` read from the node's cache. |
| `completion_tokens` | Tokens written. |
| `uncached_prompt_tokens`, `tokens_computed`, `tokens_with_cache` | As in a finished task's counts (above): `prompt_tokens - cached_tokens`, that plus `completion_tokens`, and `prompt_tokens + completion_tokens`. Unlike a result's, `live.prompt_tokens` includes the cached ones. |
| `model_s` | Seconds the gateway measured from sending each call to the node to its end (prefill and any wait on the node included). |
| `updated_at` | When the last counted call ended. |

`live` is `null` (on the queue view: absent) until a call counted, and again once the task ended:
then the result's numbers, which the worker reports, are the ones to show. A claim that starts a new
try starts from zero; one that continues an interrupted try goes on counting. The count is kept with
the task and saved with its next heartbeat, so a gateway restart loses at most the calls since then.
Both views are built at most every 15 seconds.

### The task queue when the disk fails

A project's task queue lives in `state_dir` (`tasks-<project>/`), one file per task. A write that
fails (a full disk, a read-only directory) is handled like a crash of that project's queue, and what
is on disk stays the truth:

- A call whose change did not reach the disk answers 503 with `Retry-After` and changes nothing, so
  a worker repeats it. The log says `task queue write failed`, with the file and the reason.
- A queue that cannot be read answers 503, never an empty list, and the ops page says so.

The queue heals by itself once the disk can be written again: free the space or fix the directory;
no restart is needed, and a restart does no harm. While it lasts, a worker's lease cannot be renewed,
so a fault longer than the lease (10 minutes by default) sends those tasks back to the queue when
the disk returns; the worker takes them back without losing a try.

### Campaigns and external results

A project's public record of what it ran and what came of it outside the network (decision 0021),
kept in the science database. What they mean for a project stays in its labels and its own code.

- A **campaign** is a named round of work (prove2me's trials): `id` (1-64 letters, digits, `.`, `_`,
  `-`), `title`, `status` (`preparing`, `running`, `finished`), `started_at`, `ended_at`, `labels`
  (as a task's), `text` (at most 4 KiB, line breaks allowed: for instance the owner's closing
  words) and `items`, the things it works on (at most 200): `{id, title, url, role, labels}`.
- An **external result** is what an outside service said about a piece of the work (prove2me's
  verdict on a submitted proof): `source` (`prove2me`), `external_id` (the service's own id),
  `campaign`, `item`, `task` (the gateway task that produced it; all three optional), `url`, `state`
  (`pending`, `accepted`, `rejected`, `error`), `verdict` (the service's text, at most 1000 bytes),
  `labels`, `submitted_at` and `verdict_at`.

Writes take the project's own key (401 without one, 403 with another project's):

```
PUT    /v1/projects/{id}/campaigns/{campaign}   create or replace the whole campaign: 201, or 200
PATCH  /v1/projects/{id}/campaigns/{campaign}   change the fields given; finish: {"status": "finished"}
DELETE /v1/projects/{id}/campaigns/{campaign}   remove it (409 while results refer to it)
POST   /v1/projects/{id}/results                create or update, by (source, external_id): 201, or 200
DELETE /v1/projects/{id}/results/{result}       remove it
```

Both writes are idempotent: the same body again answers 200 with `"created": false` and changes
nothing (not even `updated_at`). The rules for times: a campaign that becomes `running` gets
`started_at` now unless it has one, one that becomes `finished` gets `ended_at` now unless it has one,
and one that is not finished has no `ended_at` (reopening clears it). A result that is not `pending`
gets `verdict_at` now unless it has one. A result with a verdict never goes back to `pending`: a late
retry of the submission changes nothing. A result's `campaign` must exist first. A project keeps at
most 1000 campaigns and 50,000 results (409 beyond).

Reads take no key for a project with `public_queue` (cached 15 s); another project's need its key:

```
GET /v1/projects/{id}/campaigns                  newest first (started_at, else created_at); limit (1-200, default 50), after
GET /v1/projects/{id}/campaigns/{campaign}       one
GET /v1/projects/{id}/results                    newest submitted first; campaign, item, source, state (comma-separated), limit (1-500, default 100), after
```

A campaign comes with its items in order and `results`, its results per state. The results list
comes with `counts` per state for the same filter without `state`, and `next` for the page after.

Everything they show is public, so everything is checked when written: ids and labels as a task's;
`title`, `text` and `verdict` refuse control characters, key-shaped strings (API keys, node tokens
`nt-…`, cloud and GitHub tokens, private keys) and e-mail addresses;
a `url` must be https, without query or fragment, under one of the project's `public_links`. A
result's `external_id` is in the writer's answer only, never in a read (a service's ids may let
others ask it about the submission, or who made it); its public `id` is `r-` and 16 hex characters
of a hash of `source` and `external_id`. Never in them: prompts, workers, node ids, operators,
accounts or keys.

**The science database.** `state_dir/science.db`, SQLite (WAL), with the schema's version in its
`user_version`; the gateway brings it up to date at start. A database that is damaged, or from a
newer gateway, stops the start with a message saying so, rather than losing records. Backups: the
data volume's snapshots hold it like any file (SQLite recovers its write-ahead log as after a
crash), and once a UTC day the gateway writes a clean copy beside it, `science-YYYY-MM-DD.db`
(`VACUUM INTO`), keeping the two newest, so every snapshot also holds a consistent copy that opens
on its own. To restore one: stop the gateway, replace `science.db` with the copy, delete
`science.db-wal` and `science.db-shm`, start it. Tasks are not in it: they stay one JSON file each. A
failed read or write answers 503 "science records unavailable, try again" and logs `science
database failed` with the operation, the project and the cause (a full disk, a damaged file), never
a record's content.

## Public statistics

Without authentication, for the website (aggregates only: never users, keys, nodes or
operators):

- `GET /v1/stats`: nodes online, slots, busy, paused and held now (a held node counts only in `held`); today's (UTC) requests and tokens,
  in all, per class (`today.classes`) and per project (`today.projects`, with `node_seconds`),
  each with `preempted` (of its requests, those stopped for a person's) when not zero; totals
  since the usage log began (`total`, with `total.projects`). Here `prompt_tokens` includes the
  tokens re-read from the nodes' caches; each class and project also has `cached_tokens` and
  `uncached_prompt_tokens` (the part the model computed).
- `GET /v1/stats/history`: one entry per UTC day (oldest first, at most 400 days, today
  included so far): `requests`, `completed`, `prompt_tokens`, `completion_tokens`, `classes`
  (per class: `requests` and tokens), and `nodes_peak` / `nodes_avg` (nodes online, sampled
  every minute; missing for days before the gateway kept samples). Kept in
  `state_dir/history.json`; on start the days the usage log still covers are recounted from it.
- `GET /v1/stats/pressure?range=24h|48h|7d|14d|30d|90d` (default `7d`): how full the network was,
  per UTC hour (up to 14 days) or per UTC day (up to 90 days), oldest first, hours or days without
  data left out; `total` sums the range and `last_24h` the last 24 hours; `now` is the current
  sample. Per bucket: `slots` and `busy` (GPU slots on nodes that serve: not held, not paused; a
  node's slots are its largest model's), `utilisation` (busy slot-time over slot-time online,
  missing without slots), `queue.people` and `queue.science` (requests waiting for a slot),
  `held` (nodes held by a guard) and `holds` (new holds), `science_slots` (slots science may use:
  each project's trust and agent rules and its share of nodes), `open_tasks` (science tasks queued
  or running) and `demand` = min(open tasks, science slots), per project then in all, the signal a
  runner pool can be sized by. Each of these is sampled every 10 s and given as `avg` and `peak`
  (the queue's peak also counts every request that starts to wait). `wait_s` (from arrival to a
  slot, or to giving up) and `first_token_s` (from arrival, once its body is read, to the first
  generated token the gateway receives) per class: `count`, `p50`, `p95`, in seconds, each the
  upper edge of a log-spaced bin (never more than the longest seen; 0 = no wait). `outcomes`: one
  per request, however many nodes it tried: `ok`, `failed`, `blocked` (a guard),
  `refused` (no node took it: busy until its time ran out, queue full, no node with the model or
  context), `preempted` (science given up for a person) and `cancelled` (the client left).
  Answers are built at most every 15 s. Kept in `state_dir/pressure.json`, written every minute
  and at shutdown.
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
| GitHub (OAuth app, no scopes; `user:email` only with the user's opt-in) | `github_client_id`, `github_client_secret` | `<public_url>/account/callback` |
| Google (OpenID Connect, scope `openid`; `email` only with the user's opt-in) | `google_client_id`, `google_client_secret` | `<public_url>/account/callback/google` |

Only the provider's user id is stored: `gh:<numeric id>` or `google:<sub>`. No name or token, and
no email unless the user opts in to notices (below). Google's ID token is checked against Google's published keys (RS256, issuer,
audience, expiry, nonce) and the flow uses state and PKCE. The same person signing in with
GitHub and with Google gets two separate accounts. Manage accounts with
`aisathome-gateway accounts list|limit|close|open|add|activate|waitlist|ban|contacts|contact-clear|notice` (nodes: see
below); `list` with `-config` also shows the limits each account has and its weighted tokens
used today (UTC), the account page's number, from `state_dir/budgets.json`. Run it as the
gateway's own user (in a container: `podman exec aisathome-gateway aisathome-gateway accounts
...`), otherwise the file may become unwritable for the gateway. `waitlist` holds an account
back until you `activate` it. The usage log names accounts by a pseudonym (an HMAC of the id),
never by their GitHub, Google or email id.

### Contact address and notice

- **Opt-in.** Under the sign-in buttons is an unticked box, "Email me about important changes
  and outages". Its state travels with that one sign-in (the OAuth flow cookie, which the
  callback clears, or the pending email sign-in in memory), and only a same-origin form post
  carries it: `GET /account/login` never asks for an address. Ticked, GitHub's flow adds scope
  `user:email` and the gateway keeps the primary address if GitHub marks it verified; Google's
  adds scope `email` and the address is kept only if `email_verified` is true; email sign-in
  keeps the address as typed once the code or link proves it. Without a verified address,
  sign-in still works and the account page says that none was available. An unticked sign-in
  leaves an earlier choice as it is.
- **Stored.** `contact_email`, `contact_consent_at` and `contact_source` (`github`, `google` or
  `email`) in the account's record, nothing else. Never shown publicly (the account page shows
  it masked), never logged, never in the usage log, never sent to nodes. Turning it off on the
  account page deletes it at once; `accounts ban` and `accounts contact-clear <id>` delete it
  too. "Connect email" on the account page runs the provider's flow again with the scope, for
  the signed-in account only; an email account types its sign-in address instead, which must
  match the stored hash.
- **Writing.** `aisathome-gateway accounts contacts [id]` lists the addresses with consent time
  and source, for the operator to write from by hand. Nothing is mailed automatically.
- **Provider setup.** A GitHub OAuth app needs no change (scopes are asked per sign-in). For
  Google, add the `.../auth/userinfo.email` scope (non-sensitive) to the OAuth consent screen's
  data access.
- **Notice.** `accounts notice set "<text>"` (one line of plain text, at most 300 characters),
  `accounts notice clear`, and `accounts notice` to show it. It lives in the accounts file, so
  the running gateway shows it within seconds. The account and email sign-in pages show it as a
  banner with a Dismiss button (a cookie with the notice's id, so a new notice shows again).
  `GET /account/notice/chat` gives it in Open WebUI's banner format (v0.11.4,
  `GET /api/v1/configs/banners`): point that path of the chat at the gateway in the reverse
  proxy, and the chat shows it, dismissible, above the conversation. The text is escaped for
  Markdown and HTML, so it is shown as typed.

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
| Personal data (email addresses, card numbers, IBANs, Norwegian ID numbers) | trusted nodes only (and the requester's own nodes). With none connected it gets the 503 and its reason at once instead of waiting out the queue time, except in the two minutes after a gateway start, while nodes reconnect |
| Requests with `tools` (agents) | trusted nodes and the requester's own, unless the API key was created with "allow volunteer nodes for agents" (`agents` on the key; `volunteer_agents` for config keys) **and** the node's account is past probation. The chat (Open WebUI) never carries consent |
| Probation | `accounts.node_probation` (Go duration, default `0`: none) from the account's first node token; revoking and recreating does not reset it |
| Share cap | no untrusted operator gets more than about a third of the conversations placed in the last 10 minutes while other nodes are free (soft: if only its nodes are free, they serve). A conversation counts once per window, however many turns go to the node that holds it |
| Per address | at most 10 connected account nodes per client address (IPv6 per /64; the rightmost `X-Forwarded-For` from Caddy) |
| Own nodes | per account: "my own nodes first" (default), "only my own nodes" or "any node", for its chat and API requests; "only my own nodes" applies on top of a front-end key's `only_operator` (if they differ, no node serves) |
| Cut-off | revoking a token, `accounts ban` or `accounts node-revoke` drops the node within ~2 s (no restart); requests in flight on it fail over to another node if no output was sent yet |
| Guards | answers from untrusted nodes (not the requester's own): tool-call arguments are held back until the call is complete and their decoded values (not the JSON text, whose `\n` escapes made words such as `ncat` out of nothing) are checked for download-and-execute (`curl … \| sh`), credential paths (`~/.ssh`, `~/.aws`, `.env`, `id_rsa`; excused if the request names them), encoded commands and file uploads (`curl -F @…`, `scp`, netcat fed from a file, a here-document or a pipe: read only in shell commands and shell scripts, never in other files, so Lean's `have nc : a < b` passes); web addresses that appear nowhere in the request and are not a known source are only flagged (`guard_known_addresses`: agent clients compact long conversations into a summary, so an address the agent read from its own workspace hours before, such as Mathlib's git address in a lakefile, is gone from the request; a known source must be one plain shell word: a plain host name, and after it no quotes but closing ones, no backslashes, `$`, backticks or braces, which would join more text into the host, and at most 1,024 bytes; a path entry matches only a plain path, without `.` or `..` segments or percent-escapes). The stream itself must be well-formed: tool-call pieces after the calls were completed (`tool-call-after-finish`) or keys that differ from the stream's only in case (`malformed-chunk`) stop the answer as well, and non-streaming answers are checked again as assembled. A flagged call is never released: the request is retried on a free trusted node (keeping all of its restrictions: `only_operator`, only my own nodes) or fails with 502, the node is **held** (no requests until `accounts node-clear`) with its rule and a reason in the guard's own fixed words naming the pattern (e.g. "netcat sending a file to a host and port"), and the event is logged without content (usage log `outcome: blocked`, `guard: <rule>`). Unknown addresses are only flagged: the answer reaches the client, nothing is retried and the node is not held; the usage log records `guard: unknown-address` (in a tool call) or `guard: unknown-address-in-text` with outcome `ok`, and nothing of the address. Honest coding agents write such addresses all the time (package indexes, documentation, git clones), and the guard cannot tell them from one a node injected; a download piped into a shell is stopped whatever its address. A **package install from an unknown index** blocks and holds like the other rules (`install-source`, "a package install from an unknown index"): the index's build and install scripts would run on the client. It reads shell commands and shell scripts as above and, for an install only, every address with `://` in it: `pip`/`pip3`/`python -m pip`/`uv pip`/`pipx` install, download or wheel (`--index-url`, `-i`, `--extra-index-url`, `-f`/`--find-links`, a direct `https://` or `git+https://` requirement, `-r https://…`), `pip config set`, `uv add`/`sync`/`lock`/`tool install` and the index flags of `uv run`/`uvx`, `npm`/`pnpm`/`yarn`/`bun` installs (`--registry`, a URL or tarball spec) and `npm config set registry …`, `npx --registry`, `cargo install`/`add` (`--index`, `--git`), `GOPROXY=… go …` and `go env -w GOPROXY=…`, `gem install --source`/`gem sources --add`, `conda`/`mamba -c https://…`, the environment variables these tools read (`PIP_INDEX_URL`, `UV_INDEX_URL`, `npm_config_registry`, ...; assigned anywhere in a command, such as `make PIP_INDEX_URL=…`, and in `echo … >> file` or `echo … | tee`, but not printed or in a comment), and a package manager's own source file written by a write or edit tool (`.npmrc`, `.yarnrc.yml`, `pip.conf`, `uv.toml`, `.condarc`, `.gemrc`, `.cargo/config.toml`, `requirements*.txt`, `yarn.lock`, `pnpm-lock.yaml`; read raw and with JSON, TOML and YAML escapes such as `\/` and `\u002f` decoded; a whole `package.json` or `package-lock.json` that is valid JSON is decoded and read by its structure: every address but the project's own links at the top level of `package.json` (`homepage`, `repository`, `bugs`, `funding`, `description`, `author`, ...) and a package's metadata in a lock file (`funding`, `license`, ...), so dependency maps, `overrides`, `resolutions`, `resolved` and `version` always count; in an edit of these files, which shows no level, every address counts). An index is known if the request names its host (as a whole word: `github.com` does not name `hub.com`; punctuation such as curly quotes ends a word, an IPv6 address matches in any of its forms, and a host longer than DNS allows, 253 bytes, is never named; this holds for flagged addresses too), it is a `guard_known_addresses` source, a local or private address, or a registry the tools use by default (`pypi.org`, `files.pythonhosted.org`, `download.pytorch.org`, `registry.npmjs.org`, `registry.yarnpkg.com`, `proxy.golang.org`, `rubygems.org`, `crates.io`, `conda.anaconda.org`, `repo.anaconda.com`); such an install is at most flagged as an unknown address. A plain `pip install requests`, an address in an `echo`, a comment or a README, and arguments a run command passes to its package (`npx tool https://…`) are no install source. These are tripwires for crude attacks, not answer checking |
| Context | a request goes only to nodes whose announced context fits its estimated prompt plus `max_tokens`. The estimate is the gateway's own count of the rendered prompt plus 1% and 16 tokens (on 713 real requests the count was never under the node's and at most 47 over); without `tokenizer_file` it is a third of the rendered bytes, which is safe but runs up to 1.4 x real. Rendered means what the model sees: reasoning the template drops does not count; if no node fits that, prompt plus 4,096 tokens is enough (llama-server ends the answer at the context limit). If none fits even that: 400 `context_length_exceeded`. `/v1/models` advertises per model the smallest context among connected nodes (ignoring announcements below 8,192 when larger ones exist) |
| One model per session | nodes of different tiers serve the same roles (`deep`, `agent`) from different files under different model names (`qwen3.8-27b` on 24 GB cards, `qwen3.8-27b-q3` on 16 GB). A request that continues an answered conversation of the same requester (its rendered prompt is longer than and starts with all of that conversation, its answer included, byte for byte; secret placeholders compare without their per-request nonce; clients send no common session id, so the gateway recognises it from hashes, keeping no text) goes only to nodes serving the model that answered it, for 24 hours after the last answer. If none is free it waits like any request; if none is online after the queue time: 503 naming the model ("this conversation started on ..."), never a silent switch. A request that names a model is not held. Answers name the model that answered in `model`, streamed or not |

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

A hold shows on `/account/ops` (card "Held nodes": node, rule and reason, since when, connected or not) and in `accounts nodes`, and raises a `node-held` health warning with its rule and time until it is cleared. An admin can clear it on the page too ("Clear hold": the same change as `node-clear`, a same-origin form post, and only the hold the page showed, so a newer one stays); the gateway logs that clear with node, rule and the admin's pseudonym. A guard's hold is written to the accounts file. If that write fails, the node stays held in
memory and the gateway retries every few seconds, logging `HOLD NOT RECORDED` each minute
until it succeeds; `node-clear` works once the hold is in the file. A restart before then
forgets the hold. More than 1024 unrecorded holds hold every node until a restart.

Beyond `max_nodes` the page says "the network is full for now" (there is no waitlist for
nodes).

## Chat rules: operator terms and countries

On top of the rules above, an untrusted node serves people's requests (chat, agents, API calls)
only under two rules (GDPR decisions of 2026-11-19, D3 and D4 option B; `gateway/chatrules.go`).
Science (a project's key) carries no personal data and is not affected. Trusted nodes and the
requester's own nodes (not pilots) are outside both rules.

| Rule | |
|---|---|
| **Operator terms** | the node's account accepted the current operator terms (version and time are recorded in the accounts file, `operator_terms`). On the account page ("Operator terms": the text, a tick that also confirms 18 or older, the country the nodes run in; withdrawable) or, for a connector's user, through the connector (`POST /v1/connectors/{id}/users/{user}/operator-terms`, below). A config node token counts as accepted if it is trusted or names the version (`"operator_terms": "<version>"`). A new terms version needs a new acceptance. Without one the node serves science only. A withdrawal is recorded with its time, and a connector's record of an acceptance from before it (or before the account's own acceptance) changes nothing. The text in the code is a **draft** (version `draft-…`) until the drafted terms are approved; the approved text comes with a new version, which no draft acceptance matches. Keep `pilots_chat_enabled` off while the version is a draft |
| **Country** | a node's country is its connection address's in the GeoIP table (`geoip_country_file`), and only if its operator states the same country (account page, connector, or a config token's `"country"`); a mismatch, no stated country or an address the table does not know count as outside Europe. A request from a user who has not opted in goes only to nodes in the EEA or a country with an adequacy decision (`gateway/transfer_countries.txt`: EU, IS, LI, NO and the Commission's list of 2026-11-19; not the US, whose adequacy covers only DPF-certified organisations, nor Canada, commercial organisations only). With the opt-in it may go to any node with the operator terms |

**The opt-in** (`transfer_consent`, version and time) is per account, unticked by default: a box
on the sign-in form next to the email-notice box (it travels with that one sign-in, like the
contact box; the box carries the version of the text it showed, and a sign-in without the tick withdraws nothing) and a card on the account page ("Volunteers
outside Europe", on and off). The account's API keys and its chat follow it; a withdrawal also
applies to requests still waiting for a node. Requests without an
account (config keys, a front end's anonymous users) have none.

When the rules leave no node free, a request waits for one as any request does and is never sent
elsewhere; after the queue time it gets a 503 that says why (`outside_europe`: "allow volunteers
outside Europe on your account page"; `operator_terms`). The account page shows, per connected node,
whether it serves chat and why not. The sign-in form states the age limit (18+, D12).

```json
{
  "geoip_country_file": "/etc/aisathome/dbip-country-lite.csv.gz",
  "chat_rules_off": false
}
```

| Field | Meaning |
|---|---|
| `geoip_country_file` | DB-IP's "IP to Country Lite" CSV (`first,last,CC` per line, plain or `.gz`), monthly from <https://db-ip.com/db/download/ip-to-country-lite>, licence **CC BY 4.0**: pages that show its results credit "IP Geolocation by DB-IP" with a link (the account page does). Without it every untrusted node's country is unknown, so users without the opt-in get trusted nodes (and their own) only; the gateway logs a warning at start |
| `chat_rules_off` | turns both rules off. On by default; off only for a gateway whose nodes are all the operator's own (as `examples/gateway.json`, one local node), and in tests |

Trusted nodes must run in the EEA or an adequate country (D5); the gateway logs a warning when a
trusted node connects from elsewhere. Countries from addresses are approximate: a VPN changes them,
which is why the stated country must match.

## Connectors and pilots

A **connector** is an outside system that brings machines in bounded serving sessions, **pilots**
(decision 0020). The BOINC project's bridge is the first: a BOINC work unit is one pilot, a run of
`aisathome-node pilot` that serves for up to 4 hours. The gateway is the source of truth: it mints
the pilot, decides what it may serve, meters it and decides its credit; the connector only creates
work items with the pilot's name, says who runs each, and reports when it is done. Nothing in the
gateway is BOINC-specific beyond the configuration.

```json
"connectors": [{
  "id": "boinc",
  "key_sha256": ["<64 hex characters>"],
  "allow_from": ["203.0.113.7"],
  "max_pilots": 10,
  "mint_per_hour": 120,
  "pilots_chat_enabled": false,
  "no_chat_users": ["<Science United's BOINC user id>"],
  "trusted_users": ["<the maintainer's own BOINC user id>"],
  "chat_headroom": 0,
  "sessions_per_slot": 2,
  "unsent_buffer": 2,
  "max_credit_per_pilot": 150000,
  "max_credit_per_account_day": 1800000
}]
```

| Field | What |
|---|---|
| `id` | Lower-case letters, digits and dashes: the `{id}` in its endpoints. |
| `key_sha256` | SHA-256 of its keys (make one with `aisathome-gateway project-key <file>`). A connector key opens only that connector's endpoints: no API, project or account call. It must differ from every project key. |
| `allow_from` | Optional: addresses or prefixes its calls must come from (behind a local reverse proxy, the last `X-Forwarded-For` hop). Others get 403. Also restrict the path at the reverse proxy. |
| `max_pilots` | Its pilots that have not ended, at most (0: none can be created). |
| `mint_per_hour` | Pilots created per hour, at most (default 120). |
| `max_deadline_hours` | A pilot's deadline at most this far ahead (default 336). |
| `pilots_chat_enabled` | Off by default: its pilots serve **science only**. See below. |
| `no_chat_users` | Its users (connector ids) whose pilots serve science only, whatever else says: Science United's account, whose hosts cannot reach a person's switch. |
| `trusted_users` | Its users whose pilots are the maintainer's own machines (rented test hosts): they count as trusted nodes, like a config token with `"trust": "trusted"`, and serve chat even while `pilots_chat_enabled` is off. Never list anyone else. |
| `chat_headroom` | Pilots to keep for people's requests in the demand number, counted only while `pilots_chat_enabled`. |
| `sessions_per_slot` | Science sessions one GPU slot keeps busy (default 2: a session alternates model calls with its own tools). |
| `unsent_buffer` | Created pilots no machine has taken yet, at most (default 2). |
| `max_credit_per_pilot`, `max_credit_per_account_day` | Credit caps (default 150,000 and 1,800,000; `-1`: none). See `docs/credit.md`. |

Connectors need accounts (a pilot serves as an account's node) and work best with a `state_dir`
(the records are in `state_dir/science.db`; without one they are in memory).

### The connector API

All with `Authorization: Bearer <connector key>`, JSON bodies (`application/json`, unknown fields
refused):

| Call | What |
|---|---|
| `GET /v1/connectors/{id}/demand` | How many pilots to create now (`pilots_wanted`), how many unsent ones to cancel (`cancel_unsent`), the `unsent_buffer`, and the numbers behind them (below) |
| `POST /v1/connectors/{id}/pilots` `{"deadline": "<RFC 3339>"}` | Create a pilot: `{"pilot": {...}, "credential": "pc-…"}`, 201. The credential is shown only here; only its SHA-256 is stored. 409 at `max_pilots`, 429 over `mint_per_hour` |
| `GET /v1/connectors/{id}/pilots?state=&limit=` | Its pilots, newest first (`limit` 1-500, default 100) |
| `GET /v1/connectors/{id}/pilots/{pid}` | One pilot: state, deadline, user/host/team, `account` (`linked` or `unclaimed`, never which), times, `close_reason`, `end_reason`, `connections`, `served_seconds`, `requests` (answers that went to the client, whole or in part), `failed` (the others), `self_served`, `tokens`, `metered_credit`, `credit`, `withheld`, `agent` |
| `POST /v1/connectors/{id}/pilots/{pid}/bind` `{"user", "host", "team"}` | Who runs it: the connector's own ids (1-64 letters, digits, `.`, `_`, `:` or `-`; never an address). Binding again with the same ids changes nothing; other ids, or an ended pilot, get 409 |
| `POST /v1/connectors/{id}/pilots/{pid}/reported` `{"outcome": "success\|error\|cancelled\|expired"}` | Its work item is done (or was cancelled): the pilot ends (`end_reason` `reported: <outcome>`) and its connection, if any, is closed. Idempotent |
| `POST /v1/connectors/{id}/users/{user}/operator-terms` `{"version", "accepted_at", "country"}` | The connector's user accepted the operator terms (BOINC: the bridge, when BOINC records the user's ENROLL consent to `terms_of_use.txt`, which holds the operator terms). `version` the terms version accepted, `accepted_at` when (RFC 3339, not in the future), `country` optional: the ISO 3166-1 alpha-2 code the user states (or `other`). Recorded on the user's account, the linked one or a new unclaimed one; a later call with an older time does not replace it. Answer: `{"recorded", "current", "current_version", "account_kind"}`; `current` false means that version lets no pilot serve chat. The user's connected pilots follow within the call |
| `GET /v1/connectors/{id}/pilots/{pid}/credit` | `{"pilot", "state", "final", "final_at", "credit", "withheld"}`: `credit` only once `final`; until then `final_at` says when (null while the pilot has not ended) |

A pilot's **credential** is `pc-<12 hex>.<secret>`; its id is `p-` and the same 12 hex digits, and
the node agent takes its node id from the credential as it does from an account's node token. It
connects over the ordinary node WebSocket (`/v1/node/connect`, protocol 2) and works:

- only after the bind (before it, the connection is refused with 409 "pilot not bound yet" and
  the agent retries within seconds);
- for **one connection at a time** (a second one is closed: "this pilot is already connected");
- until the pilot ends or its deadline passes (then 401 "this pilot has ended");
- while its account may run pilots and is still linked to its user (a banned account's pilots,
  and those of a user unlinked or linked elsewhere, are refused and dropped).

**States**: `created`, `bound`, `connected`, `disconnected` (it may come back), `ended`. A pilot
ends when it closes with `pilot: time`, `pilot: idle` or `pilot: engine`, when the connector reports
it, or when its deadline passes (`end_reason` `deadline`). `pilot: sigterm` (BOINC suspended the
task) and a lost connection leave it `disconnected`; when the task resumes, the same credential
continues **the same record**: `connections`, `served_seconds`, requests, tokens and credit add up
over every run of the work unit. A gateway restart counts a connected pilot's serve time up to the
last minute it was seen (`close_reason` `gateway restart`). The record keeps only the pilot's own
close reasons (`pilot: time|idle|engine|sigterm`); any other text is stored as `closed`, and a
dropped connection as `connection lost`.

### Which account a pilot serves

At the bind the connector's user maps to an account: the one **linked** to that user, or, if none,
a new **unclaimed** account (`cx:<32 hex>`, not derived from the user id) linked to it. Every pilot
of that user lands there; its probation clock starts with its first pilot. An unclaimed account has
no sign-in, keys or node tokens and does not count against `max_active`. Claiming it from the
account page comes later; until then the operator links by hand:

```bash
aisathome-gateway accounts link gh:12345 boinc 678      # this account is BOINC user 678
aisathome-gateway accounts unlink gh:12345 boinc 678
aisathome-gateway accounts chat-on-pilots gh:12345 off  # this account's pilots: science only
```

An account's own API key reads and sets the switch too: `GET` and `PATCH /v1/account/settings`
with `{"chat_on_pilots": false}`.

### What a pilot may serve

A bound pilot is an **untrusted account node** under decision 0007 and every rule in "Volunteer
nodes" above: personal data only to trusted nodes, the share cap per operator (the account's
pseudonym), the guards and holds, requests with tools only with the requester's consent and past
the account's probation, except that a project with `pilot_probation_ok` may use pilots during
it. Unlike a standalone node, a pilot gets no "own node" exemption: the connector, not the person,
made the link, so the account's own personal data does not go to it, and its answers to the
account are guarded too (they still earn no credit). Past its deadline it gets nothing, before the
sweep drops it. On top of that it serves **science only** (requests with a project's key), not chat, agents or
API calls, when any of these holds:

| Rule | Why |
|---|---|
| its user is in `no_chat_users` | Science United's hosts: their owners cannot reach a switch |
| its account's "chat on pilots" is off | the person's choice (on by default, also for unclaimed accounts) |
| `pilots_chat_enabled` is false (the default) and its user is not in `trusted_users` | **people's prompts reach strangers' machines only after anti-retention measures and the privacy notice and GDPR review are in place** (decision 0020: they are done before outside hosts join). Until the operator turns this on at launch, only science (which carries no user data) reaches pilots; the maintainer's own test pilots go in `trusted_users` |

And it serves people's requests only under the chat rules above: its account accepted the operator
terms, and for users without the opt-in, it connects from the EEA or an adequate country that its
account states. Changes to the switch, the lists or `pilots_chat_enabled` (on reload) reach connected pilots within
seconds. Nothing here makes a pilot confidential: whoever runs the machine can read what it serves
("hardened, not confidential"). Public task views name a pilot's class as `pilot`.

### Pilots wanted

`GET …/demand` computes:

```
science sessions = Σ over projects with agent_ok (not min_trust trusted):
                   min(open tasks, runner workers active in the last 5 minutes)
have             = sessions_per_slot × GPU slots online that those projects may use
                   (held and paused nodes and this connector's own pilots left out)
science pilots   = ceil(max(0, science sessions − have) / sessions_per_slot)
chat pilots      = chat_headroom while pilots_chat_enabled, else 0
target           = min(science pilots + chat pilots, max_pilots, spend cap if set)
outstanding      = its pilots not ended (created, bound, connected, disconnected)
pilots_wanted    = min(max(0, target − outstanding), unsent_buffer − unsent, mint_per_hour left)
cancel_unsent    = min(unsent, max(0, outstanding − target))
```

Open tasks are queued or leased; a runner worker counts once it claims (a task or none) or renews a
lease, and the distinct workers holding leases count after a restart. The answer shows each part
(`science`, `chat`, `pilots` by state, `capped_by`). The spend cap is a hook for the monthly cap on
feeding pilots with science; none is set yet. The science part counts the same open
tasks and slots as runner autoscaling's demand, seen from the other side: there runners are
bounded by GPU slots, here pilots by runners, so neither grows the other from nothing; the runner
pool's minimum size seeds both.

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
