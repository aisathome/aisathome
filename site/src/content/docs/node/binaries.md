---
title: About the binaries
description: What is in an AIs@home release, how it is built, how to check it, and under which licences.
---

Every release on [GitHub](https://github.com/aisathome/aisathome/releases) is built by
GitHub Actions from the tagged source, in public, by the workflow
`.github/workflows/release.yml`. Nothing is built on anyone's own computer.

## What a release contains

| File | What |
|---|---|
| `aisathome-node-bundle-linux-amd64.tar.xz` | Everything a Linux volunteer needs (about 0.5 GB): the installer, the node agent and `llama-server` with its libraries. See [Install on Linux](/node/install/) |
| `AIsAtHome-node-setup-<version>.exe` | The Windows node (one installer): the node agent as a service, its tray icon and upstream llama.cpp's Windows CUDA 12.4 `llama-server`. Not code-signed yet, so Windows may warn before it runs. See [Install on Windows](/node/windows/) |
| `AIsAtHome-node-setup.exe` | The same installer, byte for byte, without the version in its name, so that `releases/latest/download/AIsAtHome-node-setup.exe` always gets the newest (the site's download button). From v0.6.0 |
| `aisathome-node-linux-amd64`, `-linux-arm64`, `-windows-amd64.exe` | The node agent alone, for people who build llama.cpp themselves |
| `aisathome-gateway-linux-amd64`, `-linux-arm64` | The gateway, for people who run their own |
| `SHA256SUMS` | The SHA-256 of every file above |

## How the bundle is built

- **The node agent and the gateway** are Go programs built with `CGO_ENABLED=0` (static
  binaries) by `scripts/build-release.sh`. `aisathome-node -version` prints the release.
- **`llama-server`** is [llama.cpp](https://github.com/ggml-org/llama.cpp) release **b12342**, pinned
  to commit `0fb8f157702702f33f4e018de8701b2569a1bc28`: measured on our agent workload before the
  switch (with and without speculative decoding).
  The build stops if the tag ever points elsewhere. `scripts/build-llama.sh` builds it inside
  NVIDIA's own image `nvidia/cuda:12.8.1-devel-ubuntu22.04`, pinned by its digest, for the
  RTX 30 (`sm_86`), RTX 40 (`sm_89`) and RTX 50 (`sm_120`) series.
- **No PTX** is included, only code for those three GPU generations. That is what lets the
  CUDA 12.8 build run on older drivers (NVIDIA's minor version compatibility); other GPUs
  need the [build-it-yourself](/node/install/#build-it-yourself) path.
- The bundle carries only the libraries `llama-server` actually loads (checked with `ldd`
  during the build, which fails on anything unexpected) and a `BUILD-INFO` file naming the
  llama.cpp tag and commit, the CUDA version and the GPU architectures.

## Requirements and why

| | Needed | Why |
|---|---|---|
| GPU | RTX 30, 40 or 50 series with 16 GB or more (16 GB: at most a light desktop on the card) | The model and its context need about 14 GB (`UD-Q3_K_XL`, 80k, 16 GB cards) or 18 GB (`UD-IQ4_XS`, 192k, 24 GB cards); the build targets these three generations |
| NVIDIA driver | 525.60.13 or newer; **570.26 or newer for RTX 50** | CUDA 12.x programs run on any 12.x driver from 525 on; the RTX 50 series needs CUDA 12.8's driver |
| Linux | x86_64 with AVX2, systemd, glibc 2.35 or newer (Ubuntu 22.04, Debian 12 or newer) | The build image is Ubuntu 22.04 |
| Windows | Windows 10 or 11, 64-bit; NVIDIA driver 527.41 or newer, **570.65 or newer for RTX 50** | The Windows node runs upstream llama.cpp's Windows CUDA 12.4 build |
| Disk | about 17 GB free in `/var/lib` (16 GB cards), about 18 GB (24 GB cards) | The model is 13.1 GB (16 GB cards) or 14.3 GB; on 24 GB cards the Linux installer adds MTP's 1.4 GB draft model |
| MTP (24 GB cards, Linux) | 21,000 MiB free on the card | The draft model's room; without it the node runs without MTP (the model check passes either way). See [model settings](/node/presets/#speculative-decoding-mtp) |

The installer checks all of this before it changes anything (`./install.sh --dry-run`).

## Checking a download

```bash
sha256sum -c --ignore-missing SHA256SUMS
```

This catches a corrupted or incomplete download. Be clear about its limit: `SHA256SUMS`
comes from the same release page, so it does **not** protect against someone who takes over
the release itself. Signed releases and build provenance are planned, not done yet. Until
then, if you want certainty, [build it yourself](/node/install/#build-it-yourself) from the
tagged source; the installer accepts your own llama.cpp build.

The model is downloaded by the installer from Hugging Face, pinned to one exact file and
revision, and checked against its SHA-256 before use.

**The one-line install.** `curl -fsSL https://aisathome.org/node.sh | sh` runs a short script
served by this site ([source](https://github.com/aisathome/aisathome/blob/main/site/public/node.sh)).
It downloads the bundle and `SHA256SUMS` from the latest GitHub release, checks the bundle against
them, runs the installer's checks without changing anything, and asks at your terminal before it
installs. Piping it to `sh` means trusting this site as well as the release. To read everything
before it runs, use the [steps by hand](/node/install/#prebuilt-bundle).

## Licences

| Part | Licence |
|---|---|
| AIs@home's own code (installer, node agent, gateway) | Apache License 2.0 |
| llama.cpp (`llama-server` and its libraries) | MIT, plus the licences in `licenses/llama.cpp/` |
| NVIDIA CUDA runtime and cuBLAS (`libcudart`, `libcublas`, `libcublasLt`) | NVIDIA's CUDA Toolkit EULA, which allows shipping these files unmodified with an application. They are NVIDIA's software, not covered by our licence |
| GNU OpenMP runtime (`libgomp`) | GPL-3.0 with the GCC Runtime Library Exception |

Details are in `THIRD_PARTY_NOTICES.md` inside the bundle.

## What has been tested

Releases are tested before we publish them, and we say plainly what was not.

Before the first bundle, llama.cpp built by hand:

- **RTX 4090, Ubuntu 22.04.5 (desktop), driver 530.30.02:** served as a node from 2026-10-09 to
  2026-10-13.
- **RTX 3090, Ubuntu 22.04 (rented), driver 535:** install and one chat through the gateway,
  2026-10-10, about 42 tokens/s.

A test build of the first bundle (2026-10-11):

- **RTX 3090, driver 535.183.01, Ubuntu 22.04:** it failed at first: llama.cpp compresses its CUDA
  kernels by default, and drivers before 570 cannot load them. The build now leaves them
  uncompressed (a larger bundle). With that, the full path worked: install, an interrupted
  model download resumed, the node connected, 1 × 128k in 18,302 MiB, about 42 tokens/s.
- **RTX 4090 and RTX 5090, driver 595:** the bundle's `llama-server` ran without problems, in
  containers, not through the installer; it was used for the model-check measurement.
- **RTX 3090, driver 575.51.03, Ubuntu 22.04.5 (2026-10-18):** the installer's GPU rule did not
  match this driver, so `llama-server` ran on the CPU and the install stopped. With the rule the
  installer now adds, set by hand: install, connect, requests and a cancel through the gateway, the
  local page, uninstall; about 41 tokens/s.

Later releases, each built by GitHub Actions:

- **v0.3.0 (2026-10-24):** the release package on an RTX 3090 (driver 535) and an RTX 4090.
- **v0.4.0 (2026-11-11):** the 16 GB tier's model file and settings were measured with
  `llama-server` on an RTX 5070 Ti (2026-10-31: `UD-Q3_K_XL` at 80k ran with 1.5 GB held by other
  programs, about 30 tokens/s after a 32k prompt); the installer has not yet run the 16 GB tier on
  a real card.
- **v0.5.0 (2026-11-14):** the Windows installer (a pre-release build) on an RTX 4090: on the
  network from 2026-11-13, 151 requests, none failed, about 40-46 tokens/s.
- **v0.7.0 (2026-11-25):** tested on an RTX 3090 and an RTX 4090; on 24 GB cards the model check
  passes with MTP on. RTX 50 cards were not tested on a GPU for this release.
- **Not tested yet:** drivers between 525 and 535; the 16 GB tier through the installer; RTX 30
  and 50 cards and the 16 GB tier on Windows.
