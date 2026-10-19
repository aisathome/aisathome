---
title: "AIs@home: Bringing AI into the BOINC/Gridcoin environment?"
number: 1
date: 2026-10-12
summary: "The first public post: a proof of concept for running open AI models on volunteers’ GPUs, its limits, and a request for feedback from the BOINC/Gridcoin community."
publish: true
---

Hi,

I have been contributing to BOINC/Folding@home on and off for many years now. Lately I have been wondering why there are not any AI use cases (Axiom was one?). Right now the megacorps are driving the deployment of AI and gobble up all of the hardware. I have built a PoC of how AI could be deployed at home. Open-weight/open-source models are getting quite powerful right now, and the model implemented can be used as a daily driver instead of ChatGPT/Claude. I think BOINC is a fit, but it's quite a lot of work to integrate with that environment. It also doesn't work very well for streaming/inference: small jobs, streaming jobs, burst load.

The code is open source here: https://github.com/aisathome/aisathome, built heavily by AI, I will admit that, even though I am a developer originally. https://aisathome.org/

Right now I am looking for some feedback from the community.

- Downloading and setting up a node can be done here: https://aisathome.org/node/

- Testing the chat interface can be done here by signing up: https://aisathome.org/account (the chat interface could also be run by yourself with API keys)

- Testing agentic AI (opencode) can be done by following this short guide: https://aisathome.org/use/api/

Current functionality:

- Qwen3.8 27B as the model, 128k context
- A ChatGPT-like chat interface
- API keys for using opencode locally
- There are some BOINC format statistics
- Nodes can be run by the volunteers

Limitations/issues you should be aware of:

- Privacy: There is no way to completely ensure that nodes can't read your prompts. I have put in some reasonable guardrails, like not storing logs/prompts. Only use this for stuff that is not private: no passwords, secrets etc. For example open source projects.
- At this point I have only tested some specific hardware configurations. In general that is high-end GPUs: 3090, 3090 Ti, 4090, 5090. It has to be Linux.
- Attack vector from the node: if you have auto mode enabled in opencode (given it all permissions to execute whatever), then a node could send you malicious commands that are executed, Qwen is probably aligned enough to not do this, but a malicious node can. I strongly advise to run opencode in a sandboxed environment (Docker/dedicated VM) without auto mode.

Future functionality, if there is interest in this type of project in the BOINC/Gridcoin community:

Better integration/testing with the BOINC client, so that you can run the BOINC client and the AIs@home client at the same time, or even just the original BOINC client? Idle time can be donated to research. Credit and payment of GRC as a means to pay for/compensate node operators. I guess this project would not be a candidate for whitelisting. Other AI inference: image recognition/generation, audio/voice.

How does this benefit science and BOINC?

Which I guess is important for you and me. Right now AI is driving an explosion in hardware deployment, usage is bursty. BOINC should be able to serve AI at home and use the idle time for other research projects. An alternative to the megacorps owning the full value chain of AI.

Is this project of interest? Why or why not? If you can give some feedback on what would matter, that would help.
