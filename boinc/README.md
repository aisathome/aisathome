# BOINC pilot app

The node agent as a BOINC application. A volunteer attaches the AIs@home BOINC project in the
BOINC client they already run; the client then runs **pilots**: tasks that serve model calls for
the gateway for up to 4 hours and then end. Status: first version, not yet published on the
project.

## How a pilot runs

BOINC's stock `wrapper` is the app's main program. It reads `job.xml` and runs one task,
`aisathome-node pilot` (package `node/pilot`), which:

- unpacks the node's Linux bundle (llama-server and its CUDA libraries) once into the project
  folder, in Go (no `tar` or `xz` program needed), and starts llama-server itself with the node's
  standard settings for the big tier, on a free local port and on the GPU the client assigned
  (`CUDA_VISIBLE_DEVICES=$GPU_DEVICE_NUM`);
- connects to the gateway over the node WebSocket with the credential the work unit carries;
- ends after 4 hours (it pauses, lets requests in flight finish, then exits 0), after 30 minutes
  with no request routed to it (exit 0), or when the engine keeps failing (exit 1);
- on SIGTERM (the client quits or preempts the task; the wrapper kills it 10 s later) pauses,
  gives requests in flight 3 s, closes the connection and stops the engine, within 8 s;
- closes the connection with its reason (`pilot: time`, `idle`, `sigterm` or `engine`) and writes
  a small summary as the task's output file;
- reports a checkpoint only when it has been idle for 5 minutes past one of the client's
  60-minute rotation boundaries, so a busy pilot keeps the GPU and an idle one lets the client
  run the volunteer's other projects;
- reports the fraction done as elapsed time over 4 hours.

**It never opens BOINC's `init_data.xml`**, which holds the volunteer's account key, and it logs
neither that key nor the credential. A test runs the pilot in a task folder with an
`init_data.xml` and checks that the key reaches neither the gateway nor the log nor the output.

**The credential is a file, and its name stays private.** The wrapper prints the task's command
line to stderr, and BOINC shows each result's stderr on the project's public result page. The
work unit's command line carries only the gateway URL; the credential comes as a per-work-unit
input file with an unguessable name. Input files are plain HTTP downloads at a URL derived from
that name, so the name must not appear either: `job.xml` passes the logical name (`credential`)
rather than `boinc_resolve(credential)`, which the wrapper would print resolved, and the pilot
follows BOINC's soft-link file itself and names no file in its errors. The pilot's own log
(stderr) holds no secrets for the same reason; llama-server's output goes to `engine.log` in the
task folder, which is not uploaded.

**The task's environment is only `job.xml`'s `<setenv>` entries.** On Linux the wrapper starts
the task with those variables instead of its own environment, so `job.xml` sets `PATH` next to
`CUDA_VISIBLE_DEVICES`, and the pilot runs no helper program to unpack the bundle.

## Files

| File | What |
|---|---|
| `job.xml` | the wrapper's job file (part of the app version) |
| `templates/pilot_in.xml` | input template: file 0 the model, a remote sticky file from its pinned Hugging Face URL (`create_work --remote_file URL NBYTES MD5`, open name `model.gguf`); file 1 the credential (open name `credential`); one result, no quorum, 2-day deadline |
| `templates/pilot_out.xml` | output template: the summary (`summary.json`, at most 64 KB) |
| `app-version.sh` | lays out one app version for a project's `apps/` folder, unsigned |
| `bridge/` | the bridge between the project and a gateway (below) |

A work unit's command line is `--passthrough_child --gateway wss://<gateway>/v1/node/connect`.
The command line goes to the wrapper, which refuses options it does not know; it hands what
follows `--passthrough_child` to the task (`<append_cmdline_args/>` in `job.xml`).

## Making an app version

```sh
boinc/app-version.sh --version 1 --plan-class cuda_big --beta \
  --wrapper <BOINC wrapper for x86_64-pc-linux-gnu> \
  --node <aisathome-node built for linux/amd64> \
  --bundle aisathome-node-bundle-linux-amd64.tar.xz \
  --out <dir>
```

