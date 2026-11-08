<!-- SPDX-License-Identifier: Apache-2.0 -->
# Changelog: aisathome_harness

What changed in each release of the harness. Versions are the git tags `harness-v<version>` (the node's
releases are `v<version>` in the repository's own CHANGELOG.md). The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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
