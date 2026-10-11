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
- `llama-server` built with CUDA. You need the CUDA toolkit matching your driver
  (`nvidia-smi` shows the highest CUDA version it supports), `cmake`, `ninja` and a C++
  compiler. On Ubuntu 22.04 with driver 535, for example:
  ```bash
  wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/cuda-keyring_1.1-1_all.deb
  sudo dpkg -i cuda-keyring_1.1-1_all.deb && sudo apt-get update
  sudo apt-get install -y cuda-toolkit-12-2 cmake ninja-build build-essential
  export PATH=/usr/local/cuda/bin:$PATH   # nvcc is not on PATH by default

  git clone --depth 1 --branch v0.5.0 https://github.com/ggml-org/llama.cpp && cd llama.cpp
  cmake -B build -G Ninja -DGGML_CUDA=ON "-DCMAKE_CUDA_ARCHITECTURES=86;89" -DCMAKE_BUILD_TYPE=Release
  cmake --build build --target llama-server
  ```
  `86;89` covers the RTX 30xx and 40xx series; a build for 89 only fails on an RTX 3090
  with "no kernel image is available". (`native` needs CMake 3.24; Ubuntu 22.04 has 3.22.)
- A GGUF model, e.g. `unsloth/Qwen3.8-27B-GGUF` / `Qwen3.8-27B-UD-Q4_K_M.gguf` (16.5 GB).
- `aisathome-node`: either a prebuilt release binary (`--node-release`, linux amd64 or
  arm64) or your own build (`go build ./node/cmd/aisathome-node`, then `--node-bin`).
- A `node.json` (see `examples/node.json`) and a node token from the gateway operator.

## Install

```bash
sudo ./install.sh --llama-build llama.cpp/build --model Qwen3.8-27B-UD-Q4_K_M.gguf \
     --node-release latest --node-config node.json
```

