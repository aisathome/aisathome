<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog: aisathome_harness

What changed in each release of the harness. Versions are the git tags `harness-v<version>` (the node's
releases are `v<version>` in the repository's own CHANGELOG.md). The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.3.0] - unreleased

The execution side of a stateless queue worker, taken out of prove2me's worker (which now uses it, with the
same behaviour).

### Added

- `queue`: `QueueClient`, one client for a gateway's task API (create, list, get, claim with `kinds`, `task`,
  `max_class` and `hold_seconds`, heartbeat, complete, fail, give back, cancel, max_tries, review, decide,
  exported, continue, the public queue without a key). URL, key or key file, and project passed explicitly;
  each call a `Reply` (status, body) whose `check()` raises a typed error (`BadRequest`, `Unauthorized`,
  `Forbidden`, `NotFound`, `Conflict`, `Unavailable`); the key never follows a redirect; optional retries with
  backoff on network errors. `complete` sends a result once more without `cached_tokens` to a gateway from
  before it. `safe_text` (one line the gateway's text check accepts), `task_headers` (`X-Aisathome-Task`).
- `worker`: `Worker`, one task at a time: held tasks first (resumed with their lease, else claimed back by
  id), the lease renewed while a child process runs the task (an expired lease claimed back, a lost one stops
  the child), the result or the crash reported (retried while the gateway is unreachable, else held and
  reported later), three crashes in a row pause it, SIGTERM or a watcher's notice stops the child, takes its
  checkpoint (`checkpoint-task`) and gives the task back with its try; a busy marker file while a child runs.
  `child_main` dispatches the child's commands (`run-task`, `cleanup-task`, `checkpoint-task`) to the
  project's callbacks; `GiveBack` gives a task back instead of failing it. Clock and process calls injectable.
- `checkpoint`: the store interface and `DirStore`; a task's state as a `Layout` of named parts (excluded
  paths, optional parts, parts cleared by the project's remover) packed into one tar.zst with a manifest,
  `save` (latest and previous), `restore` (only when the store's copy is newer or the local one incomplete;
  sha256 checked, `previous` after a cut upload), `sync_evidence` and `fetch_evidence` (changed files only,
  an index, every file checked on the way back).
- `aws` (optional): `S3Store` (path-style SigV4 requests, standard library only), `Credentials` (environment,
  `aws configure export-credentials`, or the instance role), `NoticeWatch` (spot interruption and Auto Scaling
  termination from IMDSv2).

- `queue.QueueClient.claim`: a gateway from before `hold_seconds` refuses the field; the claim goes once more
  without it and later claims leave it out (`hold_refused`; the worker says so once). Nothing is claimed twice:
  the refused claim claimed nothing.
- `task.Endpoint.headers` and `loop.Session.headers`: extra headers on every model request of a task (opencode's
  provider `headers` option; the sandbox's forwarder passes them on). A queue worker's project sets
  `queue.task_headers(task_id)`, so the gateway's account pages show which task a node is serving. Checked
  (`loop.request_headers`): a token as the name, printable ASCII as the value, never Authorization or Host.
  Without headers the opencode config is as before.
- `Worker(gate=..., claim_lock=...)`: the project's gate (a reason to wait, or None) holds new claims, e.g. while
  as many tasks run as the model has slots; held tasks still resume. A gate that raises is logged and ignored.
  With `claim_lock`, the gate and the claim run under one host-wide file lock.

### Changed

- `Worker.run_forever` installs its signal handlers only on the main thread (it can run in a thread in tests).

## [0.2.2] - unreleased

### Fixed

- `lean.holes.context_text`: a local binder with the same name as a theorem it left out is no longer a use of
  that theorem. After `theorem h : True := by sorry`, the independent `def keep (h : Nat) : Nat := h` was left
  out too (and what used `keep`). The names a declaration binds (its header's binder groups, `fun`, `∀`, `∃`,
  `intro`, `obtain`, `have`, `let`, patterns after `with` and in `|` alternatives) shadow a bare use of the
  same name; a qualified use (`_root_.h`, `Foo.h`) still counts. Best effort: a local bound anywhere in the
  declaration shadows the name in all of it.

## [0.2.1] - unreleased

Three fixes in `lean.holes`, found on prove2me's trial-5 outlines and in a review.

### Fixed

- `commands`: the comment lines that belong to a declaration no longer stop at a blank line inside a block or
  doc comment. The command then started inside the comment, and `dry_assembly` inserted hole lemmas there.
- `normalise` (and so `statement_key`, `signature_key`) no longer collapses whitespace inside string and char
  literals: `"a  b"` and `"a b"` are different values. Keys of statements with such literals change.
- `context_text` also leaves out every declaration that uses a theorem it left out (one that still holds a
  `sorry`), and what uses those in turn. Before, such a declaration stayed, the context module did not compile
  ("Unknown identifier"), and every later hole's statement failed to compile alone, so the holes went in place
  (prove2me's trial 5: 11 of 12 holes of one outline).

### Added

- `lexer.comments` and `lexer.literal_spans`: the spans of comments and of literal text.

## [0.2.0] - 2026-11-13

A new module and two round-engine changes.

### Added

- `lean.holes`: one proof, many holes. A copy of an outline in which each `sorry` marks its position (tactic or
  term) and runs Mathlib's `extract_goal *` (`standin_copy`); the extracted statements read from Lake's output,
  with why a hole is not extracted (not reached, elaborated more than once, data, metavariables; `extracted`);
  a key per statement from the environment, the normalised context and the normalised statement
  (`statement_key`, `signature_key`); the file's top-level commands, the scope lines and the definitions a lemma
  file needs (`commands`, `scope_lines`, `context_text`); the dry assembly (each hole's lemma in a
  `namespace <block>` above the declaration that holds it, the hole replaced by its application;
  `dry_assembly`, `failed_blocks`) and the assembly from the lemmas' proof files (`assemble`). Text only: the
  project compiles. The stand-in copy raises `maxHeartbeats` to 2,000,000 (`STANDIN_HEARTBEATS`): the holes of one
  proof share its budget, and at Lean's default the 4th extraction of a real 8-hole outline ran out (prove2me's
  validation on a runner, 2026-11-13).

### Changed

- A follow-up round that made no tool call runs again once (continued in its session with its own caps,
  like a round the endpoint consumed) instead of ending the task at once; a second one still ends it not
  scored (`unscored: "no_tool_calls"`). It never reaches the project's feedback. prove2me's trial 4 lost a
  leaf's last fix round to one reply cut after 98 characters.
- The first-edit watcher may fire more than once in round 1 when the session sets `first_edit_again` and
  the watcher's `again()` says so (a project's later nudge, sent as `Prompts.first_edit` at that moment);
  run.json's `first_edit_nudge` keeps the first firing and lists the later ones in `again`. The default
  is unchanged: one nudge. prove2me's step-20 progress nudge never fired because the step-8 first-edit
  nudge used the only one.

### Fixed

- `Sandbox.drop` removes the root-owned, mode-000 `work/work` directories that overlayfs leaves in a
  session's state: after a spot reboot in the middle of a session they still held files, the ordinary
  removal skipped them silently, and the project's next removal of the state directory failed with a
  PermissionError (prove2me, 2026-11-10: one leaf's prepare ended a whole run). What an ordinary
  removal leaves goes by `sandbox.remove_as_root`: `rm -rf --one-file-system` as root in a container
  of the recipe's image, with no network, only DAC_OVERRIDE, no new privileges and only the state
  directory mounted. `drop_overlays` does it when given an image; without one it behaves as before.

## [0.1.2]

### Security

- The egress forwarder refuses a request head with a bare CR or LF anywhere, a method or target with any
  byte outside visible ASCII (a TAB, LF, CR, NUL or DEL in the target passed the base-path check, and a
  lenient endpoint could split it into a second request, to another path, with the key), and a header
  value with a control byte other than TAB. Header values lose only spaces and tabs at their ends.
- The key no longer follows a redirect: the host-side calls to the endpoint (its /models for the context,
  `rounds.gateway_probe`) sent it on to wherever the endpoint redirected, another host or plain HTTP.
- The build-slot directory is mounted read-only into the model's container (`buildslot.sh` locks with a
  read descriptor): the model could replace a lock file, so builds stopped sharing the slots, or plant a
  link that the host then opened. The host opens slot files only as regular files, never through a link.

### Fixed

- `safefs.write_bytes` writes all the bytes: a short `os.write` (a full disk, a quota) left a truncated
  file that replaced the target, which could be opencode's session database. A write that makes no
  progress now fails, and the target stays as it was.
- The time cap counts a step that a stop interrupted. Its model time was lost (only finished steps
  counted), so a resume, also after the time cap itself stopped the run, got that time again. Each
  segment records it as `open_step_s` (a checkpoint records it for a segment still running); run.json's
  `model_s` includes it.
- A container left by an earlier run is removed when a run starts: a resume whose budget was spent ran no
  segment, so the old container could still change the tree after the final diff. If `docker rm -f`
  cannot remove it, or Docker cannot confirm it is gone, the run stops (`Sandbox.before` raises)
  instead of going on beside it.
- An open step stops counting against the time cap at an endpoint error: the wait for opencode's retry
  counted as model time (a failed attempt is still the endpoint's, as before).
- A failed final checkpoint removes model.diff and sets `model_diff_failed` in run.json: an earlier
  checkpoint's diff passed for the final tree.
- The model's container runs as the invoking user by default (`Recipe.user`), not a fixed 1000:1000,
  which could not write the tree and HOME a project prepared on a host whose user is not 1000. Root is
  refused.

## [0.1.1]

### Security

- The endpoint's API key no longer enters the model's container. In 0.1.0 it was mounted read-only at
  /run/harness/key, where the model's shell tools could read it (and its output would land in the
  evidence). Now the egress forwarder holds it: opencode talks plain HTTP to the forwarder (the endpoint's
  name still maps to it), and the forwarder drops any Authorization header the client sent, adds the key,
  and forwards over TLS with the endpoint's certificate and name checked. It forwards only origin-form
  requests under the endpoint's base path (no other host, no `..`), one request per connection. Any
  OpenAI-compatible endpoint still works. The forwarder image needs python3 and CA certificates, and with
  a key the forwarder runs as the calling user so it can read the key file.
- One forwarder per endpoint URL and key (a changed key gets a new one); a forwarder that no session uses
  any more is removed when the last session closes, so none keeps an old key.
- After each session the key is masked in every evidence file (same length, so event offsets stay valid;
  `evidence.redact`), and the log says so if it ever had to.

### Fixed

- model.diff is written as git's bytes: a CRLF file's patch applied no more after text decoding.
- A retried round is judged on its own attempt: its steps and error events are counted from the retry's
  start, so two endpoint failures in one round are both unscored infrastructure (`round_steps_all` keeps
  the whole round's steps).
- Forwarder names end in a hash of the endpoint: two long host names with the same first 40 characters
  shared one forwarder, and the second endpoint replaced the first's while it ran. Forwarders named the
  0.1.0 way are removed by the idle sweep once no session uses them.

## [0.1.0]

### Added

- First version (0.1.0): `run_task`, one task to its end, the seam for a queue; the session loop
  (opencode in a hardened container against any OpenAI-compatible endpoint; step and model-time caps,
  checkpoints, resume, the first-edit and stuck-loop nudges, the step-limit notice); the fix-round engine
  with infrastructure rounds kept apart; the evidence layout and per-step metrics; the sandbox (a network
  per session, a forwarder per endpoint, a container recipe, build slots shared with host builds); safe
  host access to the model's files and tree (no symlink is followed, the tree's git config never runs);
  the Lean kit (toolchain, package sets, a lexer for the text checks, the sorry check, the
  forbidden-construct scan and the type-and-axioms check, a proof playbook); the project contract with its
  status and submit lock; a template adapter; offline tests.
