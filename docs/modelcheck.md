# Model-check references

For the maintainer. Operators read "Model check" on the site page "Day to day"
(`site/src/content/docs/node/operate.md`).

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
the release; it must never be the prompt set of a check the network runs on nodes.

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
