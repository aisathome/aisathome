---
title: Update your node
description: Install a new release on a running AIs@home node, keeping its settings; a node from before v0.6.0 is linked again.
---

New releases are listed on [GitHub](https://github.com/aisathome/aisathome/releases); each
one says what changed and whether nodes need it.

**Updating a 24 GB Linux node to v0.7.0 turns MTP on**, also on a node where an earlier installer
stored `off`: installers before v0.7.0 stored `off` for every node that made no choice, so the new
installer reads it as no choice. MTP (speculative decoding with the model's multi-token-prediction
head) makes the node write about 1.7-1.9x faster, adds a 1.4 GB download and needs 21,000 MiB free on
the card; without the room the node runs without it and the installer says why. To keep it off, run
the installer with `--speculation off` (or `none`); that choice is remembered on later updates. See
[model settings](/node/presets/#speculative-decoding-mtp).

Also from v0.7.0 on Linux: the engine (`llama-server`) runs as its own user, `aisathome-llama`, which
now owns `/var/lib/aisathome-node/models` (scripts of your own that read that folder need to allow
for it), and the installer puts the uninstaller at `/usr/local/sbin/aisathome-node-uninstall`.

**From v0.6.0, a node token works only with the gateway that issued it.** Every token the node
gets from v0.6.0 on carries a record of that gateway; a token from an earlier release has none, so
the agent no longer uses it. A node installed before v0.6.0 is therefore **linked again** on its
update to v0.6.0: it gets a new node token and a new node id on your account. Later updates keep
the token.

## With the installer (systemd)

The one-line install updates a node that is there already:

```bash
curl -fsSL https://aisathome.org/node.sh | sh
```

It finds the installed node, says **Existing node found: updating it, keeping its settings** with the
installed and the new version, runs the installer's checks and asks before it changes anything. Then
it runs the new release's installer as an update: the node keeps its token (linked to your account
or without one), its node id and gateway, the model, the local password, the counters and whether it
starts at boot; the update never makes it a node without an account by itself. A node installed
before v0.6.0 is linked again (below): the installer asks at your terminal.

**By hand**, to read each step first: download the new bundle, check it and run its installer:

```bash
base=https://github.com/aisathome/aisathome/releases/latest/download
curl -fLO $base/aisathome-node-bundle-linux-amd64.tar.xz && curl -fLO $base/SHA256SUMS \
  && grep ' aisathome-node-bundle-linux-amd64.tar.xz$' SHA256SUMS | sha256sum -c - \
  && tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64 \
  && sudo ./install.sh && aisathome-node status
```

**A node installed before v0.6.0:** the installer moves its token aside (to
`/etc/aisathome-node/token.env.unlinked`) and asks **Link this node to your account? [Y/n]**:
answer **y** (installers before v0.7.0 ask for a node token: press **Enter**), and the update ends
with linking the node to your account, as at a [first install](/node/install/#3-link-it-to-your-account):
open the link it prints in your browser, sign in, check the code and approve (`--link` skips the
question). The node gets a new node token and a new node id; press **This is my machine** on it
under **Your nodes** on your [account page](/account). The old node stays listed there.

**A node installed with v0.6.0 or later:** the installer keeps the installed token without asking
(installers before v0.7.0 ask: press Enter to keep it); `--keep-token` (`sudo ./install.sh
--keep-token`) says so for scripts. `--keep-token` stops with a
message when the installed token has no record of its gateway, or another gateway's; link the node
then (`--link`).

The installer keeps:

- the gateway, the node id and the engine address (`upstream`, from `/etc/aisathome-node/node.json`),
  and the token when it is kept (above); pass `--node-id` or `--gateway-url` to change the first two;
- the model, which it checks again (about a minute); a release that pins another model
  downloads it (v0.3.0: `UD-IQ4_XS`, 14.3 GB, replaces `UD-Q4_K_M`; v0.4.0: a 16 GB
  card's tier runs `UD-Q3_K_XL`, 13.1 GB, in its place; the old file is replaced once the new
  one is verified, so you need about 16 GB free in `/var/lib` for the update);
- the local UI's password, its port or `off` (unless you give `UI_PORT`), the counters and whether
  the node starts at boot.

It checks the card for its [tier](/node/presets/) again (the memory your running node
holds counts as free), replaces the binaries and the services, then restarts the node and
runs the speed test before it connects again. The node is offline for a
few minutes while the model loads again and the speed test runs, and an answer it is writing at that moment is cut
off. To avoid that, **Pause** it in the [local UI](/node/operate/#local-ui) first, wait until
no request is in flight, update, then **Resume**: a pause lasts across restarts.

Two things are not kept. If you wrote other settings into `node.json` yourself, pass
`--node-config` again. If you installed with settings in the environment (such as
`CTX_PER_SLOT=65536`), set them again.

`aisathome-node -version` prints the installed version. The local UI shows it too, and so
does **Your nodes** on your [account page](/account) while the node is connected.

If you [built llama.cpp yourself](/node/install/#build-it-yourself), rebuild it if the
release notes say so, and run the installer as you did at first, for example with
`--node-release latest`: a node installed before v0.6.0 is linked again (above); a later one keeps
its token with `--keep-token`.

## On Windows

Download the newer installer from [Install on Windows](/node/windows/) (you can
[check it](/node/windows/#check-the-download) there) and run it over the installed one. It
keeps the settings, the local password and the node's state, and closes the tray icon while it
updates. A node set up with v0.6.0 or later keeps its token too.

A node set up before v0.6.0 does not connect after the update: its token has no record of the
gateway that issued it. The node's page asks for the setup again; in its last step, **Link to my
account** links the node in your browser (it gets a new node token and a new node id; press **This
is my machine** on it on your account page).

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

From v0.6.0 the agent uses a token only together with the gateway that issued it. If your start
script passes the token in `AISATHOME_NODE_TOKEN`, pass that gateway beside it in
`AISATHOME_TOKEN_GATEWAY` (`wss://api.aisathome.org/v1/node/connect` for a token from
aisathome.org); a token in `node.json` needs `token_gateway` beside it. It must be the same address
as the config's `gateway_url`. Without it the agent does not start and says why.

From v0.4.0 the agent exits with an error when it has had no working link to the gateway for
5 minutes (see [Troubleshooting](/node/operate/#troubleshooting)), so your start script must
start it again whenever it exits, for example in a loop with a pause of a few seconds.
