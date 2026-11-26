# Model-check references

For the maintainer. Operators read "Model check" on the site page "Day to day"
(`site/src/content/docs/node/operate.mdx`).

`aisathome-node modelcheck` (package `node/modelcheck`) compares a node's greedy answers to 100
fixed prompts with a **reference**: the same prompts answered on a known llama.cpp build, model
file and GPU. It measures KL(reference || node) over the top-20 logprobs at each position and
counts confident flips. A reference is only as useful as its match to what nodes run, so each
release that changes llama.cpp or a model file records a new one.

## What is compared

- **Request:** `/v1/chat/completions`, one user message, temperature 0, `max_tokens` 32,
  `logprobs` with `top_logprobs` 20, `chat_template_kwargs.enable_thinking` false, no system
  prompt. The reference stores its request; a run uses the reference's.
- **Positions:** while both greedy answers agree, plus the first position where they differ (its
  distribution still follows the same prefix). Tokens are keyed by their bytes. When one answer
  ends where the other goes on, they part there: a node that stops early is judged by the
  reference's margin at that position; one that goes on past the reference's end differs, but
  not confidently.
- **Validity:** a reference or run whose prompts have no answer, or whose logprobs are not
  log-probabilities (missing, null, above 0, or summing to more than 1), cannot be checked
  (exit 2) rather than passing.
