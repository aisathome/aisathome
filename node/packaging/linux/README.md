# AIs@home node on Linux (systemd)

Runs `llama-server` and `aisathome-node` as the unprivileged system user `aisathome`,
sandboxed by systemd: no access to `/home`, a read-only system, no new privileges.
`llama-server` listens on 127.0.0.1 only, and the node only connects outward to the
gateway, so no inbound ports or router changes are needed.

**Hardened, not confidential:** whoever controls this machine can see the prompts it
serves.

## Install from the prebuilt bundle

Each release has `aisathome-node-bundle-linux-amd64.tar.xz`: this installer, `aisathome-node`
and `llama-server` with the libraries it needs. No compiler or CUDA toolkit, only the NVIDIA
driver.

```bash
base=https://github.com/aisathome/aisathome/releases/latest/download
curl -LO $base/aisathome-node-bundle-linux-amd64.tar.xz -LO $base/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
./install.sh --dry-run                 # checks only, no root needed
sudo ./install.sh    # then paste the node token
```

Requirements:

- x86_64 with AVX2, systemd, glibc 2.35 or newer (Ubuntu 22.04, Debian 12 or newer);
- an NVIDIA GPU with compute capability 8.6, 8.9 or 12.0 (RTX 30, 40, 50 series) with 16 GB
  or more, and enough of it free for its tier (below): 15,200 MiB on a 16 GB card (no screen
  on it), 18,500 MiB on 24 GB and more;
- a decode speed of at least 20 tokens/s after a 32,000-token prompt (measured at install);
- NVIDIA driver **525.60.13 or newer**, and **570.26 or newer** on an RTX 50;
- about 16.5 GB free in `/var/lib` (the 14.3 GB model plus 2 GB).

What is in it (`BUILD-INFO` has the exact values):

| | |
|---|---|
| llama.cpp | `v0.5.0`, commit `7fe450e19305b828c199d602c23a8337aaa1f03b`, the version the presets were benchmarked with |
| CUDA | 12.8.1 (`nvidia/cuda:12.8.1-devel-ubuntu22.04`, pinned by digest), real code for `86-real;89-real;120a-real`, no PTX |
| Libraries | llama.cpp's, `libcudart.so.12`, `libcublas.so.12`, `libcublasLt.so.12` (NVIDIA, unmodified), `libgomp.so.1`; see `THIRD_PARTY_NOTICES.md` |
| Size | 497 MB packed (xz), 927 MiB unpacked; cuBLAS is most of it (869 MB unpacked, 439 MB packed). Measured on a local build with the same CUDA 12.8.1 libraries, 2026-10-11 |

Why driver 525: a CUDA 12.x build runs on any CUDA 12 driver (525 or newer) through NVIDIA's
minor version compatibility, as long as it contains no PTX, which the build ensures. RTX 50
cards need the CUDA 12.8 driver (570.26) anyway. Sources: NVIDIA, "CUDA Compatibility",
minor version compatibility, table 1; CUDA toolkit release notes, table 3. The installer
checks the driver and every GPU with `nvidia-smi`, the CPU and the C library before anything
else, and stops with a message naming the fix.

CI builds the bundle (`.github/workflows/release.yml`, `scripts/build-llama.sh`, then
`scripts/build-release.sh` with `LLAMA_DIR`). A manual run of the release workflow is a dry
run that builds everything without publishing; its `dist` artifact (kept 7 days) holds the
bundle for a test install.

### The model

Without `--model`, the installer downloads `Qwen3.8-27B-UD-IQ4_XS.gguf` from
`unsloth/Qwen3.8-27B-GGUF`, pinned to commit `4ca720788d1e01f1bff70c033e0d0028fd02e502`, and
checks it against SHA-256 `40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199`
(14,252,845,984 bytes, the file the tiers were measured with; hash and size are the file's LFS
pointer at that commit on the Hugging Face Hub). It replaced `UD-Q4_K_M` (16.5 GB) in v0.3.0.
It goes straight to `/var/lib/aisathome-node/models/model.gguf`:

- an interrupted download resumes from `model.gguf.part` on the next run;
- a wrong checksum deletes the download, so the next run starts clean;
- an existing `model.gguf` with the right checksum is kept (checked, about a minute);
- `--download-only <dir>` only downloads and checks, without root, e.g. on a slow line
  before installing with `--model <dir>/Qwen3.8-27B-UD-IQ4_XS.gguf`;
- `--model` accepts only the pinned file (size, then SHA-256): the network never mixes model
  files, because the model check cannot tell `UD-IQ4_XS` from `UD-Q4_K_M` reliably;
- `MODEL_URL`, `MODEL_SHA256` and `MODEL_SIZE` (all three) choose another file.

### node.json

Without `--node-config`, the installer writes `node.json` with the gateway
(`--gateway-url`, default `wss://api.aisathome.org/v1/node/connect`), the tier's slots and
context, and the roles `deep` and `agent`. The node id comes from `--node-id`, else from a
token made on the account page (`nt-<node id>.<secret>`), else from the host name. Only a
token from a gateway's config needs `--node-id`. When the installer runs again on an
installed node, the node id and gateway come from the installed `node.json` unless
`--node-id` or `--gateway-url` is given.

## Install with your own llama.cpp build

For other GPUs, older systems, or if you prefer to compile.

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
- `aisathome-node`: either a prebuilt release binary (`--node-release`, linux amd64 or
  arm64) or your own build (`go build ./node/cmd/aisathome-node`, then `--node-bin`).
- Optional: a GGUF model you already have (`--model`) and your own `node.json`
  (`--node-config`, see `examples/node.json`).
