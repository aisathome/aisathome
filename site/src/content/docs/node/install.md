---
title: Install on Linux
description: Install an AIs@home node on Linux with systemd, step by step.
---

Tested on a fresh Ubuntu 22.04 machine with an RTX 3090 (driver 535) and on an RTX 4090.

## 1. Build tools and llama.cpp

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

## 2. The model

```bash
curl -L -o Qwen3.8-27B-UD-Q4_K_M.gguf \
  https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/main/Qwen3.8-27B-UD-Q4_K_M.gguf
```

16.5 GB. The installer **moves** it to `/var/lib/aisathome-node/models/model.gguf`.

## 3. The node agent and the installer

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

## 4. Config

Create `node.json` with the node id and gateway you got with your token:

```json
{
  "node_id": "your-node-name",
  "gateway_url": "wss://api.aisathome.org/v1/node/connect",
  "upstream": "http://127.0.0.1:8000",
  "models": [{ "name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 2, "ctx": 32768 }]
}
```

Don’t put the token in the file: the installer asks for it.

## 5. Install

```bash
sudo ./node/packaging/linux/install.sh \
  --llama-build ../llama.cpp/build --model ../Qwen3.8-27B-UD-Q4_K_M.gguf \
  --node-bin ./aisathome-node --node-config node.json
```

Paste the node token when asked (input is hidden). The installer creates the `aisathome`
user, the two services, a watchdog that restarts a hung `llama-server`, and a hook that
stops the node around suspend. It waits until the model is loaded.

Add `--dry-run` to see the `llama-server` command without installing, `--preset` to pick a
[preset](/node/presets/), and `--enable-at-boot` to start the node at boot.

## 6. Check

```bash
journalctl -u aisathome-node -n 5
```

You should see `connected to gateway`. Then tell us, and we check that requests reach you.