- **KL per position:** over the union of both top-20 lists. A token missing from one list gets
  min(that list's smallest probability, its tail mass / (missing + 1)); the rest of the tail is
  one "other" bucket. Duplicate entries with the same bytes are merged.
- **Check statistic:** KL summed over the aligned positions of 4 consecutive prompts (in the
  reference's order; a remainder joins the last group), divided by their number. The largest
  group counts.
- **Confident flip:** the answers differ at a position where the reference's top-1 led its
  top-2 by more than 0.5 nats.
- **Verdict:** pass when the largest group is at most `max_check_kl` (0.0085) and there are at
  most 2 confident flips; fail otherwise; "could not check" (exit 2) when prompts got no answer,
  the request settings differ, or fewer than 4 prompts were compared.

The limits (`modelcheck.DefaultThresholds`) come from our measurements of honest nodes (RTX 30,
40 and 50 cards, several llama.cpp builds, with and without speculative decoding) against the
big tier's reference: 0.0085 is twice the 99.9th percentile of their 4-prompt checks. Change
them only with new measurements, in the same commit as the docs and the site page. A different
quant of the same model fails most checks; close quants are not reliably told apart, which is
why a tier never mixes model files.

The prompt set (`node/modelcheck/prompts.json`) is public. It checks an operator's own setup and
the release; it must never be the prompt set of a check the network runs on nodes. The gateway's
check (below) uses a secret set.

## The gateway's check (`model_check`)

The gateway checks every node and pilot itself (`gateway/modelcheck.go`): it sends
**secret** probe prompts at random times and compares the answers with a reference by the method
above (KL per group of 4 prompts, confident flips, the same limits).

- **What a node sees.** A `request` like any other: a fresh request id, streamed, the gateway's
  default `max_tokens`, one user message, temperature 0, `logprobs` with `top_logprobs` 20,
  `enable_thinking` false, no tools, nothing in the protocol that marks it. The answer runs to its
  end (at most 4,096 tokens); its first positions are compared (as many as the reference has, plus
  one to see whether it went on). A share of people's streamed requests without tools
  (`cover_share`, default 0.2) to nodes that have passed a check (so their engine is known to
  answer with logprobs) asks for the same logprobs, which the gateway removes before the client
  sees them, so "logprobs" alone does not mark a probe. On a rented RTX 4090 (2026-11-20,
  b12342, UD-IQ4_XS) the probe's shape gave exactly the reference's numbers: 20 of 20 public
  prompts the same 32 tokens streamed with `max_tokens` 16384 as non-streamed with 32, largest
  logprob difference 0; the full public set on that card against the published b12342 reference
  (recorded on another 4090): KL 0.0000, no flip. `node/modelcheck/testdata/stream-b12342-rtx4090.sse`
  is that card's stream of the first prompt, which the parser's test reads.
- **When.** Per node, one prompt at a time, exponential waits: a mean of `recheck_seconds` (300)
  until its first check of 4 prompts is complete and after a failed check, `interval_seconds`
  (1800, a check about every 2 hours) otherwise. A probe takes only a free slot that no waiting
  request could use, and any request that then waits for the node (a person's or science) stops it.
  A node gets no prompt twice until it has had the whole pool; among those it has not had, the
  ones sent least often over all nodes come first.
- **Pilots without work.** A pilot (a BOINC task, say) leaves after 30 minutes without a request
  and cannot tell a probe from one, so probes alone would keep it on a volunteer's GPU with no
  work and no credit. The gateway therefore sends a pilot no probe once it has had no request but
  probes for `pilot_idle_seconds` (600): none in flight, and the last one ended (or, before any,
  it first connected) that long ago, by the gateway's own record of what it relayed, kept per
  pilot across a lost link (a reconnect opens no new window; forgotten a day after its last
  request). Its next prompt is
  drawn afresh at the time one falls due, so probes resume at a random time once requests come
  again, never at a set delay after the first. A pilot needs no passed check before it gets
  requests, so a new pilot is probed only in its first `pilot_idle_seconds` until work comes. It
  leaves at most `pilot_idle_seconds` (plus one probe's answer) later than 30 minutes after its last
  request; a pilot serving work is probed as any node, and nodes that are no pilot are not affected.
  The cost: the first request after such a spell is never a probe, so a modified pilot could answer
  that one from another model, which the replay of answers is there to catch.
- **Result.** Each check is pass or fail; an answer without logprobs (a proxy, or an engine that
  is not llama-server) or an error in answer to a probe fails it. A probe the node never answered
  (it left, stalled, or a request took the slot) counts as "no answer", not against it. A node's result is judged on its last 25 checks: **fail** with
  two failed checks, or more than 2 confident flips over them (the published 100-prompt limit);
  **suspect** with one (its re-check comes within minutes); **pass** otherwise; **unknown**
  before its first check. Results are on `/account/ops` (admins) and `aisathome-gateway accounts
  model-check [node]`, saved in `state_dir/model-check.json` (prompt ids, never their text; a node
  not seen for 30 days is dropped). Probes earn no credit and are in no usage record, budget or
  statistic.
- **Observe-only, then withholding.** `withhold` is off. When it is on, a node whose result is
  fail is held like a guard hit (rule `model-check`; a pilot's credit is withheld), and
  `accounts node-clear` lets it serve again with a fresh window. It takes effect only once the
  check has observed for 3 days (`observing_since` in the results file survives restarts; after
  more than a day without the check running, observing starts again), so
  the limits are first seen to hold on our own honest nodes, which it checks as any other
  (`include_trusted`, which once opted them in, is ignored since 2026-11-20). Turn it on only
  after a clean observe-only period on 3090, 4090 and 5090 nodes.
- **Admission of nodes without an account** (`gateway/anon.go`). For them the check is a gate, whatever
  `withhold` says: probes only (every `admit_seconds`, default 20, on average) until 3 checks passed,
  then science; one failed check before that holds the node, and a fail result after it does too.
  Their admission probes come from a separate **admission set** (`admit_references`, one per model),
  never from the secret set: anyone can register and read what an unadmitted node is sent, so those
  prompts are as good as public and the set is rotated (record a new pool, replace the file and its
  pin). Recording one is cheap: 100 prompts of 32 tokens take a few minutes. Once admitted, a node gets
  the secret set at random like any node. A node without an account whose model has no admission set is
  never admitted. The gateway logs a warning when an admission set shares prompts with the ongoing set
  (only a test setup, such as staging's public references, should).

Config (`model_check` in the gateway config; the reference files are secrets, kept with the
gateway's secrets and pinned):

```json
"model_check": {
  "enabled": true,
  "references": [{"model": "qwen3.8-27b", "file": "/etc/aisathome/gateway/modelcheck-iq4xs.json.gz", "sha256": "<64 hex>"}],
  "admit_references": [{"model": "qwen3.8-27b", "file": "/etc/aisathome/gateway/modelcheck-admit-iq4xs.json.gz", "sha256": "<64 hex>"}],
  "interval_seconds": 1800, "recheck_seconds": 300, "admit_seconds": 20, "pilot_idle_seconds": 600,
  "withhold": false, "cover_share": 0.2
}
```

One reference per model name nodes announce (the big tier's `qwen3.8-27b`; the standard tier's
`qwen3.8-27b-q3` needs its own). The gateway refuses to start with a missing file, a wrong pin, a
reference recorded with thinking or other logprobs, or a pool under 16 prompts.

**Recording the secret reference.** Generate a few hundred fresh prompts (real-user-shaped requests
from many templates with random content, each asking for several sentences so the greedy answer
runs past 32 tokens) into a JSON list of `{"id", "text"}`, then, on a maintainer's own card of the
reference class with the release's engine, exactly as for a published reference:

```bash
aisathome-node modelcheck record -prompts pool.json -out modelcheck-iq4xs.json.gz \
  -name qwen3.8-27b-iq4xs-b12342-rtx4090-secret -note "Gateway model check, secret pool."
```

The file is written readable by its owner only (0600); a pool has at most 2,000 prompts. Never
record it on a rented machine (its host can read the disk) or commit the pool, its generator or
the reference to any repository. Re-record at every release that changes llama.cpp
or a tier's model file, as for the published references, and replace the pool when it may have
leaked or nodes have seen most of it (each node sees a check about every 2 hours: a pool of 700
prompts lasts a node about two weeks before it repeats).

**What it catches and what not.** It catches a wrong model file (another quant: Q3 and below fail
clearly; neighbouring 4-bit files often pass), another model, and a proxy without logprobs. It does
not catch a node agent modified to recognise probes and route them to the real model: a probe is
greedy, asks for top-20 logprobs and has no tools, which few real requests do (science traffic
always has tools, so on a science-only pilot a chat-shaped request stands out). The cover share
removes "logprobs" as a sign but not the rest. The check proves that the right model was reachable
when probed, not that it wrote the other answers: that is the replay's job.

## File format

`aisathome-modelcheck/1`: one JSON object, gzipped for the built-ins.

| Field | Content |
|---|---|
| `format`, `kind` | the format version; `reference` (recorded to publish) or `run` (a node's `-out`) |
| `name`, `created`, `note` | e.g. `qwen3.8-27b-iq4xs-b12342-rtx4090`, the UTC date, a note shown to operators |
| `engine` | `name` (llama.cpp), `build` (the release tag, `b12342`), `commit`, `build_info` as `/props` reported it |
| `model` | `file` (name only, never a path), `sha256`, `ftype` (llama.cpp's file type) |
| `gpu` | `name` (nvidia-smi's) and `cap` (compute capability) |
| `server` | `tier`, `ctx`, `slots`, `kv`, `speculation` |
| `request` | `max_tokens`, `top_logprobs`, `thinking` |
| `results` | per prompt: `id`, `text`, `tokens`: per position `k` (the chosen token's bytes in hex) and `top` (`[hex bytes, logprob]` pairs) |

A run saved with `-out` has the same format, so `modelcheck compare -ref A -run B` compares any
two files offline.

## Built-in references

Shipped in `node/modelcheck/references/` and listed in `modelcheck.Builtins` with their SHA-256;
the program checks the pin when it loads one. `TestBuiltins` fails when a file has no entry, a
pin is wrong, or a reference's model file, SHA-256 or KV type is not a tier's (`node/setup`).
Without `-ref`, the check picks the built-in for the model file the engine serves, on the same
llama.cpp build if there is one, else the newest; the report says when the build differs.

| Reference | Recorded on | Status |
|---|---|---|
| `qwen3.8-27b-iq4xs-b12342-rtx4090` | big tier file, llama.cpp b12342 (`0fb8f15`, the node v0.6 engine, the node's own build), RTX 4090, 1 x 196608, q4_0 KV, speculation off, 2026-11-15 | the default for b12342 (and newer) engines. On the recording machine a second plain run matched it exactly and a run with MTP passed it (largest group 0.0009, no confident flip) |
| `qwen3.8-27b-iq4xs-b11146-rtx4090` | big tier file, llama.cpp b11146 (`7fe450e`, the node v0.4 engine), RTX 4090, 1 x 196608, q4_0 KV, 2026-10-23 | the reference the limits were set against; kept for older engines. b12342 passes it, plain and with MTP (largest group 0.0032, no confident flip) |
| `qwen3.8-27b-q3kxl-b12323-rtx4060ti` | standard tier file, llama.cpp b12323 (`93cf736`, the node v0.5.0 bundle's own llama-server), RTX 4060 Ti 16 GB (rented), 1 x 81920, q4_0 KV, speculation off, 2026-11-22 | the default for standard-tier engines from b12323 on; staging's public stand-in for the standard tier's secret and admission sets |
| `qwen3.8-27b-q3kxl-b11146-rtx4090` | standard tier file, llama.cpp b11146, RTX 4090, 1 x 131072, q4_0 KV, 2026-10-11 | provisional: no honest standard-tier node on another card compared yet |

A new llama.cpp build moves the logprobs about as much as another GPU type does, and the two
add up, so **record a reference on the release's build**.

Not checked yet: upstream's official b12342 Linux CUDA build (it needs glibc 2.38, newer than
the Ubuntu 22.04 image used for the recording), and the upstream Windows build the Windows node
ships. Run `modelcheck` on a Windows node before relying on it there.

## Recording a reference at a release

When `LLAMA_TAG` or a tier's model file changes (`docs/RELEASING.md`):

1. Take the release's bundle (the dry run's `dist` artifact) to a GPU of the reference class
   (an RTX 4090 for the big tier so far; a 16 GB card for the standard tier) and install it as
   usual (`install.sh` with the tier's pinned model; `--speculation off`). Pause the node (local
   UI) or run it without a token, so nothing else uses the engine.
2. Record:

   ```bash
   aisathome-node modelcheck record -out qwen3.8-27b-iq4xs-b12342-rtx4090.json.gz \
     -note "Big tier (UD-IQ4_XS, q4_0 KV) on llama.cpp b12342, the engine of node v0.6."
   ```

   It reads the build from `/props` (`-engine-build bNNNN` when it says `b1`, as bundles before
   v0.6.0 do), hashes the model file the engine loaded (`-model PATH` or `-model-sha256` when
   it cannot read it), takes the GPU from nvidia-smi (`-gpu` with several cards) and the tier
   from the model file. It refuses to write a file when any prompt fails, and prints the SHA-256
   and the `Builtins` entry to paste. Name the file `.json.gz`: the built-ins are the gzipped
   files, pinned by the SHA-256 of the `.gz` bytes (a `.json` output is plain JSON; gzip it
   and pin the new hash).
3. Sanity checks on the same machine: `aisathome-node modelcheck -ref <file>` passes (a
   second run of the same engine), `modelcheck -ref <the previous reference>` passes, and, for
   the big tier, the same with `--speculation mtp`.
4. Commit the file to `node/modelcheck/references/` with its `Builtins` entry; mention it in the
   CHANGELOG. Keep the older references while nodes may still run their builds; remove one only
   with the release that stops supporting its build.

Keep references small (about 0.8 MB gzipped for 100 prompts) and never add machine details beyond
the GPU model: no host names, paths or serial numbers.
