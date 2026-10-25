---
title: "AIs@home's contributions to Tau Ceti"
number: 3
date: 2026-10-25
summary: "Five of our twelve Tau Ceti pull requests are merged: what a refactor and a new lemma cost in tokens and GPU time, why the runner stays on the server, and prove2me as a next case."
publish: true
---

## Contributions

AIs@home has made some real contributions to the [Tau Ceti](https://github.com/TauCetiProject/TauCeti) repository. They range from simple docstring changes to refactoring and to new proofs. Simple fixes, such as docs and lint refactoring, fit quite well as a use case for Qwen3.8 27B running at home.
Generating new mathematics and writing new proofs from scratch is more difficult. Not impossible, but more difficult.

At least this shows that an open model on a consumer GPU (for now rented RTX 3090s) can make contributions that get merged.

Some examples: [this refactor](https://github.com/TauCetiProject/TauCeti/pull/19779) cost about 2.0M tokens and 23 GPU minutes, while [a new lemma](https://github.com/TauCetiProject/TauCeti/pull/19781) has cost 21.1M tokens and about 5.5 GPU hours so far (most of those tokens are the model re-reading its context from cache). That is quite a large difference. As for Tau Ceti itself, the potential is huge, so it will not run out of problems to solve any time soon.

As of 25 October, 5 of the 12 pull requests we have opened have been merged, and 5 of the 12 were approved at the first review ([live numbers](https://aisathome.org/science/tauceti/)):

| Type of task | Opened | Merged | Approved at first review |
|---|---|---|---|
| Namespace move | 5 | 2 | 3 |
| Lint fix | 1 | 1 | 1 |
| Docstring cleanup | 2 | 1 | 1 |
| Generalisation | 2 | 1 | 0 |
| New lemma | 2 | 0 | 0 |
| **Total** | **12** | **5** | **5** |

| Pull request | Type | Qwen tokens | GPU time |
|---|---|---|---|
| [#19779](https://github.com/TauCetiProject/TauCeti/pull/19779) | Namespace move (merged) | 2.0M | 23 min |
| [#19781](https://github.com/TauCetiProject/TauCeti/pull/19781) | New lemma (still open) | 21.1M | 5.5 h |

It still requires quite a lot of babysitting, so scaling up doesn't make much sense yet.

There are also some problems on the Tau Ceti side. The project hasn't quite handled the scaling yet: CI pipelines crashing and running out of resources, and reviews breaking halfway. Three of our pull requests were stopped because the reviews they got were not valid (a reviewer's sandbox failed, or outdated rubrics were used), not because of the code. With the huge number of contributions at the moment, I guess that will continue for quite some time. Frontier models seem to be able to produce pull requests a lot faster.

## Architecture

Some architectural changes were needed to contribute to science like this. We want one node to run the opencode/harness session and another node to supply the GPU inference. Measuring where the time goes, I see roughly 80/20: about 80% on the GPU node (the model) and 20% on the CPU/runner node (builds and tools). So the bottleneck is clearly on the GPU side. There is potential for distributing the CPU nodes that run the opencode harness. However, as discussed earlier, that carries real security risks: the runner executes the commands the model writes, so running it on a volunteer's machine would mean running code from a remote model there, a form of remote code execution (RCE), which is what many attacks aim for. So far I have therefore decided to keep the runner on the server side, where we also isolate it: it runs in a sandbox, can only run specific programs, reaches nothing on the internet except our gateway, and holds no tokens that can write to GitHub. Running the runner on a volunteer's machine could come later, once we have sufficient isolation and security practices in place. For now, since the CPU part is small, it's not a big issue.

## Other initiatives

There are other interesting use cases for AI inference besides Tau Ceti.
[prove2me](https://prove2.me/) is an initiative that focuses less on scalability, code quality or reusability, and more on building its own database of proofs. I think this might be an even better fit for AIs@home. As of 25 October there are 2,378 open missions there, and 3,699 are closed. We are exploring it now, first with a small local test where nothing is submitted, and I also wonder what the synergies with Tau Ceti are.
