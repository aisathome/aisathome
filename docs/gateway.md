# Running a gateway

For operators of a gateway (the project server). Volunteers who want to run a node read
[the site's node docs](https://aisathome.org/node/) instead. The protocol between gateway and
nodes is in `protocol/PROTOCOL.md`.

Config fields beyond the example (`examples/gateway.json`):

| Field | What |
|---|---|
| `state_dir` | Directory for state that survives restarts (daily token budgets, the credit ledger `credit.json` and the daily history `history.json`, saved every 5 s and on shutdown; the guard log `guard-log.json`, below, saved likewise; the task queues; the science database `science.db`, below). Without it a restart gives every key a fresh day and forgets credit, and the science records live in memory. A corrupt budgets file is logged and ignored; a corrupt ledger is set aside as `credit.json.bad`. |
| `site_stats_file` | The site statistics file `tools/sitestats` writes on the same host (daily aggregates only), read for the funnel card on `/account/ops`: visitors, views of the node pages (`/node/`, `/node/windows/`, `/node/install/`, `/node/binaries/`) and install starts (fetches of `/node.sh`). Read-only, reread when it changes. Absent or unreadable: the card shows the gateway's own counts and says why the site's are missing. |
| `node_release_url` | GitHub's API address of the node's latest release, `https://api.github.com/repos/aisathome/aisathome/releases/latest`, served at `GET /v1/node/release` for the website's Windows download ("The latest node release" below). Unauthenticated: no token. Absent: off, and that endpoint answers 503 (the site shows the release it was built with). Read at start. |
| `tokenizer_file`, `tokenizer_sha256` | The model's `tokenizer.json`, so the gateway counts tokens for credit itself (see [credit.md](credit.md)); the gateway refuses to start if the file does not match the SHA-256. |
| `model_check` | The gateway's model check of every node and pilot: secret probe prompts at random times, their logprobs compared with a secret reference ([modelcheck.md](modelcheck.md), "The gateway's check"). `enabled`, `references` (`model`, `file`, `sha256`; the gateway refuses to start if a file is missing or does not match), `interval_seconds` (1800), `recheck_seconds` (300), `withhold` (false: observe-only; when true, a failing node is held, and only after 3 days of observing), `cover_share` (0.2). Results in `state_dir/model-check.json`, on `/account/ops` and `aisathome-gateway accounts model-check`. Read at start. |
| `node_protocol_min`, `node_protocol_max` | The node protocol versions the gateway accepts (`protocol/PROTOCOL.md`, "Protocol versions"). Default: the gateway's own version and the one before it, so nodes of the current and the previous release connect. A node outside the range is refused with close code 4001 and a reason its volunteer sees, logged as `msg="node refused: unsupported protocol version"`. Every connect logs `protocol=<version>`; the account page shows it next to the node's software version. Max cannot exceed the gateway's own version; raise min only on a published retirement date. |
| `guard_known_addresses` | Web addresses the guards count as known (not flagged) in tool calls from nodes even when the request does not name them (see "Guards" below): `"host"` (with its subdomains) or `"host/path"` (that host, the path and below, segment by segment), lower case, no scheme. Absent: the built-in list of Lean and Mathlib sources (`github.com/leanprover-community`, `github.com/leanprover`, `leanprover-community.github.io`, `lean-lang.org`, `cache.mathlib.org`, `lakecache.blob.core.windows.net`); `[]` turns it off; a list replaces it. Read at start. |
| `guard_official_installers` | Install scripts whose download piped into a shell (`curl … \| sh`, `sh -c "$(curl …)"`) stops a node's answer as any other but does **not hold the node** (see "Guards" below): `"host"` (with its subdomains) or `"host/path"` (that path and below), https only, written as `guard_known_addresses`. Absent: the built-in list, from each project's own install page: `sh.rustup.rs`, `astral.sh/uv`, `astral.sh/ruff`, `raw.githubusercontent.com/nvm-sh/nvm`, `deno.land/install.sh`, `deno.land/install.ps1`, `bun.sh/install`, `bun.sh/install.ps1`, `raw.githubusercontent.com/homebrew/install`, `elan.lean-lang.org`, `raw.githubusercontent.com/leanprover/elan`, `install.python-poetry.org`; `[]` turns it off (every such answer holds its node again); a list replaces it. Read at start. |
| `keys_file` | Per-user API keys, managed with `aisathome-gateway keys` (use absolute paths: relative ones resolve from the working directory). The gateway rereads it every 2 s, so no restart or reload is needed. |

Keys in `api_keys` and `keys` (in the config, in clear) still work and reload on SIGHUP.
The keys file stores only a SHA-256 hash and a short prefix of each key:

```bash
aisathome-gateway keys add bob -config gateway.json     # prints the key once
aisathome-gateway keys add bob -tokens-per-day 500000 -rpm 10
aisathome-gateway keys list -config gateway.json        # prefixes, users, limits
aisathome-gateway keys revoke ak-AbCd1234 -config gateway.json
```

New keys default to 2,000,000 weighted tokens per UTC day, 30 requests per minute and 4 in
flight; their requests may go to every node, as everyone's (`only_operator`, edited into the file,
restricts a key to one operator's nodes). Secrets in prompts
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

### Unattended clients

A key with `"unattended": true` (in `keys` in the config, or edited into the keys file) belongs to
a client that runs tool calls without a person approving each one, such as a runner with
opencode `--auto`. The guards stop and hold for it on every node, an account's too (see "Guards"
below); a project's key is always treated so. Every other key and the chat get account nodes'
answers only flagged, and their users must approve commands themselves.

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

A **project** is a workload the operator runs, for example an agent runner.
Everything the gateway knows of a project is this data; no project's name or workflow is in the
code. It exists only in the config, has its own keys, its own daily quota instead of
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
  "public_links": ["https://prove2.me/theorems/"],
  "campaign_auto_finish": {"labels": ["trial", "campaign"], "idle_hours": 6}
}]
```

| Field | What |
|---|---|
| `id` | Lower-case letters, digits and dashes. It is public: `/v1/stats` counts requests, tokens and node time per project. |
| `key_sha256` | SHA-256 (hex) of each of the project's keys. Keys are never in the config. |
| `daily_tokens` | The project's budget in weighted tokens per UTC day, shared by all its keys; `0` = no limit. Accounts' budgets never apply to it. Over it, requests get 429 with `Retry-After`. |
| `max_node_share` | At most this share of the nodes that may serve a request work for the project at once (rounded down, at least one node, so a small network still does science when people leave it idle); `0` = no cap. |
| `agent_ok` | The project's requests with tools (agents) may go to volunteer nodes past their probation, as an account key with consent may; without it they go to no node (a project has no nodes of its own). Requests that look like personal data are refused, and secrets are redacted, as for everyone. |
| `pilot_probation_ok` | With `agent_ok`: the project's agents may also use connector pilots (below) whose account is still in probation (science only, where every proof is checked). Standalone nodes in probation stay excluded. |
| `rpm`, `concurrent` | Request limits for the project (all keys together); `0` = none. |
| `queue_timeout_seconds` | How long its requests wait for a node (default the gateway's `queue_timeout_seconds`); science waits behind people, so give it more. |
| `kinds` | The task kinds the project may create besides `other`: one per scaffold generation (`p2m-v7`), 1-32 lower-case letters, digits, `_` or `-`. Its workers claim by kind, so a worker never takes a task of a generation it does not run. None: `other` only. |
| `public_queue` | Publishes the public fields of the project's tasks without a key, at `GET /v1/projects/<id>/queue` and in `GET /v1/tasks` (below). Never the spec, the title, the worker, the lease, the error text or the raw evidence. Built at most every 15 s. The website's prove2me page reads it. Off: 404, and none of its tasks in `/v1/tasks`. |
| `page`, `page_anchor` | The project's page on the website (a path such as `/science/prove2me/`), and a label name whose value places a task on it (`leaf`: `#leaf-<value>`). The account page links a node's science work there. Both optional. |
| `task_compact_days` | Days after a finished task's end until it is compacted (below), 1-3650; default 30. |
| `public_links` | The https prefixes (each ending in `/`, at most 8) that the project's campaign items and external results may link to (below). None: they carry no links. |
| `campaign_auto_finish` | Finish the project's campaigns by themselves (below, "Campaigns that finish by themselves"): `{"labels": ["trial", "campaign"], "idle_hours": 6}`. `labels` (1-4 label names) are the task labels whose value is the id of the campaign a task belongs to; `idle_hours` (more than 0, at most 720; default 6) is how long its tasks must be idle. None: off, and campaigns end only when the project finishes them. |

