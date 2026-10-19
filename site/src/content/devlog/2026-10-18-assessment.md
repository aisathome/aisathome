---
title: "Assessment: week of 11–18 October 2026"
date: 2026-10-18
summary: "Why selling tokens is hard, Stockfish and Lean proofs (Tau Ceti) as science cases for AI at home, where agentic work should run, the week’s costs, and the 24 GB limit."
publish: true
---

Cases for AI inference at home. I have been thinking quite a bit about whether the AI value
chain will be a winner-takes-all scenario: centralisation in a few megacorps, or
decentralisation across the many. Setting up a decentralised AI network right now has many
issues and drawbacks. I outlined a couple of them in my
[previous post](/devlog/2026-10-12-r-gridcoin/).

One obvious use case is selling tokens and listing AIs@home on
[OpenRouter](https://openrouter.ai/) or something similar. A machine with a high-end GPU could then generate tokens when the operator does not need it.
I do not think that is an easy option
right now, partly because the economics do not make sense: for example, paying for a lot of
[Vast.ai](https://vast.ai/) instances and then going through the hoops of getting approved on
OpenRouter or something similar. There are three more reasons that I think make it even harder:

- **Security.** Nodes can be operated by anyone, and they can see the prompts (see our
  [privacy page](https://aisathome.org/about/privacy/)).
- **Competing with free.** Qwen3.8 27B is already served for free, although with small limits,
  and other strong models are free in huge volumes.

  | Free Qwen3.8 27B, checked 18 Oct | Free limit |
  |---|---|
  | [Groq](https://console.groq.com/docs/rate-limits) | 30 requests/min, 1,000/day, 200k tokens/day |
  | [Cloudflare Workers AI](https://developers.cloudflare.com/workers-ai/models/qwen3.8-27b/) | 10,000 "neurons"/day: about 34k output or 244k input tokens |
  | [OVHcloud AI Endpoints](https://docs.ovhcloud.com/en/guides/public-cloud/ai-machine-learning/ai-endpoints-capabilities) | 2 requests/min per IP, no account |
  | [OpenRouter](https://openrouter.ai/qwen/qwen3.8-27b) | none since 5 Oct (paid only) |

- **The alternative already exists.** You can already rent out your GPU on
  [Vast.ai](https://vast.ai/hosting), [Salad](https://salad.com/) (as a "Chef") or
  [Clore.ai](https://clore.ai/). I have no insight into what the actual payouts on those sites are, though. Their calculators probably inflate the numbers a bit and assume a 100% load.

## AI inference at home for science research

This is an alternative. You can build a network, do something useful and as
[BOINC](https://boinc.berkeley.edu/) shows, the supply of compute comes. That is what
the community enjoys. What is important is that the science being done actually has to
be valuable (although I question the value of some of the projects, even some that are
whitelisted in [Gridcoin](https://gridcoin.us/)). The perceived value of the work has to be high
for the cruncher.

I have found a few attempts at AI on volunteer hardware ([AI Horde](https://aihorde.net/) for
chat, [MLC@Home](https://www.mlcathome.org/) and Axiom on BOINC), but none that runs LLM
inference at home for science in a way that is actually used. I have done some research, and I
found two cases interesting.

**[Stockfish](https://stockfishchess.org/), the chess engine.** This is interesting both as a
traditional BOINC-style project and for AI inference research at home. As a traditional project,
it needs CPUs to verify potential improvements: every functional change has to pass [Fishtest](https://tests.stockfishchess.org/), where
volunteers' CPUs play tens of thousands to hundreds of thousands of games between the new variant
and the current engine. It has to run on a CPU because the search evaluates millions of positions
one at a time, and GPUs are only efficient on large batches.

For AI inference on GPUs, the most interesting case is researching and developing hypotheses for
new improvements to the code, especially the algorithms for searching the tree. Here we have a case for AI inference
in research. It would also be possible for the AI to research ways to improve the neural network
inside Stockfish, then retrain and test it. That is a small loop in the spirit of RSI (recursive
self-improvement).

What makes this case a bit difficult is that no scaffolding is built around using AI in this way.
A few people have already had LLMs write Stockfish patches, and a handful passed Fishtest and were
merged ([example](https://github.com/official-stockfish/Stockfish/commit/218c74ec4d97807afaab3a4dbda94f43e6e02647)),
but out of hundreds of attempts and by hand. How do you split the research goals and hypotheses,
and how do you scale the research across tens or hundreds of nodes with minimal human setup? The
existing Stockfish community might also be sceptical of using AI this way at volume. It is also
questionable whether Qwen3.8 27B is powerful enough here; most of the research might require
frontier models (the merged LLM patches came from much bigger models).

**Mathematical proofs.** The second case, which I think is very interesting, is using AI for
mathematical proofs. I am not a mathematician and don't have much insight into it at all, and
there is quite some resistance to using AI this way. But if you look at the recent progress with
autoformalisation in [Lean](https://lean-lang.org/), it is fast: OpenAI's
[Navier–Stokes result](https://github.com/openai/NavierStokesAndEuler) (the version with an
external force; the Clay Institute is still reviewing it) and Anthropic's
[complete Lean proof of Fermat's Last Theorem](https://www.anthropic.com/research/formalizing-fermats-last-theorem).

There is a well-maintained library called [Mathlib](https://github.com/leanprover-community/mathlib4),
which contains both the infrastructure for formalising proofs and a ton of proofs that are useful
for future proof work. The library itself is not particularly AI-friendly (see its
[AI policy](https://leanprover-community.github.io/contribute/index.html#use-of-ai)). But there is
a related library called [Tau Ceti](https://github.com/TauCetiProject/TauCeti), built on top of
Mathlib by the [Lean FRO](https://lean-fro.org/) with some of the same people. Tau Ceti is
intended for AI-developed proofs, and its tools provide the scaffolding needed for
automated work. It also has a [roadmap](https://github.com/TauCetiProject/TauCetiRoadmap) that
guides the AI agents, written by mathematicians. I have run experiments against it to see if the
work passes: Qwen3.8 27B on one rented 3090 got 5 of 9 small, well-specified tasks through Tau
Ceti's CI checks, run locally (nothing was submitted to them). This seems like quite a good initiative for
AIs@home to contribute to. Most of the work being committed right now uses Codex or Claude Code;
using open-weight models would be genuinely new.

## Architecture of agentic research

The Tau Ceti case flips the architecture somewhat. In the initial setup we had just an LLM running
on a node, and a user would run either a chat interface or [opencode](https://aisathome.org/use/api/)
themselves. To support a use case such as Tau Ceti, we need additional nodes, or some other place,
running opencode itself. This is because opencode needs access to tool calls, and here those
(Lean builds and checks) are heavy on CPU, memory and disk. In our Tau Ceti
experiments, about 10% of the time went to local tool calls and about 90% to waiting for the model:

| Where the time went (8 Tau Ceti tasks, Qwen3.8 27B on a 3090) | Share of time |
|---|---|
| Local tool calls on the CPU (Lean builds, checks, file reads) | 5–20% per task, median 10% |
| Waiting for the model (GPU, plus network and queueing) | 80–95% per task, median 90% |

We can either set up a new architecture where distributed nodes run opencode and control the GPU
processing, bundle the agentic flow and the LLM on the existing nodes, or run the agentic flow on
the server side, since it's quite a small share of the work anyway.

My hunch is that running the agentic flow on the node itself, without the node operator actually
controlling it, opens up a ton of security vulnerabilities that I am not sure are easy to contain.
The same could be true on a server, but on a server it should in theory be easier to isolate.

## Latest update on software and cost

Currently setting up v0.2.0, which contains some small fixes here and there. There are no big
changes in the software or architecture; most time has been spent experimenting and testing. This
also means I have been racking up some infrastructure costs:

| 11–18 October | Cost |
|---|---:|
| Vast.ai: the 3090 production node, all week | $36 |
| Vast.ai: about 13 short test rentals, including today's 16 GB tests | $16 |
| AWS: gateway server, staging, IP address, disk (estimate) | about $5 |
| **Total** | **about $57** |

## Big limitations

One big limitation I have been thinking about lately is the requirement of x090 GPUs from Nvidia.
From about $1,200 for a used 3090 to $4,000 and more for a 5090 is really not something ordinary
people usually invest in. 5090s are also hard to find, since AI demand for memory is squeezing
GeForce supply, which makes the case for these GPUs worse. How can we use less capable
GPUs in AIs@home? I didn't know if they would be useful, since we need that 24 GB of VRAM to run
Qwen at 4 bits, so I have started testing: a 16 GB RTX 4080 runs Qwen3.8 27B at 3–4 bits almost as fast as a 4090.
Agentic work on 16 GB cards is still untested.

