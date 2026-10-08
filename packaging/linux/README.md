# AIs@home node on Linux (systemd)

Runs `llama-server` and `aisathome-node` as the unprivileged system user `aisathome`,
sandboxed by systemd: no access to `/home`, a read-only system, no new privileges.
`llama-server` listens on 127.0.0.1 only, and the node only connects outward to the
gateway, so no inbound ports or router changes are needed.

**Hardened, not confidential:** whoever controls this machine can see the prompts it
serves.

## Requirements

- NVIDIA GPU with ~20 GB free VRAM for the default 27B model, plus the NVIDIA driver with
  CUDA (`nvidia-smi` works).
- `llama-server` built with CUDA, e.g.:
  ```bash
  git clone --depth 1 --branch v0.5.0 https://github.com/ggml-org/llama.cpp && cd llama.cpp
  cmake -B build -G Ninja -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=89 -DCMAKE_BUILD_TYPE=Release
  cmake --build build --target llama-server
  ```
- A GGUF model, e.g. `unsloth/Qwen3.8-27B-GGUF` / `Qwen3.8-27B-UD-Q4_K_M.gguf` (16.5 GB).
- `aisathome-node` (`go build ./cmd/aisathome-node`), a `node.json` (see
  `examples/node.json`) and a node token from the gateway operator.

## Install

```bash
sudo ./install.sh --llama-build llama.cpp/build --model Qwen3.8-27B-UD-Q4_K_M.gguf \
     --node-bin ./aisathome-node --node-config node.json
```

- The token is read from stdin (hidden), never from the command line.
- The model file is **moved** to `/var/lib/aisathome-node/models/`.
- Defaults: 2 slots, 32k context in total. Override with `SLOTS=4 CTX=65536 sudo -E ...`.
- The node does not start at boot unless you pass `--enable-at-boot`.

## Day to day

| | |
|---|---|
| Pause and free the GPU | `sudo systemctl stop aisathome-node aisathome-llama` |
| Resume | `sudo systemctl start aisathome-llama aisathome-node` |
| Logs | `journalctl -u aisathome-node -u aisathome-llama -f` |
| Remove | `sudo ./uninstall.sh` (keeps the model; `--purge` removes it too) |