Before the typed task schema the gateway also ran TauCeti's review loop (`auto_rounds`, `review_loop`,
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
next is the project's business (one exception: with `campaign_auto_finish`, the campaign of tasks
that have all ended finishes by itself later, below).

A task may also carry, written when it is created, the typed fields every project
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
updated_at, node_classes, live` (`node_classes`: the classes, volunteer or pilot (a connector's pilot, below; `trusted` in tasks from before 2026-11-20), of the nodes that
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
         "tokens_with_cache": 1538557, "model_s": 1310.4, "in_flight": 1,
         "updated_at": "2026-11-15T14:02:11Z"}
```

| Field | What |
|---|---|
| `requests` | Model calls that ended ok (close to the worker's steps; a failed, cancelled or preempted call does not count). |
| `prompt_tokens` | Prompt tokens of those calls, the cached ones included, as the node reported them (clamped like credit). |
| `cached_tokens` | The part of `prompt_tokens` read from the node's cache. |
| `completion_tokens` | Tokens written. |
| `uncached_prompt_tokens`, `tokens_computed`, `tokens_with_cache` | As in a finished task's counts (above): `prompt_tokens - cached_tokens`, that plus `completion_tokens`, and `prompt_tokens + completion_tokens`. Unlike a result's, `live.prompt_tokens` includes the cached ones. |
| `model_s` | Seconds the gateway measured from sending each call to the node to its end (prefill and any wait on the node included). |
| `in_flight` | Calls that named the task and have not ended yet, from the moment the gateway took them (a wait for a node included) to their end, however they end. |
| `updated_at` | When the last counted call ended; absent before the first. |

`live` is `null` (on the queue view: absent) until a call counted or while none is in flight, and
again once the task ended:
then the result's numbers, which the worker reports, are the ones to show. A claim that starts a new
try starts from zero; one that continues an interrupted try goes on counting. The count is kept with
the task and saved with its next heartbeat, so a gateway restart loses at most the calls since then.
Both views are built at most every 15 seconds. `in_flight` is kept in memory only (a restart starts
it from zero) and also shows, for a running task, in the project's own `GET
/v1/projects/{id}/tasks` and task, as `live.in_flight`, which is there whenever that task has a
`live`: a worker can tell a task that waits on the model from one working without it (its tools,
its checks after a session) and size its claims by that.

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

A project's public record of what it ran and what came of it outside the network,
kept in the science database. What they mean for a project stays in its labels and its own code.

- A **campaign** is a named round of work (prove2me's trials): `id` (1-64 letters, digits, `.`, `_`,
  `-`), `title`, `status` (`preparing`, `running`, `finished`), `started_at`, `ended_at`, `labels`
  (as a task's), `text` (at most 4 KiB, line breaks allowed: for instance the project's closing
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

**Campaigns that finish by themselves.** For a project with `campaign_auto_finish`, a task belongs
to a campaign when one of the configured labels has the campaign's id as its value (prove2me:
`trial` on its proof tasks, `campaign` on its submission tasks); a task without them, or made before
labels, belongs to none. Every 5 minutes the gateway finishes each `running` campaign of the project
that:

- has no task `queued` or `leased` (a submission is an ordinary task, so one still waiting keeps its
  campaign open),
- has at least one finished task (`done`, `failed` or `cancelled`),
- whose last task ended at least `idle_hours` ago, and
- has itself not changed for `idle_hours` (a campaign the project has just put back to `running`,
  by hand or with a new plan, is not finished under it).

Its `ended_at` is the last task's end (a done task's result `finished_at`, else when the task
ended), never before its `started_at`; `updated_at` is the time of the sweep. The log says
`campaign finished by itself`. A campaign that is `preparing` is never touched, and one that changes
between the sweep's read and its write is left for the next sweep.

A new task whose labels name a `finished` campaign of the project reopens it: `running`, no
`ended_at`, `started_at` kept (now if it had none); the log says `campaign reopened by a new task`.
A repeated create of an existing task reopens nothing. The sweep finishes the campaign again once
that work is over, with the new last end. The sweep and the reopen take turns, so a task created
while a sweep runs either keeps its campaign open or reopens it right after. A failed reopen is
logged and the task stands. `PATCH {"status": "finished"}` still finishes a campaign at once, for
instance with an `ended_at` of the project's choosing: an override the sweep never undoes (only a
new task reopens it).

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
`science.db-wal` and `science.db-shm`, restore the accounts file from the same snapshot (a connector
request the copy holds as verified is finished against it at start), start it. Tasks are not in it: they stay one JSON file each. A
failed read or write answers 503 "science records unavailable, try again" and logs `science
database failed` with the operation, the project and the cause (a full disk, a damaged file), never
a record's content.

## Public statistics

Without authentication, for the website (aggregates only: never users, keys, nodes or
operators):

- `GET /v1/stats`: nodes online, slots, busy, paused and held now (a held node counts only in `held`); `serve`, how many
  of the nodes online may serve each kind of request from someone who is not their operator right now: `chat` (people's
  chats and API requests without tools), `tools` (agents: `chat` and past probation) and `science` (a project's requests
  without tools). They ask the placement rule itself, so the operator terms, a draft of them, the own-only switch and a
  pilot's science-only switch all count; context and free slots do not. Nodes online with `chat` 0 refuse every chat and
  API request (2026-11-20, `reason=operator_terms` for 8 hours): the operations page then shows a `no-chat-nodes` health
  warning with the reasons (counts), and the log a `WARN` line "no online node may serve people's requests" when such a
  request is refused (at most once per 10 minutes, with the refusals since the last line). Today's (UTC) requests and tokens,
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
  missing without slots), `queue.people` and `queue.science` (requests waiting for a slot) and
  `queue.all` (both together, so its peak is the most that waited at once),
  `held` (nodes held by a guard) and `holds` (new holds), `science_slots` (slots science may use:
  each project's agent rules and its share of nodes), `open_tasks` (science tasks queued
  or running) and `demand` = min(open tasks, science slots) per project, capped by the slots of
  the nodes the projects with work may use (each node once), the signal a runner pool can be sized
  by; `runners` (science runners that reported in the last 150 s). Each of these is sampled every
  10 s and given as `avg` and `peak` (the queue's peak also counts every request that starts to
  wait). `wait_s` (from arrival to a
  slot, or to giving up) and `first_token_s` (from arrival, once its body is read, to the first
  generated token the gateway receives) per class: `count`, `p50`, `p95`, in seconds, each the
  upper edge of a log-spaced bin (never more than the longest seen; 0 = no wait). `outcomes`: one
  per request, however many nodes it tried: `ok`, `failed`, `blocked` (a guard),
  `refused` (no node took it: busy until its time ran out, queue full, no node with the model or
  context), `preempted` (science given up for a person) and `cancelled` (the client left).
  `node` (when an answer finished): `requests`, `prefill_share` (node time reading the prompt over
  node time in all), `prefill_tok_s` (uncached prompt tokens per prefill second), `decode_tok_s`
  (generated tokens per decode second, per answer), `cache_hit` (prompt tokens read from the
  engine's cache over prompt tokens), `engine_timed` (answers timed by the engine itself:
  llama-server's `timings`, used when they fit within the gateway's own measure; the others are
  split at the first generated text the gateway receives, so their prefill includes the network).
  `runner` (when a runner reported, below): averages over runner-minutes and peaks of `cpu`,
  `iowait`, `steal` (shares 0-1), `mem_private_gb` (processes' own memory), `refaults_s` (file
  pages read again after eviction: the page cache too small), `disk_read_mb_s`, `disk_write_mb_s`,
  `sessions`; `mem_total_gb`; `page_cache_gb` (`avg`, `low`); `free_gb_low`; `psi` (`cpu`,
  `memory`, `io` stall shares, where the kernel has them); `lean`: `slots`, `busy` and `waiting`
  (builds holding and waiting for a slot), `run_s` and `wait_s` (`count`, `p50`, `p95`);
  `spot_notices` (runners the cloud announced it would interrupt).
  Only projects with `public_queue` count here (their tasks, slots, demand and runners): a project
  that keeps its queue private would otherwise show its queue depth by subtraction.
  Answers are built at most every 15 s. Kept in `state_dir/pressure.json`, written every minute
  and at shutdown.
- `GET /v1/stats/credit` and `/stats/`: credit (see [credit.md](credit.md)).

### The latest node release

`GET /v1/node/release`, without authentication: the node release the website's Windows download offers
(`site/src/data/node-release.ts`, on the home page, `/node/` and `/node/windows/`), so a release shows
there within minutes of being published, with no change to the site:

```json
{"version": "v0.7.0", "installerBytes": 390380154, "bundleBytes": 478190808, "versionFreeInstaller": true, "fetched_at": "2026-11-26T12:00:00Z"}
```

With `node_release_url` set, the gateway reads that release from GitHub's public API at start and
every 10 minutes, without a token (GitHub allows 60 such requests an hour per address; an unchanged
release answers 304 to `If-None-Match`, which does not count). It takes a release only when it is one
the block can offer: a tag `vX.Y.Z` (no pre-release suffix), not a draft or a pre-release, and
`AIsAtHome-node-setup.exe` (or `AIsAtHome-node-setup-<tag>.exe`; with both, the same size) and
`aisathome-node-bundle-linux-amd64.tar.xz` fully uploaded. `installerBytes` and `bundleBytes` are
those files' sizes, `versionFreeInstaller` whether the version-free installer is there (the button
then links to `releases/latest/download`), `fetched_at` when GitHub last confirmed it. It follows
`releases/latest`, the release the site's static download link serves (an older release made latest
by hand is answered too).

A failed read (GitHub down or refusing, a release that fails the checks) keeps the last good release
and logs `WARN` "reading the latest node release failed; keeping the last good one" with the release
kept and when it tries again: after 1 minute, doubling with each failure in a row up to an hour, or
later when GitHub says when (`Retry-After`, or the rate limit's reset; honoured up to a day). A new release logs `INFO`
"node release". The answer is cached for 5 minutes (`Cache-Control: public, max-age=300`). Before the
first good read, and without `node_release_url`, it answers 503 (not cached). The release is kept in
memory only: a restart reads it again at once.

The site reads it in the browser, same origin like `/v1/stats`, and shows it only when it is well
formed and not older than the release the site was built with; the Windows button then links that
release's own file by its tag (`releases/download/<tag>/…`), so the version, size and file always
match. Otherwise, and without JavaScript, it shows the build's copy, whose button links
`releases/latest/download`. The JSON-LD's `softwareVersion` is the build's.

### Runner metrics

A science project's runners report their host once a minute to
`POST /v1/projects/{id}/runner-metrics` with the project's key (another project's key: 403; none:
401), as `application/json` of at most 16 KB with exactly these fields (others are refused):
`schema` (1), `runner` (a random id of 16-32 lowercase hex characters the runner draws at boot:
never an instance id, host name or address, which the pattern refuses), `at` (within 10 minutes of
the gateway's clock), `period_s` (10-600), `cpus`, `cpu_busy`, `cpu_iowait`, `cpu_steal` (shares
of the period), `mem_total_gb`, `mem_private_gb`, `mem_private_peak_gb` (anonymous memory),
`page_cache_gb`, `page_cache_low_gb`, `refaults_s`, optional `psi` (`cpu`, `memory`, `io`: the
"some" stall share), `disk_read_mb_s`, `disk_write_mb_s`, `free_gb` (the work volume),
`lean_slots`, `lean_busy`, `lean_busy_peak`, `lean_waiting`, `lean_waiting_peak`, `lean_runs_s`
and `lean_waits_s` (each Lean process and each wait for a build slot that ended in the period,
seconds, at most 300 each), `sessions`, `sessions_peak` (agent sessions running) and
`spot_notice` (the cloud announced the interruption). One report a minute per runner (429
otherwise). The report goes into the hour's and the day's buckets of `GET /v1/stats/pressure`
(`runner`, `runners`); the runner id is kept in memory only, to count runners and to rate-limit,
and is never saved or served. A project without `public_queue` gets `{"counted": false}`. A
metrics collector on each runner sends it.

### Demand for autoscaling (design; not built yet)

The public `demand` counts only projects with `public_queue`. Runner autoscaling needs every
project: the gateway will compute the same number over all projects in the same sample
(`pressureNow`, one more sum: no new walk of the nodes), keep it out of every public answer, and
serve it to the operator only, either as a field of the admin page's status or as a CloudWatch
metric pushed by a timer in the deployment that reads it with an admin-only key. The ASG then targets
ceil(demand / workers per runner). Not built here: the IAM, the metric and the scaling policy
are part of the deployment.

## A chat front end (Open WebUI)

A key with `"forward_users": true` lets a trusted front end speak for its users: the gateway
takes the user from Open WebUI's `X-OpenWebUI-User-Email` / `X-OpenWebUI-User-Id` headers and
applies that user's limits (and, for signed-in accounts, the account's budget). Such a key is
accepted **only** on the private listener `frontend_addr` (e.g. `":8081"`), never on the
public one, so a leaked key can neither pose as other users nor be used as an ordinary key.
Point Open WebUI at `http://<gateway>:8081/v1` over a private network and never publish that
port. The front-end key's `only_operator` also binds the accounts it carries.

With accounts, Open WebUI logs users in from headers set by the gateway's
`/account/auth` (`X-Auth-Email`, `X-Auth-Role`). That is safe only if the reverse proxy
removes any client-sent copies of these headers and Open WebUI is reachable only through
that proxy; the deployment's Caddyfile does both, and its browser tests check it.

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
used today (UTC), the account page's number, from `state_dir/budgets.json`, and its use over all
time from the usage log (`usage_log`): requests (a retry on another node counts again), prompt +
completion tokens and the UTC day of the last request; "-" when there is no log. Run it as the
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

**Every node is untrusted**: a volunteer's, a pilot and a config token's alike,
the maintainer's included; whoever controls a machine can read what it serves. The one exception
is no trust: a requester's **own nodes** (standalone nodes registered to the requesting account,
not its pilots) may get what that requester sends, its personal data included, since that is its
own data on its own machine. Nothing here is confidential; the measures below make keeping or
misusing data harder and more visible ("hardened, not confidential"). Until 2026-11-20 config
tokens could be marked `"trust": "trusted"`; that and the other trust fields (`min_trust` on keys,
accounts and projects, a connector's `trusted_users`, `model_check.include_trusted`) are now
accepted and ignored, each named once in the log at start (`removed config field ignored`). A key,
account or project that had `min_trust: "trusted"` is served by every node now; `only_operator`
and an account's "only my own nodes" still restrict where requests go.

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

**Linking a node without a token** (`gateway/nodelink.go`; RFC 8628 style). A node
with no token asks `POST /v1/node/link` (no authentication; JSON: what it reports about itself,
`name`, `gpu`, `vram_mib`, `os`, `agent`, all optional and unchecked) and gets a `device_code` (its
secret for polling), a `user_code` such as `KXQT-4821`, `verification_uri` (`<public_url>/link`),
`verification_uri_complete` (with `?code=`), `expires_in` 600 and `interval` 5. The person opens the
link on any device, signs in (the sign-in comes back to the link), and the page shows the code, the
node's details, what approving does, the warning to approve only a code a machine of one's own shows,
and the operator terms line of "Add a node". **Approve** creates the node exactly as "Add a node" does
(active accounts only, `nodes_closed`, `max_nodes_per_account`, `max_nodes`; the operator terms are
accepted unless the account chose science only), with `"via": "link"` on the node and
`"via": "node-linked"` on an acceptance it records; **Deny** refuses it. The node polls
`POST /v1/node/link/poll {"device_code"}`: `pending` (with `interval`), `slow_down` (polled too fast:
the interval grows by 5 s), `approved` with `token` and `node_id` (once; the link is then gone),
`denied`, or `expired` (also for an unknown or used code). Codes live only in memory, as HMACs under a
key made at start, for 10 minutes and one use; a restart drops pending links. The token exists in clear
only between the approval and the node's next poll; a node approved but never picked up (within 10
minutes, and at least a minute after the approval) is revoked. Limits, in memory only: 20 codes per
address in 10 minutes and 60 an hour, 300 an hour over the gateway, 500 pending at once; 10 wrong codes
per account an hour and 30 per address; only an active account looks a code up (a waitlisted one is
told to come back). Caddy must send `<domain>/link` to the gateway (the deployment's Caddyfile does).
Against RFC 8628's remote phishing (someone sends a person their code, and the person approves it), a
linked node is **not the account's own node until its owner confirms it once** on the account page
("This is my machine", recorded as `"confirmed"` with its time). Until then it gets
none of the account's personal data (such a request is refused as with no own node), and the account's
other requests reach it only as they would a stranger's node. The account page and the approval page
say so. Chat stays behind the operator terms exactly as for any node.

**Nodes without an account** (`gateway/anon.go`). `POST /v1/node/register`
(`{"accept_terms": true, "agent"}`, no authentication; the installer calls it after its speed test,
`install.sh --anonymous`, `node.sh`, the Windows setup page's "Start without an account", or
`aisathome-node register -accept-terms`) makes an unclaimed account `ax:<32 hex>` with one node
`a-<8 hex>` and answers its token `nt-a-<8 hex>.<secret>` once (stored hashed, as any node token). Hard
rules, not settings: such a node serves **science only** (no chat, API or agent request of a person,
whatever `chat_rules_off` or a key's consent say; its account never counts as having accepted the
operator terms); it gets **no science until the model check admitted it**: its first work is probe
prompts only, from the **admission set** (`model_check.admit_references`, other prompts than the secret
set, so registering never shows anyone a secret-set prompt), every `model_check.admit_seconds` (default
20) on average, and it is admitted after 3 passed checks (each 4 probe prompts, the same KL and flip limits as the check), normally within minutes;
one failed check before then holds it (rule `model-check`), and after admission a fail result holds it,
both whatever `model_check.withhold` says. It announces exactly one model, and admission holds only for the model its checks were of (back with another model, its checks start afresh). Without the model check, registering is refused (503); a node
whose model has no reference is never admitted. It earns **no credit** (its usage records carry none,
and nothing goes to the ledger), so nothing moves when it is linked; it is listed nowhere public. Agent
calls of a project with `pilot_probation_ok` may use it during probation, as pilots. Limits: 3
registrations per address a day and 30 an hour over the gateway (in memory only; a restart forgets
them), a pool of `max_anon_nodes` (default 25) apart from `max_nodes`, `anon_closed` and `nodes_closed`
stop new ones, at most 2 connected per address (account nodes keep their 10). Retention: one that never
connected is deleted 24 hours after it registered, one not seen for 90 days is deleted, and a held one 24
hours after its hold (its last
connection is written at most once an hour). `GET /v1/node/self` (its own token) tells the node's local
page its state: `checking` (with `checks_passed` of `checks_needed`), `admitted`, `held` (no reason
given), `offline`, `no_check`. **Linking**: `aisathome-node link` on such a node sends its token with
`/v1/node/link`; the approval page says it ran without an account; approving creates a new node on the
account as any link does (unconfirmed until "This is my machine"), and once the node picked up its new
token the `ax:` account is deleted. A held node cannot be linked.

Routing and protections:

| What | Rule |
|---|---|
| Personal data (email addresses, card numbers, IBANs, Norwegian ID numbers; `gateway/privacy.go`) | the requester's own nodes only. If none of them is online it is **refused at once**, 400 with `"code": "personal_data"` and the users' rule: "This looks like personal data (an email address). Personal data isn't allowed in chats on AIs@home: answers are computed on volunteers' machines. Remove it and try again. …", naming the kind found, never the value. An own node that is online but busy is waited for as any node; in the two minutes after a gateway start a request of an account with registered nodes waits for them to reconnect |
| Requests with `tools` (agents) | the requester's own nodes, unless the API key was created with "allow volunteer nodes for agents" (`agents` on the key; `volunteer_agents` for config keys) **and** the node's account is past probation. The chat (Open WebUI) never carries consent |
| Probation | `accounts.node_probation` (Go duration, default `0`: none) from the account's first node token; revoking and recreating does not reset it |
| Share cap | no operator gets more than about a third of the conversations placed in the last 10 minutes while other nodes are free (soft: if only its nodes are free, they serve). A conversation counts once per window, however many turns go to the node that holds it |
| Per address | at most 10 connected account nodes per client address (IPv6 per /64; the rightmost `X-Forwarded-For` from loopback or a `trusted_proxies` peer, i.e. Caddy) |
| Own nodes | per account: "my own nodes first" (default), "only my own nodes" or "any node", for its chat and API requests; "only my own nodes" applies on top of a front-end key's `only_operator` (if they differ, no node serves) |
| Cut-off | revoking a token, `accounts ban` or `accounts node-revoke` drops the node within ~2 s (no restart); requests in flight on it fail over to another node if no output was sent yet |
| Guards | answers from every node but the requester's own (pilots included): tool-call arguments are held back until the call is complete and their decoded values (not the JSON text, whose `\n` escapes made words such as `ncat` out of nothing) are checked for download-and-execute (`curl … \| sh`; piped into python, node, ruby, perl or php only when the interpreter gets no program from its arguments: `\| python3`, `\| python3 -`, `/dev/stdin`, an interactive flag, `python -m code`, an inline program that itself runs code such as `exec(sys.stdin.read())`, a substitution where the program goes, or a later stage of the pipeline that is a shell, so `curl … \| python3 -c '…'` and `curl … \| python3 parse.py` read the download as data and pass; `python3 <(curl …)` and `node -e "$(curl …)"` count; a shell counts whatever follows it), credential paths (`~/.ssh`, `~/.aws`, `.env`, `id_rsa`; excused if the request names them), encoded commands and file uploads (`curl -F @…`, `scp`, netcat fed from a file, a here-document or a pipe: read only in shell commands and shell scripts, never in other files, so Lean's `have nc : a < b` passes); web addresses that appear nowhere in the request and are not a known source are only flagged (`guard_known_addresses`: agent clients compact long conversations into a summary, so an address the agent read from its own workspace hours before, such as Mathlib's git address in a lakefile, is gone from the request; a known source must be one plain shell word: a plain host name, and after it no quotes but closing ones, no backslashes, `$`, backticks or braces, which would join more text into the host, and at most 1,024 bytes; a path entry matches only a plain path, without `.` or `..` segments or percent-escapes). The stream itself must be well-formed: tool-call pieces after the calls were completed (`tool-call-after-finish`), keys that differ from the stream's only in case, and a chunk the guard cannot read exactly as a client would (`malformed-chunk`: not one JSON object, a key given twice, tool calls of another shape than a list of pieces with a whole-number index from 0 to 255 and string names and arguments, or tool calls anywhere but in a choice's `delta`) stop the answer as well; numbers are read as written, so a valid number beyond a float64's range is no reason to skip a chunk. The guard reads each chunk as the node wrote it, before the gateway re-encodes it (to name the model or drop logprobs). The legacy `function_call` is held and checked as tool calls are, and released in its own shape (also in a non-streaming answer, as `message.function_call`). Non-streaming answers are checked again as assembled. **On an account's node (`n-`) these rules only flag**, unless the request comes from a project's key or a key with `"unattended": true` (no person approves its tool calls; "Unattended clients" above), which keep the stop and hold on every node: the call goes out, nothing is retried, the node is not held, and the event is logged and kept with its evidence as `flagged` (usage log `guard: <rule>`, outcome `ok`). The guard cannot tell an honest model reading a credential file from an attack, and stopping such answers could hold every node of the network on one agent's retries; the defence is the client's own command approval (site: "API and opencode"). The stream rules below still stop on every node. On a node without an account, a config token's node or a pilot, a flagged call is never released, and the request is not sent to another node (no node may answer it unchecked): it fails with 502, the node is **held** (no requests until `accounts node-clear`) with its rule, a reason in the guard's own fixed words naming the pattern (e.g. "netcat sending a file to a host and port"), the request id and the **evidence** (below), and the event is logged without content (usage log `outcome: blocked`, `guard: <rule>`; the log line has rule, reason, node and request id, never what matched). An **official installer** (`guard_official_installers`: every word of the download command is an `https://` address of the list or a flag that only quietens, fails, follows redirects or asks for https, such as rustup's `curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \| sh`) is stopped the same way, but the node is not held: honest models suggest these installers, and a hold for one took half the network's capacity for hours (2026-11-19). It is still stopped because the guard reads one command, not what the installer does with its arguments and environment, and a node can read the list too; so the list decides only whether a node is held, never what reaches a client. A second address, a proxy, a config file, `http://`, another path, a second command, an expansion, a redirection or a quote inside the substitution, or another blocking call in the same answer holds as before. Unknown addresses are only flagged: the answer reaches the client, nothing is retried and the node is not held; the usage log records `guard: unknown-address` (in a tool call) or `guard: unknown-address-in-text` with outcome `ok`, and nothing of the address. Honest coding agents write such addresses all the time (package indexes, documentation, git clones), and the guard cannot tell them from one a node injected; a download piped into a shell is stopped whatever its address. A **package install from an unknown index** blocks and holds like the other rules (`install-source`, "a package install from an unknown index"): the index's build and install scripts would run on the client. It reads shell commands and shell scripts as above and, for an install only, every address with `://` in it: `pip`/`pip3`/`python -m pip`/`uv pip`/`pipx` install, download or wheel (`--index-url`, `-i`, `--extra-index-url`, `-f`/`--find-links`, a direct `https://` or `git+https://` requirement, `-r https://…`), `pip config set`, `uv add`/`sync`/`lock`/`tool install` and the index flags of `uv run`/`uvx`, `npm`/`pnpm`/`yarn`/`bun` installs (`--registry`, a URL or tarball spec) and `npm config set registry …`, `npx --registry`, `cargo install`/`add` (`--index`, `--git`), `GOPROXY=… go …` and `go env -w GOPROXY=…`, `gem install --source`/`gem sources --add`, `conda`/`mamba -c https://…`, the environment variables these tools read (`PIP_INDEX_URL`, `UV_INDEX_URL`, `npm_config_registry`, ...; assigned anywhere in a command, such as `make PIP_INDEX_URL=…`, and in `echo … >> file` or `echo … | tee`, but not printed or in a comment), and a package manager's own source file written by a write or edit tool (`.npmrc`, `.yarnrc.yml`, `pip.conf`, `uv.toml`, `.condarc`, `.gemrc`, `.cargo/config.toml`, `requirements*.txt`, `yarn.lock`, `pnpm-lock.yaml`; read raw and with JSON, TOML and YAML escapes such as `\/` and `\u002f` decoded; a whole `package.json` or `package-lock.json` that is valid JSON is decoded and read by its structure: every address but the project's own links at the top level of `package.json` (`homepage`, `repository`, `bugs`, `funding`, `description`, `author`, ...) and a package's metadata in a lock file (`funding`, `license`, ...), so dependency maps, `overrides`, `resolutions`, `resolved` and `version` always count; in an edit of these files, which shows no level, every address counts). An index is known if the request names its host (as a whole word: `github.com` does not name `hub.com`; punctuation such as curly quotes ends a word, an IPv6 address matches in any of its forms, and a host longer than DNS allows, 253 bytes, is never named; this holds for flagged addresses too), it is a `guard_known_addresses` source, a local or private address, or a registry the tools use by default (`pypi.org`, `files.pythonhosted.org`, `download.pytorch.org`, `registry.npmjs.org`, `registry.yarnpkg.com`, `proxy.golang.org`, `rubygems.org`, `crates.io`, `conda.anaconda.org`, `repo.anaconda.com`); such an install is at most flagged as an unknown address. A plain `pip install requests`, an address in an `echo`, a comment or a README, and arguments a run command passes to its package (`npx tool https://…`) are no install source. These are tripwires for crude attacks, not answer checking |
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
aisathome-gateway accounts nodes anon close|open  # stop or allow new nodes without an account (anon_closed)
aisathome-gateway accounts nodes anon limit 25    # the pool of nodes without an account (max_anon_nodes, default 25; "default" unsets it)
aisathome-gateway accounts node-revoke n-1a2b3c4d # delete one node token (a node without an account's too: a-…)
aisathome-gateway accounts node-clear n-1a2b3c4d  # let a held node serve again
aisathome-gateway accounts guard-log [n-1a2b3c4d] # recent stops, holds and flags with what matched (14 days)
aisathome-gateway accounts node-checks [7] [n-1a2b3c4d] # answer checks and signals per node over the last days
aisathome-gateway accounts routing own-only       # emergency: every request, waiting ones too, only to its sender's own nodes (no stranger's node serves anything, science neither); "any" to undo
aisathome-gateway accounts ban gh:12345           # also deletes the account's node tokens
```

**Evidence.** Every stop, hold and flag keeps what matched: a fragment of the **node's answer** (the tool call's decoded arguments with its name, the answer's text, or the stream chunk; never the request), at most 300 bytes around the match, with secrets in the formats the request redaction knows replaced by their kind (`ghp_[redacted]`, also one that only starts or ends inside the fragment), line breaks as spaces and control or format characters (escape sequences, bidirectional overrides, invalid UTF-8) as `\u` escapes; where it matched (`tool call <name>`, `answer text`, `stream`), the host of the address it names, the request id, the node and the time. A hold keeps it in its record in the accounts file; the last 200 events of every kind (`held`, `stopped` for an official installer, `flagged`) are kept in `state_dir/guard-log.json` (in memory without a `state_dir`). Both are **kept 14 days** (GDPR, data minimisation: long enough to look at a hold after a weekend): older log events are dropped on reading and saving, and a hold's evidence is removed from the accounts file (the hold stays until cleared) and never shown after that; a guard log that does not parse is deleted, not set aside. Admins only: the ops page's "Held nodes" card, `accounts nodes` and `accounts guard-log [node]` (as of the gateway's last save, every 5 s; needs `-config` with the `state_dir`, not the accounts file). The fragment can repeat what the node copied from the conversation into its answer.

A hold shows on `/account/ops` (card "Held nodes": node, rule and reason, what matched, since when, connected or not) and in `accounts nodes`, and raises a `node-held` health warning with its rule and time until it is cleared. An admin can clear it on the page too ("Clear hold": the same change as `node-clear`, a same-origin form post, and only the hold the page showed, so a newer one stays); the gateway logs that clear with node, rule and the admin's pseudonym. A guard's hold is written to the accounts file. If that write fails, the node stays held in
memory and the gateway retries every few seconds, logging `HOLD NOT RECORDED` each minute
until it succeeds; `node-clear` works once the hold is in the file. A restart before then
forgets the hold. More than 1024 unrecorded holds hold every node until a restart.

Beyond `max_nodes` the page says "the network is full for now" (there is no waitlist for
nodes).

### Answer checks and node signals

The gateway pays per token only for tokens it can show came from the model (node verification). Every answer that can earn credit (`ok`, `cancelled`, `preempted`),
from every node, pilots alike, is checked against three rules no honest node
breaks, all from what the gateway itself counted and timed (`gateway/verify.go`):

| Rule | Fails when | Why no honest node fails it |
|---|---|---|
| `speed` | output tokens > `speed_burst` + `speed_rate` × the answer's duration (from sending the request to the node to its end) | Set both from your own nodes' measured speeds per speculative-decoding setting, with a margin for faster cards and speculation (the burst covers short answers the gateway counts in one go). A node that pads without running the model sends thousands of tokens a second. The defaults are deliberately generous; keep the values you deploy private, since an exact ceiling tells a cheater how fast it may pad |
| `reasoning-budget` | reasoning tokens > the budget + the message forced at it (+ 32 tokens and 2% for counting the text again) | The budget is the request's `thinking_budget_tokens` (or `reasoning_budget_tokens`), else the nodes' default (`node_reasoning_budget`, the installer's 8,192, with its message). llama-server stops thinking there itself |
| `max-tokens` | output tokens > the request's `max_tokens` (+ 32 tokens and 2%) | llama-server never writes past it. Output past it already earned nothing; now the whole answer is in question |

Without `tokenizer_file` only `speed` runs, on the node's clamped count. A failed answer's usage
record lists the rules (`checks`), the gateway logs a warning (`answer failed a check`, with node,
rules, tokens and duration, no content), and the operations page counts it per node. What happens
next is the switch `verify.mode`:

- `observe` (the default): nothing else; credit is unchanged. Run it on our own nodes first and
  read `accounts node-checks` before enforcing.
- `enforce`: the answer's credit is voided: it earns nothing, in the ledger and in a pilot's record,
  and the record's `voided` says what it would have earned.
- `off`: no checks.

```json
"verify": {"mode": "observe", "speed_rate": <tokens per second>, "speed_burst": <tokens>,
           "node_reasoning_budget": 8192, "node_budget_message": "(Thinking budget reached; answering now.)"}
```

All keys are optional (the reasoning values shown are the defaults, the installer's;
`node_reasoning_budget: -1` checks only budgets a request sets). The public wording is "spot-checked".

**Signals** (`gateway/signals.go`, `discard.go`): each usage record also carries `task` (the
`X-Aisathome-Task` a project's request named, a well-formed task name) and `signals`: `tps` (output tokens per second
of the gateway's time), `reasoning` (reasoning tokens), `finish` (the node's `finish_reason`; `length`
is an answer cut at `max_tokens`), `tool_calls` and `bad_calls` (no name, or arguments that are not a
JSON object), and for generated text of 400 bytes or more `text` (`gz`, compressed over raw size;
`rep`, the share of repeated word 6-grams; `odd`, the share of unusual characters) and `degenerate`
(beyond the tails of honest agent text measured on our own prove2me transcripts). A **discarded** answer, one
whose requester's next prompt starts with the prompt it answered but not with the answer (a retry or
regenerate), is named in the record of the request that discarded it: `discarded: [{"req_id", "node"}]`. These are numbers only,
computed in memory; no text is kept. None of them changes credit: they are compared per node with the rates
of the maintainer's own nodes, and decide what a person, and later the replay sampler, looks at. Honest rates of
discarded answers and `length` endings are still to be measured on our own nodes.

The operations page (`/account/ops`, card "Answer checks") shows per node since the gateway started:
answers, failed checks by rule, credit voided, answers at `max_tokens`, bad tool calls, discarded and
degenerate answers. `accounts node-checks [days] [node]` gives the same from the usage log over days
(default 7), with credit and output tok/s (median, 99th percentile, max).

The card "Funnel" shows, for today and the 7 UTC days before it, totals only: from `site_stats_file`
(above) the site's visitors, views of the node pages and install starts (distinct clients fetching
`/node.sh`, and the curl and wget share of all its fetches); and the gateway's own daily counters: nodes
registered without an account, nodes added to an account (account page or link), of those by an approved
link, nodes without an account linked to an account, and distinct nodes connected for the first time. The
nodes of the admin accounts (`accounts.admins`) are shown apart as the project's own and left out of the
one-line summary (which puts the site's numbers and the gateway's side by side, with no rate between them:
first connections come from every install path, not only the install script); nodes without an account cannot be told apart, and nodes on the config's
`node_tokens` are not counted. The counters live in the accounts file (`funnel`: numbers per day, kept
400 days, never a node or account), each counted in the write that records its event; a node record
keeps one flag, `connected`, so a reconnect is not a first connection. First connections count only for
nodes created after the counters started (`funnel.since`, shown on the card). Downloads of the Windows
installer and the Linux bundle are counted by GitHub on the release page; the card links there.

## Chat rules: operator terms

On top of the rules above, a node serves people's requests (chat, agents, API calls)
only once its account has accepted the operator terms
(`gateway/chatrules.go`). Where a node runs makes no difference. Science (a
project's key) carries no personal data and is not affected. The requester's own nodes (not pilots)
are outside the rule.

- **Accepting** (2026-11-20: on by default, with an opt-out). **Creating a node** on the
  account page accepts the operator terms (section "Requests your node answers", approved on
  2026-11-19): the "Add a node" form says "By creating this node and running it you accept the
  operator terms (§6)" with a link, the new token's panel repeats it, and the creation records
  `operator_terms` with `via: "node-created"`. There is no pre-ticked box: the action is the
  acceptance. An account that switched to science only stays so when it adds a node. The account's
  button accepts too (`via: "account"`): accounts whose nodes came before 2026-11-20 get a banner,
  "Accept the operator terms to let your nodes answer chats", and their nodes stay science-only until
  they press it; nothing is accepted on their behalf. For a connector's user, the connector records
  it (`POST /v1/connectors/{id}/users/{user}/operator-terms`, below; BOINC: the ENROLL consent). A config node token counts as
  accepted if it names the version (`"operator_terms": "<version>"`). The version and
  time are recorded in the accounts file (`operator_terms`). Without an acceptance the node serves
  science only.
- **Versions.** The current version is `2026-11-19`. A new version needs a new acceptance. A version
  starting with `draft-` is accepted by nobody: while one is current, no node but the requester's
  own serves people's requests.
- **Science only on my nodes** (the account page's switch, when accepted) withdraws the acceptance
  for all the account's nodes and pilots: they do science only, and chat stops within the call; "Let
  my nodes answer chats" accepts again. The withdrawal is recorded with its time; a connector's record of an acceptance from before it (or
  from before the account's own acceptance) changes nothing.

When the rule leaves no node free, a request waits for one as any request does and is never sent
elsewhere; after the queue time it gets a 503 that says why (`operator_terms`). The account page
shows, per connected node, whether it serves chat and why not. The sign-in page states the age
limit (18+) and the rule for people who chat (no personal data in chats); the account page
repeats the rule.

| Field | Meaning |
|---|---|
| `chat_rules_off` | turns the rule off. On by default; off only for a gateway whose nodes are all the operator's own (as `examples/gateway.json`, one local node), and in tests |
| `trusted_proxies` | the reverse proxies whose `X-Forwarded-For` the gateway believes (addresses or prefixes), besides loopback, which it always believes. The client address counts for the per-address node limit, email sign-in's limits and connectors' `allow_from`; from any other peer the header is ignored (once, from a private address, with a warning in the log). Behind Caddy in a container network, list that network (`"10.89.0.0/24"`) |

## Connectors and pilots

A **connector** is an outside system that brings machines in bounded serving sessions, **pilots**.
The BOINC project's bridge (`aisathome-bridge`, `boinc/README.md`) is the first: a BOINC work unit is one pilot, a run of
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
  "chat_headroom": 0,
  "sessions_per_slot": 2,
  "unsent_buffer": 2,
  "max_credit_per_pilot": 150000,
  "max_credit_per_account_day": 1800000,
  "accounts": {
    "name": "BOINC",
    "attach_url": "https://boinc.aisathome.org/aisathome/",
    "attach_command": "boinccmd --project_attach {url} {key}",
    "key_url": "https://boinc.aisathome.org/aisathome/weak_auth.php",
    "address_domain": "nomail.aisathome.org"
  }
}]
```

| Field | What |
|---|---|
| `id` | Lower-case letters, digits and dashes: the `{id}` in its endpoints. |
| `key_sha256` | SHA-256 of its keys (make one with `aisathome-gateway project-key <file>`). A connector key opens only that connector's endpoints: no API, project or account call. It must differ from every project key. |
| `allow_from` | Optional: addresses or prefixes its calls must come from (behind a reverse proxy on loopback or in `trusted_proxies`, the last `X-Forwarded-For` hop). Others get 403. Also restrict the path at the reverse proxy. |
| `max_pilots` | Its pilots that have not ended, at most (0: none can be created). |
| `mint_per_hour` | Pilots created per hour, at most (default 120). |
| `max_deadline_hours` | A pilot's deadline at most this far ahead (default 336). |
| `pilots_chat_enabled` | Off by default: its pilots serve **science only**. See below. |
| `no_chat_users` | Its users (connector ids) whose pilots serve science only, whatever else says: Science United's account, whose hosts cannot reach a person's switch. |
| `chat_headroom` | Pilots to keep for people's requests in the demand number, counted only while `pilots_chat_enabled`. |
| `sessions_per_slot` | Science sessions one GPU slot keeps busy (default 2: a session alternates model calls with its own tools). |
| `unsent_buffer` | Created pilots no machine has taken yet, at most (default 2). |
| `max_credit_per_pilot`, `max_credit_per_account_day` | Credit caps (default 150,000 and 1,800,000; `-1`: none). See `docs/credit.md`. |
| `accounts` | Optional: people link their account to the connector's users from the account page ("Linked accounts" below). `name` as people know the connector; `attach_url` (https) what machines attach to; `attach_command` the one-line attach, `{url}` and `{key}` replaced; `key_url` (https) where people find the account key to claim with (BOINC: `weak_auth.php`, which shows both keys); `address_domain` the domain of the pseudonymous addresses (mail to it should be dropped). |

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
| `POST /v1/connectors/{id}/users/{user}/operator-terms` `{"version", "accepted_at"}` | The connector's user accepted the operator terms (BOINC: the bridge, when BOINC records the user's ENROLL consent to `terms_of_use.txt`, which holds the operator terms). `version` the terms version accepted, `accepted_at` when (RFC 3339; stored as sent; at most a minute ahead of the gateway's clock, more is refused; a withdrawal is recorded after any acceptance it withdraws and never moves back, so a retried record never undoes it). An optional `country` is accepted and ignored. Recorded on the user's account, the linked one or a new unclaimed one; a record older than the account's acceptance or withdrawal changes nothing. Answer: `{"recorded", "current", "current_version", "account_kind"}`: `recorded` whether this call changed the record, `current` whether the account's pilots may serve chat now. The user's connected pilots follow within the call |
| `GET /v1/connectors/{id}/pilots/{pid}/credit` | `{"pilot", "state", "final", "final_at", "credit", "withheld"}`: `credit` only once `final`; until then `final_at` says when (null while the pilot has not ended) |
| `GET /v1/connectors/{id}/requests` | People's pending requests, oldest first (at most 50): `{"requests": [{"id", "kind", "created_at", …}]}`, see "Linked accounts" |
| `POST /v1/connectors/{id}/requests/{rid}` | A request's result: `{"ok": true, "user", "key"}` (`create_link`: the user it made; `claim`: the user the account key belongs to; `key` its attach key, `<user>_<secret>`), `{"ok": true}` (`set_email`) or `{"ok": false, "error": "<short reason>"}` (stored without anything that looks like an address or a key). The same answer again is accepted; another one after the first, also another key for the same user, is 409 |

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
no sign-in, keys or node tokens and does not count against `max_active`. Its person claims it from
the account page ("Linked accounts" below); the operator can also link by hand:

```bash
aisathome-gateway accounts link gh:12345 boinc 678      # this account is BOINC user 678
aisathome-gateway accounts unlink gh:12345 boinc 678
aisathome-gateway accounts chat-on-pilots gh:12345 off  # this account's pilots: science only
```

An account's own API key reads and sets the switch too: `GET` and `PATCH /v1/account/settings`
with `{"chat_on_pilots": false}`; the account page has it as "Science only on my pilots".

### Linked accounts

With `accounts` set on a connector, an active account's page offers, per connector:

- **Run on BOINC** (`create_link`): the connector makes a user for the person and the account is
  linked to it. The person picks a display name (default `aisathome-<6 hex>`: BOINC publishes
  names), an address and optionally a password (8+ characters, for BOINC Manager and BOINC's site;
  it changes the attach key, so it is chosen now). The address is **pseudonymous** by default,
  `b<20 hex>@<address_domain>`, derived from the account with the session secret and never stored,
  so the page can show it as the Manager login; or the person's own (needed for one cross-project
  id and Gridcoin).
- **Claim a BOINC account** (`claim`): the person pastes their **full account key** (BOINC's
  authenticator, 32 hex digits, at the top of BOINC's "Account keys" page, `weak_auth.php`). Not the
  weak account key: BOINC means that one to be shared with whoever computes for the owner, so it
  proves nothing. The connector finds the user by the account key (BOINC: `am_get_info.php`) and
  answers its id and attach (weak) key. Claiming a user linked to the same account again refreshes
  the stored attach key (BOINC changes the weak key with the password or the address).
- A person with a pseudonymous user who wants their own address (one cross-project id, Gridcoin)
  changes it on the connector's site, logged in with the shown login and their password. The password
  stays optional; the form says to choose one to log in to the BOINC site later. A
  claimed user's card says the person can log in there with their account key (stock BOINC accepts it
  in place of the password). (Stock
  BOINC's `am_set_info.php` is disabled, so the gateway has no `set_email` form; the request kind
  stays in the schema, and one left from before is still listed to the connector.)
- The linked users with their **attach command** (`attach_command` with the stored key) and, for a
  pseudonymous user made here, the Manager login; the **chat switch**; and the account's **pilots**
  (the newest 20: state, host, start, serve time, answers, credit and whether it is final).

A request waits for the connector (`GET …/requests`), which carries it out and answers (`POST
…/requests/{rid}`). What the connector sees per kind: `create_link` the `email`, `user_name`,
`own_email` and `passwd_hash` (MD5 hex of the password followed by the lower-case address, BOINC's
form; none: pick a random password); `claim` the `account_key` to check; `set_email` (no longer
made) the `user` and `email`. Never which account asked. An account has one open request per connector and
at most 10 a day; a connector at most 200 pending. Unanswered requests fail after 24 hours;
finished ones are deleted after 30 days. The page says while a request waits and why one failed
(the connector's text).

When the connector says yes, the user is linked (`create_link` and `claim` alike):

- only to an **active or waitlisted** account (a banned one gets no link, also if it was banned while
  the request waited). A waitlisted account may claim, run on BOINC, see its
  pilots and use the chat switch, and its pilots serve under it as any account's do; the waitlist
  still keeps it from the chat, the API and keys until it is let in;
- a user linked to the same account keeps its link with the new key;
- a user held by an **unclaimed** account is **merged**: all its links move, the account's probation
  starts at the earlier first-node time, its chat switch is off if either was off, its operator
  terms carry over (the newer acceptance and the later withdrawal, an acceptance older than a
  withdrawal dropped), the pilot
  records and their daily credit move to it, and the unclaimed account is deleted. That user's
  connected pilots are dropped and reconnect within seconds as the account's nodes (the same
  records continue). A bind that came before the answer (the user's first pilot) is merged the
  same way;
- a user linked to **any other account** is not taken: the request fails ("linked to another
  AIs@home account"); the operator unlinks first if that is wrong.

Linking spans the accounts file and the science database; a crash between the two finishes on
the next sweep (a minute). **Privacy:** the gateway never stores a connector user's address. A
person's own address and the password hash live only in the pending request and are blanked once
the connector answers (or the request fails); the account key of a claim is replaced by the
answer's attach key then, which leaves the request when it is done or failed, and the link keeps
the attach key (it can attach machines to that user and do nothing else). After each such write the
WAL is checkpointed and truncated, so it keeps no older copy. The records are in `science.db` (`connector_requests`, migration 3), backed up with it; the
database overwrites blanked values (`secure_delete`), and its daily copies blank the values of
pending requests and mark them failed (a restored one could not be carried out); verified ones keep
their answer's attach key, so a restored copy still finishes them. A WAL truncation that a reader
blocks is retried at the next sweep. Hardened, not airtight: the data volume's snapshots can still hold a request
that was open at that moment. The pseudonymous login shown on the page is derived from the session
secret: rotating it changes what the page shows, not the login the connector holds.

### What a pilot may serve

A bound pilot is an **account node** under every rule in "Volunteer
nodes" above: no personal data, the share cap per operator (the account's
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
| `pilots_chat_enabled` is false (the default) | **people's prompts reach strangers' machines only after anti-retention measures and the privacy notice and GDPR review are in place** (they are done before outside hosts join). Until the operator turns this on at launch, only science (which carries no user data) reaches pilots, the maintainer's own test pilots too (`trusted_users`, which let those serve chat, is gone) |

And it serves people's requests only under the chat rules above: its account accepted the operator
terms. Changes to the switch, `no_chat_users` or `pilots_chat_enabled` (on reload) reach connected pilots within
seconds. Nothing here makes a pilot confidential: whoever runs the machine can read what it serves
("hardened, not confidential"). Public task views name a pilot's class as `pilot`.

### Pilots wanted

`GET …/demand` computes:

```
science sessions = Σ over projects with agent_ok:
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
  30/day (rightmost `X-Forwarded-For` when the connection comes from loopback or a
  `trusted_proxies` peer; IPv6 per /64); 300/day in all. Wrong codes: 5 end an attempt; 20 per
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
