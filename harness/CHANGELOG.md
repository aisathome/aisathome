<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog: aisathome_harness

What changed in each release of the harness. Versions are the git tags `harness-v<version>` (the node's
releases are `v<version>` in the repository's own CHANGELOG.md). The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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
