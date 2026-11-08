# SPDX-License-Identifier: Apache-2.0
"""aisathome_harness: run an open model behind any OpenAI-compatible endpoint as an agent on outside
projects' tasks, check the results mechanically, keep the evidence, and submit only where the
maintainers have agreed.

Modules: task (one task to its end: the seam for a queue), project (the contract, status, submit lock),
loop (one session), rounds (fix rounds), evidence (the evidence layout), stepmetrics (per-step metrics),
behaviour (how a run spent its steps), loop_guard (stuck-loop detection), host (the machine: limits,
build slots, host-side commands), safefs and hostgit (host access to the model's files and tree),
sandbox (the hardened container: networks, forwarder, recipe), lean (the Lean kit).
"""
__version__ = "0.1.0"
