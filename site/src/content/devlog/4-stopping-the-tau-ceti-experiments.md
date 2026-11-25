---
title: "Stopping the Tau Ceti experiments"
number: 4
date: 2026-11-13
summary: "28 of Qwen's 34 Tau Ceti pull requests were merged before the maintainers asked us to stop: what they were, what they cost, and prove2me and a Windows node as next steps."
publish: true
---

For the past couple of weeks I have been trying quite intensively to investigate how AIs@home could contribute to one of the largest efforts in formalising maths: [Tau Ceti](https://github.com/TauCetiProject/TauCeti).

I did a previous write-up [here](https://aisathome.org/devlog/3-ais-at-home-contributions-to-tau-ceti/), where I showed that at least some of it was possible. I continued to develop the harness and the scaffolding needed to make Qwen able to contribute. That included setting up a new concept called a "runner" in the cloud. The runner runs opencode in a sandbox on an AWS instance, so that it can both be scaled and be detached from the manual/operator mode of computation. Selection of tasks was still manual with the help of a frontier model and based on the roadmap in the Tau Ceti repository.

Sadly I got asked to stop the contributions, since the maintainers are not confident in Qwen's abilities to contribute there. Even though I felt that 28 of 34 pull requests getting through both the CI pipeline and the code review by the frontier models was enough evidence that the model can contribute.

## What got contributed
28 of the 34 pull requests Qwen opened have been merged ([all of them here](https://github.com/TauCetiProject/TauCeti/pulls?q=is%3Apr+author%3Aaisathome-agent)). Most of it is housekeeping: moving things into the right Mathlib namespaces, proving a repeated lemma only once, and cleaning up docs. Two of them are new mathematics: [reachability and communication for kernels](https://github.com/TauCetiProject/TauCeti/pull/21266) and [the positive-multiple saturation of an additive submonoid](https://github.com/TauCetiProject/TauCeti/pull/24775). The new lemmas cost 10-30x more tokens than the housekeeping tasks.

| Type of task | Merged |
|---|---|
| Refactor | 16 |
| Docs | 8 |
| New lemma or definition | 2 |
| Fix | 1 |
| Chore | 1 |
| **Total** | **28** |

In total that is 1,140 lines added and 821 removed across 113 files. It took 91 attempts through the harness with opencode, 58.5 GPU hours and about 8M tokens per merged pull request, and 68% were approved at the first review ([live numbers](https://aisathome.org/science/tauceti/)).

## Funding

Tau Ceti at the same time posted that they received [an inference grant from Anthropic](https://taucetiproject.org/blog/2026-10-30-an-anthropic-grant-for-tau-ceti/), so it makes sense that they are not really that cost or compute sensitive right now. They want high quality and they have the funds to do it.

Right now tokens and inference are heavily subsidised by the megacorps especially through the subscriptions. I do wonder if we are heading into a world of centralisation of power to the few, or abundance to the many. Will we be left with a few AI companies that own the total value chain? Or will the wealth be distributed to the many?

## What's next

I am looking at other cases right now.

[prove2me](https://prove2.me/), which has many similarities to Tau Ceti, although they do not care so much about code quality, but rather the proof itself. I have some early results which can be seen [here](https://aisathome.org/science/prove2me/) that indicate it is possible. However it seems quite difficult getting the harness to work as expected for this objective instead.
I have also been working on a Windows node. Currently at the point where I am testing it.

There is also the option of forking the Tau Ceti project, since most of it is open source, although the scale and cost of running it would only be possible with a large and robust setup in AIs@home. We would have to handle CPU nodes as well, since CI pipelines, opencode agents and Lean compilation require a good chunk of CPU power.
Currently working on open sourcing the harness/scaffolding for contributions to science initiatives like this, at the same time as I am using prove2me as the case for verifying instead of Tau Ceti.


I have spent quite a lot of hours and cash myself already: roughly 120 hours over the last five weeks (counted from my coding sessions, so probably a bit more in reality), and roughly 400 USD so far for the GPU on Vast.ai, AWS and AI subscriptions.
If you have a node and would be interested in contributing it, please get in touch, especially if you have an Nvidia GPU with 12 GB of VRAM. I have had some people reach out with support, but generally they have hardware which I haven't gotten around to supporting yet.
Right now I am spending about 5-10 USD a day on Vast.ai and AWS already. I could do that in Gridcoin instead, if anyone is interested?

