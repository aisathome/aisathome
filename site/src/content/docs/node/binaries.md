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
| `aisathome-node-linux-amd64`, `-linux-arm64`, `-windows-amd64.exe` | The node agent alone, for people who build llama.cpp themselves |
| `aisathome-gateway-linux-amd64`, `-linux-arm64` | The gateway, for people who run their own |
| `SHA256SUMS` | The SHA-256 of every file above |

## How the bundle is built

- **The node agent and the gateway** are Go programs built with `CGO_ENABLED=0` (static
  binaries) by `scripts/build-release.sh`. `aisathome-node -version` prints the release.
- **`llama-server`** is [llama.cpp](https://github.com/ggml-org/llama.cpp) **v0.5.0**, pinned to
  commit `7fe450e19305b828c199d602c23a8337aaa1f03b`: the version our benchmarks measured.
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
| GPU | RTX 3090, 4090 or 5090 class, 24 GB | The model and its context need about 18 GB; the build targets these three generations |
| NVIDIA driver | 525.60.13 or newer; **570.26 or newer for RTX 50** | CUDA 12.x programs run on any 12.x driver from 525 on; the RTX 50 series needs CUDA 12.8's driver |
| Linux | x86_64 with AVX2, systemd, glibc 2.35 or newer (Ubuntu 22.04, Debian 12 or newer) | The build image is Ubuntu 22.04 |
| Disk | about 20 GB free in `/var/lib` | The model is 16.5 GB |

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

## Licences

| Part | Licence |
|---|---|
| AIs@home's own code (installer, node agent, gateway) | Apache License 2.0 |
| llama.cpp (`llama-server` and its libraries) | MIT, plus the licences in `licenses/llama.cpp/` |
| NVIDIA CUDA runtime and cuBLAS (`libcudart`, `libcublas`, `libcublasLt`) | NVIDIA's CUDA Toolkit EULA, which allows shipping these files unmodified with an application. They are NVIDIA's software, not covered by our licence |
| GNU OpenMP runtime (`libgomp`) | GPL-3.0 with the GCC Runtime Library Exception |

Details are in `THIRD_PARTY_NOTICES.md` inside the bundle.

## What has been tested

Releases are tested before we publish them, and we say plainly what was not. The first
bundle was built and checked on a machine with an RTX 4090, and is being tested on rented
RTX 3090, 4090 and 5090 machines (including an older driver, 535) before its first release.
