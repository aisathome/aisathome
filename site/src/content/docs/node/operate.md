---
title: Day to day
description: Pause, resume, logs, troubleshooting and uninstalling an AIs@home node.
---

| | |
|---|---|
| Pause and free the GPU | `sudo systemctl stop aisathome-node aisathome-llama` |
| Resume | `sudo systemctl start aisathome-llama aisathome-node` |
| Logs | `journalctl -u aisathome-node -u aisathome-llama -f` |
| Start at boot | `sudo systemctl enable aisathome-llama aisathome-node` |
| Remove | `sudo ./node/packaging/linux/uninstall.sh` (keeps the model; `--purge` removes it too) |

## Privacy hardening

These measures make it less likely that prompts end up on disk by accident:

- **No core dumps** (`LimitCORE=0`): a crash dump would contain prompts from memory.
- **No swap** (`MemorySwapMax=0`, needs cgroup v2): prompts are never paged out.
- **A smaller prompt cache in RAM** (`--cache-ram 2048` MiB instead of 8192). Override with
  `CACHE_RAM_MIB`; `0` disables it.
- **No prompt logging:** `llama-server` runs without verbose logging.

They do not make the node confidential: anyone with root on the machine can still read
prompts from RAM or VRAM while they are being served.

## Troubleshooting

**`no kernel image is available for execution on the device`.** `llama-server` was built for
another GPU generation. Rebuild with `"-DCMAKE_CUDA_ARCHITECTURES=86;89"` (see
[install](/node/install/)).

**CMake: `CMAKE_CUDA_COMPILER-NOTFOUND`.** `nvcc` is not on `PATH`:
`export PATH=/usr/local/cuda/bin:$PATH` and configure again from an empty `build/`.

**`no /dev/nvidia-uvm and no nvidia-modprobe`.** The GPU device nodes are missing and the
helper that creates them is not installed: `sudo apt install nvidia-modprobe`, then start
the services again.

**`llama-server did not become healthy`.** Check `journalctl -u aisathome-llama`. Out of
memory usually means another program uses the GPU; try `--preset low-vram`.

**The node does not connect.** Check the `gateway_url` in `/etc/aisathome-node/node.json`
and that the machine can reach `api.aisathome.org` on port 443. A rejected token shows up
in `journalctl -u aisathome-node`.

**After suspend.** The node stops before suspend and starts again after resume. If the GPU
was reset, the watchdog restarts `llama-server` within a few minutes.