- A node token: sign in at `https://aisathome.org/account`, "Your nodes", "Add a node". The
  token carries the node's id, so `node_id` (and `--node-id`) is needed only with tokens
  from a gateway's config.

```bash
sudo ./install.sh --llama-build llama.cpp/build --node-release latest
```

## Install options

- `--node-release latest` (or a tag such as `v0.1.0`) downloads `aisathome-node` for
  this machine's architecture from the
  [GitHub releases](https://github.com/aisathome/aisathome/releases) and checks it against
  that release's `SHA256SUMS`. This catches corrupt downloads, not a compromised release:
  the checksums come from the same place (the same holds for the bundle). Pin a tag to
  install a known version; `latest` skips pre-releases.
- `--node-bin ./aisathome-node` installs your own build instead.
- An explicit `--llama-build` wins over the bundle's `llama-server` and skips its checks.
- The node token is read from stdin (hidden), never from the command line. On an installed
  node, Enter on an empty line at the prompt keeps the installed token, and `--keep-token`
  keeps it without asking (for scripts; empty piped input is still an error).
- A file given with `--model` is **moved** to `/var/lib/aisathome-node/models/`.
- Defaults: the tier for the card (below), a thinking budget of 8,192 tokens, and
  reasoning from earlier turns is not kept in the prompt.
- The node does not start at boot unless you pass `--enable-at-boot`.
- `--dry-run` runs the checks (bundle, driver, GPU, the tier's free memory, disk), prints the generated `node.json`,
  the `llama-server` command and its systemd unit, and exits without installing (no root
  needed; with `--node-release` it downloads and verifies the binary too).
  `./install_test.sh` tests the tiers, overrides, the checks (with a fake `nvidia-smi`,
  `systemctl` and `df`), the speed test (against a fake `llama-server`), the downloads and the units' GPU device rules (against `/proc/devices` as drivers
  530 and 575 list it) this way; `../../../scripts/bundle_test.sh` tests the bundle's
  packaging.

## Model settings

Every node runs the same model file, one request at a time, with a context set by the card's
size only (two tiers). The installer picks the tier; there is nothing to choose.

| Tier | Cards (nvidia-smi total) | Context | KV | Free GPU memory needed before start |
|---|---|---|---|---|
| standard | 15,500 to 22,999 MiB (16 GB class) | 1 × 81,920 | `q4_0` | 15,200 MiB: no screen on the card, or under about 650 MB of desktop |
| big | 23,000 MiB and more (24 GB+) | 1 × 196,608 | `q4_0` | 18,500 MiB |

- **It must fit; it is never shrunk.** The installer reads the card's free memory (the memory
  of this node's own running `llama-server` counts as free on an update) and stops with a
  message if the tier does not fit. If `llama-server` still runs out of memory at start, it
  stops and disables the node with the same advice. With several GPUs, the largest card is
  used and `llama-server` is pinned to it (`CUDA_VISIBLE_DEVICES=<uuid>`).
- **Speed test.** After the model has loaded and before the agent connects, the installer
  sends a 32,000-token prompt to `llama-server` and reads its decode speed for 256 tokens. Below
  20 tokens/s it stops and disables the node. A short prompt would misrank cards: an RTX 5070
  Ti writes 47 tokens/s at 200 tokens but 29 at 32k and 17 at 78k with `q4_0` KV.
- **Measured** with `UD-IQ4_XS` and llama.cpp v0.5.0 (2026-10-22/23): standard on an RTX 5070 Ti,
  15,070 MiB after load and 15,098 at the peak of a 77k prompt, loads with 640 MiB held by a
  simulated display and fails at start with 768 MiB; big: 256k on an RTX 3090 used 19,202 MiB
  at the peak, hard needles 77/80 at ~195k and 62/80 at ~247k (hence 192k). `q8_0` KV does not
  fit 80k in 16 GB.

For testing only, `SLOTS`, `CTX_PER_SLOT`, `KV_TYPE`, `REASONING_BUDGET` (`-1` =
unlimited), `CACHE_RAM_MIB`, `MODEL_ALIAS` and `MIN_DECODE_TPS` (`0`: measure, never refuse)
override the setting, e.g. `CTX_PER_SLOT=65536 sudo -E ./install.sh`; with any of the first
three the free-memory check is skipped. A node with a smaller context gets only requests that
fit it.

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
| Update | `sudo ./install.sh --keep-token` from the new bundle (below) |
| Remove | `sudo ./uninstall.sh` (keeps the model; `--purge` removes it too) |

**Update:** unpack the new release's bundle and run its `install.sh` with `--keep-token` (or
press Enter at the token prompt). It keeps the token, the node id, the gateway and the
model (checked again, about a minute), replaces the binaries and units and restarts the
node. Without systemd (a container), stop the two processes, swap the bundle directory for
the new one and start them again: `node.json` and the token live outside the bundle.

**Suspend and resume:** the node stops before the computer sleeps and starts again after
resume, if it was running. The sleep hook waits until llama-server has released the GPU,
before the NVIDIA driver suspends it (SIGKILL after 20 s, at most 30 s in all), and
llama-server starts again only after the driver's resume. Logs:
`journalctl -u aisathome-node-sleep`. Installs from before 2026-10-13: run the installer
again to get this.

## Local UI

The agent serves a status page at **http://127.0.0.1:7381/** (on desktops there is also an
"AIs@home node" menu entry). It shows whether the node is connected, paused or serving, the
gateway, node id, model and preset, slots in use, the GPU (name, memory, temperature, through
`nvidia-smi`), requests and tokens served today and in total (kept across reinstalls), and recent events
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
