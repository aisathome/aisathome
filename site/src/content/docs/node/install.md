---
title: Install on Linux
description: Install an AIs@home node on Linux with systemd, step by step.
---

There are two ways to install: the **prebuilt bundle** (no compiler, no CUDA toolkit, only
the NVIDIA driver) or **building llama.cpp yourself**. Both end with the same services.

## Prebuilt bundle

You need an RTX 30, 40 or 50 series card with 24 GB, the NVIDIA driver 525.60.13 or newer
(570.26 or newer for an RTX 50), an x86_64 Linux with systemd and glibc 2.35 or newer
(Ubuntu 22.04, Debian 12 or newer), and about 20 GB of free disk space in `/var/lib`.

### 1. Download and check

```bash
base=https://github.com/aisathome/aisathome/releases/latest/download
curl -LO $base/aisathome-node-bundle-linux-amd64.tar.xz -LO $base/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
```

The bundle (about 0.5 GB) holds the installer, the node agent and `llama-server` with
llama.cpp v0.5.0, built with CUDA 12.8 for the RTX 30, 40 and 50 series, plus the NVIDIA
CUDA runtime and cuBLAS libraries it needs (see `THIRD_PARTY_NOTICES.md`). If the latest
release has no bundle yet, use [build it yourself](#build-it-yourself).

### 2. Check, then install

```bash
./install.sh --dry-run                 # checks driver, GPU and disk; installs nothing
sudo ./install.sh --preset balanced
```

Paste the node token when asked (input is hidden). A token from your account page carries
the node's id; if we gave you a node id with your token, add `--node-id <id>`.

The installer then:

- checks the NVIDIA driver, the GPU and the free disk space, and stops with a clear message
  if one is not right;
- downloads the model (Qwen3.8 27B `UD-Q4_K_M`, 16.5 GB) from Hugging Face, pinned to one
  exact file, and checks its SHA-256. An interrupted download resumes when you run the
  installer again;
- writes `node.json` for the [preset](/node/presets/) and the gateway
  `wss://api.aisathome.org/v1/node/connect` (`--gateway-url` for another);
- creates the `aisathome` user, the two services, a watchdog that restarts a hung
  `llama-server`, and a hook that stops the node around suspend, and waits until the model
  is loaded.

Add `--enable-at-boot` to start the node at boot. On a slow connection, download the model
first without root: `./install.sh --download-only ~/models`, then install with
`--model ~/models/Qwen3.8-27B-UD-Q4_K_M.gguf` (the file is moved).

### 3. Check

```bash
journalctl -u aisathome-node -n 5
```

You should see `connected to gateway`. Then tell us, and we check that requests reach you.

## Build it yourself

For other GPUs, other distributions, or if you prefer to compile. Tested on a fresh Ubuntu
22.04 machine with an RTX 3090 (driver 535) and on an RTX 4090.

### 1. Build tools and llama.cpp

`llama-server` must be built with CUDA. You need the CUDA toolkit that matches your driver
(`nvidia-smi` shows the highest CUDA version it supports), `cmake`, `ninja` and a C++
compiler. On Ubuntu 22.04 with driver 535:

```bash
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb && sudo apt-get update
sudo apt-get install -y cuda-toolkit-12-2 cmake ninja-build build-essential git
export PATH=/usr/local/cuda/bin:$PATH   # nvcc is not on PATH by default

git clone --depth 1 --branch v0.5.0 https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -G Ninja -DGGML_CUDA=ON "-DCMAKE_CUDA_ARCHITECTURES=86;89" -DCMAKE_BUILD_TYPE=Release
cmake --build build --target llama-server
cd ..
```

`86;89` covers the RTX 30xx and 40xx series. A build for one of them only fails on the
other with `no kernel image is available for execution on the device`.

### 2. The node agent and the installer

```bash
git clone https://github.com/aisathome/aisathome && cd aisathome
```

Until the first release is published, build the agent yourself. It needs Go 1.27 or newer
from [go.dev/dl](https://go.dev/dl/) (distribution packages are usually older):

```bash
go build -o aisathome-node ./node/cmd/aisathome-node
```

Once releases exist, `--node-release latest` downloads it instead and checks it against the
release’s `SHA256SUMS`.

### 3. Install

```bash
sudo ./node/packaging/linux/install.sh --llama-build ../llama.cpp/build \
  --node-bin ./aisathome-node
```

As with the bundle, the installer downloads and checks the model, writes `node.json` and
asks for the token. To use a model file you already have, add `--model <file.gguf>` (it is
**moved** to `/var/lib/aisathome-node/models/model.gguf`). To write `node.json` yourself,
pass `--node-config node.json`:

```json
{
  "node_id": "your-node-name",
  "gateway_url": "wss://api.aisathome.org/v1/node/connect",
  "upstream": "http://127.0.0.1:8000",
  "models": [{ "name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 2, "ctx": 32768 }]
}
```

Don’t put the token in the file: the installer asks for it. Add `--dry-run` to see the
`llama-server` command without installing.
