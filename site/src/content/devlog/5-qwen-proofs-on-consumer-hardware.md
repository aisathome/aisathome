---
title: "Maths proofs done by Qwen3.8 27B, runnable on consumer hardware"
number: 5
date: 2026-11-22
summary: "Qwen3.8 27B on consumer GPUs has 11 accepted proofs on prove2me, 8 of them its own, from 43 open lemmas: where it works, what it costs, and how the pipeline runs."
publish: true
---



An Anthropic mathematician has [announced a proof of the Riemann hypothesis made with Claude](https://www.nature.com/articles/d41586-026-03812-7), and so far it looks like it holds up. Math proving is moving completely into the hands of AI models now. Currently it is only possible for the frontier labs.

The shift is astonishing, but we have seen shifts like this before. Computation used to be done by humans, and it moved to computers. What I want to figure out is what is possible with open-weight models and consumer hardware. What can we do without depending on the megacorps?


I started out looking at [Tau Ceti](https://taucetiproject.org/), which by itself is a very interesting project where they build a shared library of fundamental mathematical proofs in Lean.

I have since moved on to [prove2me](https://prove2.me/). It is more concerned with results than with a shared library. People and models post Lean statements cut out of real math papers, and anyone can submit proofs. Prove2me checks every proof itself with Lean, so the proofs Qwen3.8 writes are verified in exactly the same way as proofs from frontier models. Either Lean accepts it or it doesn't.

So the question: is it possible to contribute to theorem proving using just Qwen3.8 27B on consumer hardware?

## Results

Promising, but not a slam dunk.

- 43 different open lemmas attempted over 10 trials (some of them several times while we tuned the setup, and 4 only got a first outline).
- 11 accepted by prove2me: 8 proved by Qwen itself, 3 by reusing a proof that already existed on the platform.
- 26 failed. 6 are still running in the current trial.
- The 5 most recent proofs took about 24 GPU-hours each on RTX 3090/4090s, counting everything those trials ran. Most of that time goes to the lemmas it doesn't manage to prove (83% in those three trials). *(Update, 27 November: this figure is out of date. From trial 11 on we ran only lemmas we judged easy or moderate, and the cost per accepted proof fell well below it. The [proved theorems page](/science/prove2me/theorems/#cost) has the current numbers, computed from the gateway's records each time it loads. The 24 GPU-hours stays here as what these trials cost.)*
- In tokens, each of those 5 proofs cost about 111M prompt (input) tokens and 3.0M generated (output) tokens. About 96% of the input tokens are served from the KV cache, so only about 4M input tokens per proof are actually computed.

| Summary | |
|---|---|
| Trials | 10 (plus 2 rounds of outline probes) |
| Open lemmas attempted | 43 |
| Proved by Qwen (accepted by prove2me) | 8 |
| Accepted via an existing proof | 3 |
| Failed | 26 |
| Still running (trial 10) | 6 |
| GPU-hours on these lemmas | 208 |
| Prompt (input) tokens, trials 6-10 | 702M (96% from the KV cache) |
| Generated (output) tokens, trials 6-10 | 19.8M |

GPU-hours are the time the model spent answering a lemma's tasks, failed and cancelled ones included; Lean checks on the CPU machines and runs on already-proved calibration lemmas and benchmark replays are not counted. The token counts start at trial 6 (26 of the 43 lemmas, 153 of the 208 GPU-hours), because trials 1-5 did not record the tokens served from the cache.

Of those 208 GPU-hours, 47 went to the 8 lemmas Qwen proved, 1 to the 3 closed with an existing proof, 134 to the 26 that failed, and 26 so far to the 6 lemmas of trial 10.

I have dug into where it works and where it doesn't. I set up different trials (each a collection of theorems) and investigated where it succeeds and where it fails. With the current pipeline (trials 6-9), lemmas I would call bachelor level (finite sums, induction, a bit of linear algebra) it proved 6 out of 6. The harder ones, more like a lemma a PhD student writes in a paper with analysis and long chains, it proved 0 out of 8. For comparison, OpenAI's Codex proved two of those hard ones in about 30-35 minutes each. So the gap to the frontier is real, but the easier half of the work is doable on a 3090.

| Lemma | Codex | Codex time | Codex tokens in / out | Qwen | Qwen GPU-hours | Qwen tokens in / out |
|---|---|---|---|---|---|---|
| [03d4b4](https://prove2.me/theorems/03d4b499-ce62-4a28-a92c-001d2940e42d) Edit distance | proved | 29.4 min | 3.8M / 41k | failed | ≥ 20.2 | 98.3M / 2.2M |
| [115f6c](https://prove2.me/theorems/115f6c5f-c75e-4b73-b209-623860ff8a1a) Pinsker's inequality | proved | 34.3 min | 4.9M / 44k | failed | 21.1 | 110.4M / 2.7M |

Codex's time is the sum of its agent sessions on the lemma, through the same pipeline. Qwen's numbers cover every task it ran on the lemma in that trial, failed outlines included ("≥" because some cancelled tasks did not report their time). Input tokens include the cached ones for both (94-95% for Codex, 97% for Qwen). Qwen generated 50-60 times as many tokens as Codex on these two and still did not get there.

The hardness classes come from the setup we now use to pick lemmas before a trial. For trials 6-9 they were assigned after we knew the outcomes (the shorter readings written before those trials split almost the same way: 6 of 7 against 0 of 7), so read the split as a strong hint, not a measurement. Trial 10, still running, is the first test on lemmas the rubric did not see.

| Hardness | Level (rough) | Proved by Qwen, all trials | Proved by Qwen, trials 6-9 | Via existing proof | Running |
|---|---|---|---|---|---|
| easy | bachelor | 2 of 5 | 2 of 2 | - | - |
| moderate | bachelor | 6 of 8 | 4 of 4 | - | 2 |
| moderate-hard | master | 0 of 3 | - | 1 | 2 |
| hard | PhD | 0 of 14 | 0 of 8 | 2 | 2 |
| research | research | 0 of 4 | - | - | - |
| **Total** | | **8 of 34** | **6 of 14** | **3** | **6** |

"Proved, all trials" counts the finished attempts that were not closed by an existing proof. The three easy failures are all from trials 1-2, before the pipeline below existed, when a lemma got one agent session of 30-70 minutes.

Live queue and every attempt: [aisathome.org/science/prove2me](https://aisathome.org/science/prove2me/)

## Architecture

This runs on AIs@home, a volunteer network I'm building: people run a node on their own GPU, and a gateway hands out work. Here are some rough details on the setup. Most of it is also open source, [here](https://github.com/aisathome/aisathome).

![Top: volunteer GPU nodes running Qwen3.8 27B talk to the gateway, which talks to runners (CPU spot machines with the agent and a Lean sandbox); runners submit to prove2me. Bottom: outline, lemmas as tasks, assembly, Lean check, submit; a failed step sends the lemma back to a new outline.](/images/devlog/5-qwen-proofs-pipeline.svg)


#### The model

The model is Qwen3.8 27B in Unsloth's UD-IQ4_XS quantisation (~14.3 GB), running on llama.cpp.

I run it with 192k context and a q4_0 KV cache, so it fits on a 24 GB card. Past ~195k, long-context recall dropped clearly (77/80 at ~195k against 62/80 at ~247k on a needle test), so I capped it there.

MTP speculative decoding gave about 1.7-1.9x tokens per GPU minute, which adds up when one proof takes a day of GPU time.

16 GB cards run UD-Q3_K_XL at 80k context, but all the proofs so far came from 24 GB cards.

#### The agent

opencode drives the model, inside a sandbox with Lean 4 and Mathlib. In there it can write Lean, run it and read the errors. The model can only reach the gateway, nothing else.

One task is often 40-130 agent steps, and the agent re-reads its whole context every step. That sounds wasteful, and it would be, but about 96% of the prompt tokens come out of the KV cache. That is the only reason this is doable on one GPU: about 700M prompt tokens over the last five trials, but only ~20M generated.

#### The pipeline

First the model writes an outline: the proof split into small lemmas that are not yet proved. Think of it as the skeleton of the argument, with the pieces still left open.

Each of those lemmas then becomes its own task, spread over the GPUs in the network. Each task is a fresh agent session that proves just that one lemma.

When all the pieces are proved, they are put together and checked by Lean, and only then submitted to prove2me, which checks it again. If a step fails, the lemma goes back to a new outline.

Lean runs on small CPU machines (spot instances, 8 GB). Mathlib alone wants about 5 GB of page cache, so memory is actually the constraint right now. However, that is primarily because I try to keep the cost of the instances as low as possible.

Splitting into lemmas, together with a bigger budget per lemma and easier lemmas, took us from 1 proved open lemma in 27 attempts (trials 1-4) to 6 in 14 (trials 6-9).

## Limitations

- Slow. ~24 GPU-hours per proof is a lot, even if it's cheap hardware. *(Update, 27 November: see the note above; the current cost per proof is on the [proved theorems page](/science/prove2me/theorems/#cost).)*
- The outline is the weak point. The outline works like the first rough draft of the proof, end to end, but without proving each step. The results so far show that when the outline is good, the model proves almost every piece on the first try. When the outline has a false step, or one step as hard as the whole lemma, everything after that is wasted time. That is where most of the GPU-hours go right now.
- Hard lemmas are out of reach for now. Maybe new models or better scaffolding will improve this.
- Small numbers. Each recent trial is 5-6 lemmas, so it's hard to tell if a change actually helped.
- There are not that many easy lemmas on prove2me. People point their Claude/Codex harnesses at prove2me, and the easy lemmas get solved quickly by those harnesses/models.

## Next steps
If you want to help or contribute, the easiest way is to run a node. A node will receive parts of a lemma and work together with other agents to solve the overall theorem. You need an NVIDIA GPU with 16 GB or more. There is a Windows installer and a Linux install on [aisathome.org/node](https://aisathome.org/node/). A one-line install that needs no account is coming in the next node release. *(Update, 25 November: it came with v0.6.0. On Linux, `curl -fsSL https://aisathome.org/node.sh | sh`; [read the script](https://github.com/aisathome/aisathome/blob/main/site/public/node.sh) first.)*

If you do not want to run a node yourself or use the setup here, that is also possible, but I do not have a boilerplate for it. You would have to register on prove2me, find some easier theorems with the help of an agent, and prompt your way to a solution.

It requires quite a complicated setup with Lean, agents etc. You can see how I set that up [here](https://github.com/aisathome/aisathome/tree/main/harness) (the agent harness and its sandbox), and how I set up the node [here](https://github.com/aisathome/aisathome/tree/main/node).

Currently I am working on improving the setup, particularly the scaffolding. I noticed some wasted work, so making the pipeline more efficient is one topic. Being able to reuse already proved theorems and lemmas efficiently is another goal. It's actually not as easy as it sounds, as there are hundreds of thousands of lemmas available now. I am also working on picking the lemmas we work on better, making sure they are not too hard before we begin working on them, and on having a stable and reliable setup. More computation is also highly needed, but it's not that easy to get.