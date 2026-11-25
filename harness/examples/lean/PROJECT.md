<!-- SPDX-License-Identifier: Apache-2.0 -->
# Lean example

status: local-only
since: 2026-11-25
agreement: none

<!-- Read by the harness (aisathome_harness.project): local-only, so it never submits. -->

## What

One exercise, `exercise/Hello.lean`: prove `a + b = b + a` for natural numbers with Lean's core only. It
shows the whole path of a task (prepare, the sandboxed session, the jailed check, fix rounds, the evidence)
on any OpenAI-compatible endpoint, a small local model included.

## How a task is checked

The model's `Hello.lean` is scanned (no `sorry`, `admit`, `native_decide`, axioms or code run while
compiling), built with our own Lake files in a jail, and its theorem's type and axioms are checked there.

## Results

None kept: run it yourself; the evidence lands in `raw/hello/`.