- `--node-release latest` (or a tag such as `v0.1.0`) downloads `aisathome-node` for
  this machine's architecture from the
  [GitHub releases](https://github.com/aisathome/aisathome/releases) and checks it against
  that release's `SHA256SUMS`. This catches corrupt downloads, not a compromised release:
  the checksums come from the same place. Pin a tag to install a known version; `latest`
  skips pre-releases. `llama-server` is not part of the release; build it as above.
- `--node-bin ./aisathome-node` installs your own build instead.
- The token is read from stdin (hidden), never from the command line.
- The model file is **moved** to `/var/lib/aisathome-node/models/`.
- Defaults: the `balanced` preset (below), a thinking budget of 8,192 tokens, and
  reasoning from earlier turns is not kept in the prompt.
- The node does not start at boot unless you pass `--enable-at-boot`.
- `--dry-run` prints the resulting `llama-server` command and exits without installing
  (no root needed; with `--node-release` it downloads and verifies the binary too).
  `./install_test.sh` tests presets, overrides and the release download this way.

## Presets

`--preset <name>` (or `PRESET=<name>`) picks the slots, the context per slot and the KV
cache type. Every slot has its own context (`--kv-unified-per-slot`), so one long request
cannot starve another.

| Preset | Slots × context | KV cache | Measured on an RTX 4090 with Qwen3.8 27B `UD-Q4_K_M` |
|---|---|---|---|
| `balanced` (default) | 2 × 32k | q8_0 | 1 × 64k q8: 17,858 MiB VRAM; mean KLD against f16 0.0020; no needle loss up to 60k |
| `low-vram` | 2 × 32k | q4_0 | 1 × 64k q4: 16,834 MiB (1.0 GB less than q8); no needle loss up to 60k; mean KLD 0.0044 (2.16× q8) |
| `long-context` | 1 × 128k | q4_0 | 18,306 MiB; hard needles at 120k 118/120 (q8: 119/120 at 20,354 MiB); first 120k prompt 82 s; decode 46.6 tok/s short, 32.5 at 120k |
| `long-context-dual` | 2 × 128k | q4_0 | 21,272 MiB. **Head-of-line blocking:** while one ~100k prompt was prefilled (about 60 s), the other stream's decode fell to 3.9 tok/s |

Source: `bench/kvquant/RESULTS.md` in the research repo (llama.cpp v0.5.0, 2026-10-09).

Tested installs: an RTX 4090 (the owner's node) and a fresh RTX 3090 VM (Ubuntu 22.04,
driver 535, 2026-10-10): `balanced` used 17,972 MiB and decoded about 42 tokens/s for a
single 400-token answer measured through the gateway (one run).
Caveats:
- The 2 × 32k presets were not measured as such: the numbers are for one 64k slot, which
  has the same total context.
- The 128k rows were measured on a headless card. A desktop on the same GPU needs extra
  VRAM.
- q8 is the default because q4's mean KLD is 2.16× q8's, just over the 2× rule set before
  the benchmark. q4 showed no task loss, including at 120k.

Explicit variables override the preset: `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE` (`q8_0`, `q4_0`
or `f16`), `REASONING_BUDGET` (`-1` = unlimited), `CACHE_RAM_MIB` and `MODEL_ALIAS`, e.g.
`KV_TYPE=q8_0 sudo -E ./install.sh --preset long-context ...`.

## Privacy hardening

These measures make it less likely that prompts end up on disk by accident:

- **No core dumps** (`LimitCORE=0` on both services): a crash dump would contain prompts
  from memory.
- **No swap** (`MemorySwapMax=0` on both services, needs cgroup v2): prompts are never
  paged out to the swap device.
- **Smaller prompt cache in RAM** (`--cache-ram 2048` MiB instead of llama-server's 8192):
  bounds how much conversation data stays warm in memory. llama-server has a size limit,
  not a time limit. Override with `CACHE_RAM_MIB`, where `0` disables the cache (follow-up
  turns then need a full prefill more often).
- **No prompt logging:** llama-server runs without verbose logging and without
  `--log-prompts-dir`.

They do not make the node confidential. Anyone with root on this machine can still read
prompts from RAM or VRAM while they are being served.

## Day to day

| | |
|---|---|
| Status | `aisathome-node status` (exit code 0 when connected), or the local UI below |
| Pause (model stays loaded) | the local UI's **Pause** button |
| Pause and free the GPU | `sudo systemctl stop aisathome-node aisathome-llama` |
| Resume | `sudo systemctl start aisathome-llama aisathome-node` |
| Logs | `journalctl -u aisathome-node -u aisathome-llama -f` |
| Remove | `sudo ./uninstall.sh` (keeps the model; `--purge` removes it too) |

## Local UI

The agent serves a status page at **http://127.0.0.1:7381/** (on desktops there is also an
"AIs@home node" menu entry). It shows whether the node is connected, paused or serving, the
gateway, node id, model and preset, slots in use, the GPU (name, memory, temperature, through
`nvidia-smi`), requests and tokens served today and since install, and recent events
(connections, pauses, errors). It never shows prompts or answers. **Your nodes** links to
your account page.

**Controls** (pause, resume, restart the engine) need the local password, created by the
installer:

```bash
sudo cat /etc/aisathome-node/ui-password
```

Enter it once; the browser keeps a session cookie for 90 days or until the password
changes. To change it, write a new one (12 characters or more) to that file.

- **Pause** finishes requests in flight and takes no new ones; the model stays in GPU
  memory. The gateway is told at once. The pause survives restarts until you resume.
- **Restart engine** restarts `llama-server`, and the agent with it; requests in flight
  end. The agent has no privileges: it writes `/var/lib/aisathome-node/restart-engine`, and
  the root-owned `aisathome-node-restart.path` unit runs the restart. Without that unit the
  page shows the command to run instead: `sudo systemctl restart aisathome-llama`.

The UI listens on 127.0.0.1 only, so there is no port to open or to close. It answers only
to `127.0.0.1`, `localhost` or `[::1]` in the address bar, and its controls refuse requests
from other web pages. `UI_PORT=<port>` at install picks another port, `UI_PORT=off` turns the
UI off.

**Headless machines:** open an SSH tunnel from your own computer, then browse to
http://localhost:7381/ there:

```bash
ssh -N -L 7381:127.0.0.1:7381 you@node-machine
```

Any local port works on your side (`-L 8000:127.0.0.1:7381`, then http://localhost:8000/).

`aisathome-node status` prints the same status in a terminal (`-json` for the raw data). It
exits with 0 when the node is connected (also when paused), 1 when it is not, and 3 when no
agent answers.

Counters and events are kept in `/var/lib/aisathome-node/node-state.json`: counts and event
types only, no request content.
