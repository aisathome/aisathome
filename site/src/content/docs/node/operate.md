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
| Update | `sudo ./install.sh --keep-token` from the new release's bundle: see [Update your node](/node/update/) |
| Remove | `sudo ./uninstall.sh` in the unpacked bundle, so keep that directory (keeps the model; `--purge` removes it and the `aisathome` user too). From a source checkout: `sudo ./node/packaging/linux/uninstall.sh` |

## Local UI

Nodes installed from release `v0.1.0` or later serve a status page on your computer at
**http://127.0.0.1:7381/**; an older node gets it when you run the new installer (desktops also
get an "AIs@home node" menu entry). It shows whether the node is connected, paused or
serving, the model, slots in use, the GPU's memory and temperature, requests and tokens
served today and in total (kept across reinstalls), and recent connections and errors. It never shows prompts
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

## Running BOINC on the same GPU

The model takes the whole GPU while it serves, so BOINC's GPU tasks and the node should not
run at the same time. BOINC can step aside by itself whenever the node's engine runs: name
`llama-server` as an exclusive GPU app in `cc_config.xml` in BOINC's data directory
(`/var/lib/boinc-client/` with the Debian and Ubuntu package):

```xml
<cc_config>
  <options>
    <exclusive_gpu_app>llama-server</exclusive_gpu_app>
  </options>
</cc_config>
```

If the file already exists, add only the `<exclusive_gpu_app>` line inside its `<options>`.
Then choose **Options → Read config files** in BOINC Manager, or restart the client
(`sudo systemctl restart boinc-client`).

- BOINC looks at the running processes every 10 seconds. While `llama-server` runs, it
  suspends all its GPU work ("an exclusive app is running"); CPU work goes on.
- The model stays loaded as long as the node runs, also while it is paused in the
  [local UI](#local-ui), so BOINC's GPU work waits all that time. To give the GPU back to
  BOINC, stop the node (`sudo systemctl stop aisathome-node aisathome-llama`): BOINC resumes
  within about 15 seconds.
- Suspending ends BOINC's running GPU task: it starts again from its last checkpoint, and
  the work since then is lost.
- On Linux BOINC compares only the first 15 characters of a process name; `llama-server`
  fits.
- It has no effect while BOINC's GPU mode is set to **always**; use **based on
  preferences** (BOINC Manager's Activity menu).

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

**`not enough disk space`.** The model needs 14.3 GB plus about 2 GB in `/var/lib`. Free
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

**`llama-server found no GPU and ran on the CPU`** (older installers: `llama-server is running
on the CPU, not the GPU`). CUDA saw no GPU inside the service's sandbox. With installers
before v0.2.0 that happens on NVIDIA driver 545 and newer: the sandbox allowed the GPU only
under the name older drivers use. Download the newest bundle and run its `install.sh` again;
it keeps the downloaded model. The installer stops `llama-server` in this case, so nothing
keeps running on the CPU. If the newest installer still says this while `nvidia-smi` works,
tell us and include the output of `grep nvidia /proc/devices`.

**`... MiB free; the standard tier (80k context) needs 15200 MiB free`** (or the big tier,
18,500). A screen or programs use too much of the card's memory for its tier; the installer
does not shrink the context. On a 16 GB card, connect the monitor to the motherboard's
graphics (integrated GPU) or run the computer without a desktop; close programs that use
the GPU (`nvidia-smi` lists them), then run the installer again. A desktop with a browser
often takes 1-2 GB.

**`llama-server ran out of GPU memory while loading`.** The same, found when the model
loaded: something took GPU memory after the check. The installer stopped the node; free the
memory and run it again.

**`... writes N tokens/s after a 32000-token prompt; the network needs at least 20`.** The
speed test after the install was too slow, so the installer stopped the node. Cards with
slow memory (RTX 4060 Ti, 5060 Ti) do not reach it. If BOINC GPU tasks, a game or a video ran
during the test, stop them and run the installer again.

**`... is not the network's model`.** `--model` takes only the pinned file. Leave it out and
the installer downloads the right one.

**`llama-server did not become healthy`.** Check `journalctl -u aisathome-llama`.

**The node does not connect.** Check the `gateway_url` in `/etc/aisathome-node/node.json`
and that the machine can reach `api.aisathome.org` on port 443. A rejected token shows up
in `journalctl -u aisathome-node`.

**After suspend.** The node stops before suspend and starts again after resume. If the GPU
was reset, the watchdog restarts `llama-server` within a few minutes.
