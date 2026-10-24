---
title: Update your node
description: Install a new release on a running AIs@home node, keeping its token and settings.
---

New releases are listed on [GitHub](https://github.com/aisathome/aisathome/releases); each
one says what changed and whether nodes need it.

## With the installer (systemd)

Download the new bundle, check it and run its installer:

```bash
base=https://github.com/aisathome/aisathome/releases/latest/download
curl -LO $base/aisathome-node-bundle-linux-amd64.tar.xz -LO $base/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
sudo ./install.sh --keep-token
aisathome-node status
```

`--keep-token` keeps the installed node token, so you don't paste it again. Without it, the
installer asks as usual: press Enter on the empty prompt to keep the installed token, or
paste a new one.

The installer keeps:

- the token, the node id and the gateway (from `/etc/aisathome-node/node.json`); pass
  `--node-id` or `--gateway-url` to change them;
- the model, which it checks again (about a minute); a release that pins another model
  downloads it (v0.3.0: `UD-IQ4_XS`, 14.3 GB, replaces `UD-Q4_K_M`; the old file is replaced
  once the new one is verified, so you need about 16 GB free in `/var/lib` for the update);
- the local UI's password, the counters and whether the node starts at boot.

It checks the card for its [tier](/node/presets/) again (the memory your running node
holds counts as free), replaces the binaries and the services, then restarts the node and
runs the speed test before it connects again. The node is offline for a
few minutes while the model loads again and the speed test runs, and an answer it is writing at that moment is cut
off. To avoid that, **Pause** it in the [local UI](/node/operate/#local-ui) first, wait until
no request is in flight, update, then **Resume**: a pause lasts across restarts.

Two things are not kept. If you wrote `node.json` yourself, pass `--node-config` again. If
you installed with settings in the environment (such as `CTX_PER_SLOT=65536`), set them
again.

`aisathome-node -version` prints the installed version. The local UI shows it too, and so
does **Your nodes** on your [account page](/account) while the node is connected.

If you [built llama.cpp yourself](/node/install/#build-it-yourself), rebuild it if the
release notes say so, and run the installer as you did at first, adding `--keep-token`, for
example with `--node-release latest`.

## Without systemd (containers)

A node in a container, for example on a rented GPU, does not use the installer. Update it by
swapping the bundle directory:

1. Download the new bundle and check it against `SHA256SUMS` (above), then unpack it next to
   the old one.
2. Stop `aisathome-node` and `llama-server`.
3. Move the old directory aside and put the new one in its place (or point your start
   script at the new one).
4. Start both again, as before, and check that the node connects.

`node.json`, the token and the model live outside the bundle, so they stay as they are.
Keep the old directory until the node has connected, so you can go back.