It writes `<dir>/pilot/1/x86_64-pc-linux-gnu__cuda_big/` with the four files, each named with the
start of its SHA-256 (a physical name in BOINC never changes content), and `version.xml`
(`is_wrapper`, `needs_network`, optionally `beta`). Every file must then be signed with the
project's code-signing key on an offline machine (BOINC's `sign_executable`, a `.sig` beside each
file); the key never goes on a server. The project's `update_versions` refuses unsigned files
when it has no private key.

The bundle runs on x86_64 Linux with an NVIDIA GPU of compute capability 8.6, 8.9 or 12.0 (RTX 30,
40 and 50 series) and a driver new enough for its CUDA 12 build; the project's plan class must
match (`scripts/build-llama.sh`). The big tier needs about 22 GB of GPU memory.

## The bridge (`aisathome-bridge`)

The bridge joins the project to a gateway as its connector (`docs/gateway.md`, "Connectors and
pilots"). It runs on the project's host as the project's user, where BOINC's tools and the project
directory are (for example in a container from the server's own image), and holds the only connector
key. The gateway is the source of truth; the bridge mirrors it into BOINC:

| Every | What |
|---|---|
| 60 s | **Demand**: as many pilots as the gateway wants. For each, it creates the pilot at the gateway, writes the credential into the download directory as a per-work-unit input file with an unguessable name (`bin/dir_hier_path`, mode 0640) and runs `bin/create_work` with the pilot's id as the work unit's name, the model as a remote sticky file and the credential as file 1. Unsent work units the gateway no longer wants (newest first), and those whose pilot has less than `min_runway_hours` left, are cancelled (`bin/cancel_jobs --name`) and reported `cancelled`. If BOINC sent the work unit while it was being cancelled, the pilot is not ended: it is bound and reported when its result is over. If `create_work` fails, the pilot is ended and its credential removed only when the work unit really is not there (a timed-out `create_work` may have made it) |
| 10 s | **Binds and ends**: when BOINC sends a work unit, it tells the gateway the BOINC user, host and team (`result.userid`, `hostid`, `teamid`); when the result is over, it reports the outcome (success, error, `expired` for no reply by the deadline, `cancelled`). A pilot with no work unit 5 minutes after its creation is reported `error` (the bridge stopped between the two steps) |
| 60 s | **Credit**: for each successful result that waits for validation, it asks the gateway for the pilot's credit (BOINC's "not validated yet" and "inconclusive": the validator's first look, before the file exists, leaves a lone result inconclusive, and it looks again later; every page of them, oldest first, so a backlog still in the gateway's hold never keeps later ones out) and, once final (a day after the pilot's end), writes it to `credit_dir/<pilot id>` (one number; written whole). The validator reads that file into the result's claimed credit and grants it with `--post_assigned_credit`; until the file is there it tells BOINC to try again later. Withheld credit is written as 0; `zero_credit` writes 0 for every pilot (a bridge for a test gateway) |
| 10 s | **People's requests**: "Run on BOINC" calls `create_account.php` with the address, the display name and the person's password hash (or, without one, the hash of a password derived from the request with the connector key, so a retry after a crash finds the same user), then `am_get_info.php` for the user id and the weak (attach-only) key. A claim calls `am_get_info.php` with the account key the person gave. BOINC's refusals become the request's failure reason; a failed call (network, database, a wrong invitation code) is tried again 2 minutes later |
| 60 s | A status line in its log (open pilots by state, created, cancelled, bound, reported, requests answered, credit files, errors) |

**The credential in the download directory.** A work unit's input files are plain downloads from
the project's download directory, by name, without authentication: that is how BOINC clients fetch
them. A pilot's credential lies there under a name with 128 random bits, so its name is its only
protection. The bridge therefore refuses to start (and `-check` fails) unless the project's web
server answers the download directory and its fan-out directories with something other than a
listing (Apache: `Options -Indexes`), and keeps the credential short-lived: it works only after the
bind, for one connection at a time, until the pilot ends or its deadline passes (60 hours), and an
unsent work unit is cancelled after about 6 hours. What remains: whoever learns the file's name (the
host it was sent to, the web server's access log, anything between the client and the server) can
connect with the credential after the bind; if they connect before the pilot does, they hold its one
connection and receive what the pilot would have (science, and people's chats once chat on pilots
is on; never personal data), and the pilot's credit goes to the BOINC user it was sent to. A
credential that the first connection redeems for a per-pilot secret only the real pilot keeps would
close that; it needs the node agent and the gateway, and is not built yet.

It reads the project's database (SELECT on `workunit`, `result` and `app` only; give it a user with no
other rights, or it uses `config.xml`'s) and never writes it: BOINC's own tools make and cancel work
units. Its work units carry `create_work --batch <batch>`, and it looks only at its own batch, so a
test work unit made by hand, or a second bridge for another gateway (a staging one, with
`target_host` and `zero_credit`), share the project safely. Never give two bridges the same batch or
the same connector. It never logs a credential, an account key, a weak key, an address or a password
hash. The account RPCs take their parameters in the query string: the bridge calls them on the
project's own web server, which should keep those pages out of its access log.

```json
{
  "gateway": "https://api.example.org",
  "connector": "boinc",
  "key_file": "/run/bridge/connector.key",
  "pilot_gateway": "wss://api.example.org/v1/node/connect",
  "project_dir": "/home/boincadm/project",
  "batch": 1,
  "model": {"url": "https://huggingface.co/<repo>/resolve/<revision>/<file>.gguf", "nbytes": 14252845984, "md5": "<32 hex>"},
  "web_url": "http://apache/project/",
  "invite_code_file": "/run/bridge/invite_code",
  "db_dsn_file": "/run/bridge/db.dsn"
}
```

Optional: `app` (`pilot`), `wu_template` and `result_template` (`templates/pilot_in`,
`templates/pilot_out`), `target_host`, `deadline_hours` (60: the pilot's deadline at the gateway, after which its credential is refused),
`min_runway_hours` (54: the work unit's 48 h delay bound, a 4 h pilot and 2 h), `credit_dir`
(`project_dir/credit`), `zero_credit`, `poll_seconds` (10), `demand_seconds` (60).

```sh
go build -o aisathome-bridge ./boinc/bridge/cmd/aisathome-bridge
aisathome-bridge -config bridge.json -check   # key, database, tools, credit directory, no listing
aisathome-bridge -config bridge.json
```

Tests: `go test -race ./boinc/bridge/` (stand-ins for the gateway and BOINC);
`AISATHOME_BRIDGE_TEST_MYSQL='user:pass@tcp(127.0.0.1:3306)/empty_db'` adds the SQL against a real
MariaDB; `e2e/bridge_test.go` runs it against the real gateway with a real node agent as the pilot.

## Open

- The pilot starts a fresh 4-hour budget when the client restarts a task it quit.
- The client's RAM accounting with the memory-mapped model and the prompt cache is not measured.
- The gateway mints per-pilot credentials (`pc-<hex>.<secret>`, "Connectors and pilots" in
  `docs/gateway.md`): one pilot, one connection at a time, until the pilot ends or its deadline
  passes; the node agent takes its node id from it. The bridge (below) requests them; a test work
  unit made by hand can still carry an ordinary node token. The project keeps the credential file in
  its download folder until the work unit is done and the file deleter removes it; the credential
  dies with the pilot, which makes that copy worthless.
- The validator that reads the bridge's credit files (BOINC's validator framework with
  `--post_assigned_credit`) is not in this repository yet.
- A pilot that ends with exit 1 (its engine kept failing) is a client error in BOINC, which BOINC
  never validates: answers it served before that earn no BOINC credit.
