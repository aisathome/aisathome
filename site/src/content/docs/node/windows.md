---
title: Install on Windows
description: Install an AIs@home node on Windows 10 or 11 with one installer, step by step.
---

New in v0.5.0: one installer sets up the node as a Windows service with a tray icon, and the
node's own page walks you through the rest.

## What you need

- Windows 10 or 11, 64-bit.
- An NVIDIA card with 16 GB or more, in the same [tiers](/node/presets/) as on Linux: 24 GB
  and more (RTX 3090, 4090, 5090 class) or 16 GB (RTX 4080, 5070 Ti, 5080 class, with at most a
  light desktop on the card; see [what you need](/node/#what-you-need)).
- A current NVIDIA driver: at least 527.41, and 570.65 for an RTX 50. Update it from
  [nvidia.com/drivers](https://www.nvidia.com/drivers) or the NVIDIA app.
- About 17 GB free on the system drive for the model (13.1 or 14.3 GB, by tier), which goes
  into `%ProgramData%\AIsAtHome`.
- A node token from [your account page](/account) (**Add a node**).

## 1. Download and check

Download `AIsAtHome-node-setup-v0.5.0.exe` and `SHA256SUMS` from the
[latest release](https://github.com/aisathome/aisathome/releases/latest) (the file name carries
the version). In PowerShell, in your Downloads folder:

```powershell
Get-FileHash AIsAtHome-node-setup-v0.5.0.exe
```

The hash must match the installer's line in `SHA256SUMS`. That catches a broken download, not
someone who takes over the release itself; see [checking a download](/node/binaries/#checking-a-download).

## 2. Run the installer

**The installer is not code-signed yet.** SmartScreen, Defender or your browser may warn when
you download or start it. Once the hash matches, choose **More info → Run anyway**, then
accept the one administrator prompt.

It installs the node under Program Files as the service `aisathome-node`, with its own
service account, and the tray icon. At the end it opens the node's page.

## 3. Set up on the node's page

The page is `http://127.0.0.1:7381/`, on your PC only; the tray and the Start-menu entry
**AIs@home node** open it signed in. It walks through four steps:

1. **The card:** the card, its driver and its free memory.
2. **The model download:** 13-14 GB from Hugging Face, resumed after a break and checked by
   its SHA-256.
3. **The speed test:** the card must write at least **20 tokens/s** after a 32,000-token
   prompt. Stop BOINC GPU tasks, games and overclocking tools while it runs.
4. **The node token:** create a node on [your account page](https://aisathome.org/account) and
   paste its token. The gateway is aisathome.org; **Other gateway** is only for testing.

The node then starts and connects. **Your nodes** on the account page shows it.

## The tray icon

The icon shows the node's state (serving, connected, starting, paused, engine stopped, not set
up, or a problem). Its menu:

- **Open the node page**, signed in.
- **Stop engine** frees the GPU and takes the node off the network; **Start engine** brings it
  back (the model loads again, a few minutes).
- **Collect diagnostics...** (below).

## When something goes wrong

**Collect diagnostics...** in the tray menu writes one zip to your Desktop (it asks for
administrator rights). Or, in PowerShell as administrator:

```powershell
& "$env:ProgramFiles\AIsAtHome\aisathome-node.exe" diagnose
```

The zip holds the version, the service, the settings, `nvidia-smi` and the logs; the node token
and the local password are removed. Attach it when you ask for help at
[contact@aisathome.org](mailto:contact@aisathome.org). If the speed test fails, the page says
why, usually other GPU work or locked clocks.

## Update and uninstall

- **Update:** download the newer installer, check it the same way and run it over the old
  one. It keeps the settings and the token, the local password and the node's state.
- **Uninstall:** Settings → Apps → **AIs@home node** → Uninstall. It removes the service and
  the program and asks whether to delete the data folder (the model and settings).

## Tested so far

One RTX 4090 on Windows, with a pre-release build of the installer: on 2026-11-13 it served
151 requests on the live network, none failed, at about 40-46 tokens/s. **RTX 30 and RTX 50
cards and the 16 GB tier have not run on Windows yet.** Tell us what worked for you.

## Privacy

The node only dials out (no open ports), runs under its own service account, keeps its data
folder readable only by that account and administrators, and does not log prompts. It has less
hardening than the [Linux node](/node/operate/#privacy-hardening) so far: Windows may still write
memory to the page file or a crash report. Like every node it is **hardened, not
confidential**, and the same promise applies: don't look at what you serve
([what you are responsible for](/node/#what-you-are-responsible-for)).
