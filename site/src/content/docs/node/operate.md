---
title: Day to day
description: Pause, resume, logs, troubleshooting and uninstalling an AIs@home node.
---

| | |
|---|---|
| Status | `aisathome-node status`, or the [local UI](#local-ui) |
| Pause (model stays loaded) | **Pause** in the [local UI](#local-ui) |
| Pause and free the GPU | `sudo systemctl stop aisathome-node aisathome-llama` |
| Resume | `sudo systemctl start aisathome-llama aisathome-node` |
| Logs | `journalctl -u aisathome-node -u aisathome-llama -f` |
| Start at boot | `sudo systemctl enable aisathome-llama aisathome-node` |
| Remove | `sudo ./node/packaging/linux/uninstall.sh` (keeps the model; `--purge` removes it too) |

## Local UI

The node serves a status page on your computer at **http://127.0.0.1:7381/** (desktops also
get an "AIs@home node" menu entry). It shows whether the node is connected, paused or
serving, the model, slots in use, the GPU's memory and temperature, requests and tokens
served today and since install, and recent connections and errors. It never shows prompts
or answers. **Your nodes** opens your [account page](https://aisathome.org/account).

Pause, resume and restarting the engine need the local password the installer created:

```bash
sudo cat /etc/aisathome-node/ui-password
```

Enter it once and the browser remembers it. **Pause** lets requests in flight finish and
takes no new ones, while the model stays in GPU memory; it lasts until you resume, also
across restarts. To free the GPU, stop the node instead (table above).

The page is reachable from this computer only. On a machine without a screen, open an SSH
tunnel from your own computer and browse to http://localhost:7381/ there:

```bash
ssh -N -L 7381:127.0.0.1:7381 you@node-machine
```

In a terminal, `aisathome-node status` prints the same; it exits with 0 when the node is
connected and 1 when it is not, so scripts and monitoring can use it.

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

**`NVIDIA driver ... is too old for the prebuilt llama-server`.** The bundle is built with
CUDA 12.8 and needs driver 525.60.13 or newer (570.26 or newer for an RTX 50). Update the
driver (Ubuntu: `sudo ubuntu-drivers install`), reboot, and run the installer again. Or
[build llama.cpp yourself](/node/install/#build-it-yourself) for your driver.

**`has compute capability ...; the prebuilt llama-server supports 8.6 8.9 12.0`.** The
bundle is built for the RTX 30, 40 and 50 series only. Build llama.cpp yourself for your
GPU.

**`not enough disk space`.** The model needs 16.5 GB plus about 2 GB in `/var/lib`. Free
some space and run the installer again; a started download resumes.

**`model checksum mismatch`.** The downloaded model is not the pinned file (a corrupt
download or a changed mirror). The installer deletes it; run it again to download it again.
If it happens twice, tell us.

**`the prebuilt llama-server cannot run here`.** Your system's C library is older than
glibc 2.35 (for example Ubuntu 20.04). Build llama.cpp yourself.

**`no kernel image is available for execution on the device`.** `llama-server` was built for
another GPU generation. Rebuild with `"-DCMAKE_CUDA_ARCHITECTURES=86;89"` (see
[install](/node/install/#build-it-yourself)).

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
