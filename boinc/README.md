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

## Open

- The pilot starts a fresh 4-hour budget when the client restarts a task it quit.
- The client's RAM accounting with the memory-mapped model and the prompt cache is not measured.
- The credential becomes a single-use per-pilot credential minted by the gateway; until then a
  test work unit carries an ordinary node token. The project keeps the credential file in its
  download folder until the work unit is done and the file deleter removes it; only the
  single-use credential makes that copy worthless.
