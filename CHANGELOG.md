# Changelog

What changed in each release, for people who run a node or use the API. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the git tags. Each
entry says where it applies: **(node)** needs the new release on your machine, **(gateway)**
and **(site)** are live on aisathome.org once deployed.

## [Unreleased]

- **Dev log** at [aisathome.org/devlog/](https://aisathome.org/devlog/), with numbered posts,
  an RSS feed and the latest posts on the home page. (site, live on aisathome.org)
- **Visitor statistics without cookies:** daily page views and an estimate of visitors, counted
  from the web server's log; no IP addresses or browser names are kept. What is counted is
  described in `tools/sitestats/doc.go`. (site)
- **Node settings explained:** the preset docs link to the exact lines in `install.sh` that set
  each value. (docs)

## [v0.2.0] — 2026-10-20

A release for running nodes reliably and for long agent sessions.

### Added

- **Cache-aware routing:** a conversation goes back to the node that already holds its
  context, so long agent sessions don't re-read 50–100k tokens when they switch node.
  (gateway, live on aisathome.org)
- **The node reports the release version**, shown on the local status page and to the
  gateway. (node)
- **Docs:** running BOINC on the same GPU (`exclusive_gpu_app`), uninstalling, tested
  systems. (node)

### Changed

- **Never left running on the CPU:** if the GPU isn't usable, the node stops instead of
  serving slowly on the CPU. (node)
- **Credit only for what reached you:** a node is credited only for output that was actually
  delivered. (gateway, live on aisathome.org)
- **Safety:** a node flagged by the gateway's checks is held *before* it can take another
  request, and the hold can't be lifted by accident. (gateway, live on aisathome.org)

### Fixed

- **Newer NVIDIA drivers (545 and later) work:** the installer allows the GPU under both its
  old and new device names. Tested on driver 575 (RTX 3090) and 530 (RTX 4090). (node)
- **Installed libraries are readable** whatever the file modes in the bundle (fixes a crash
  loop after installing with a restrictive umask). (node)
- **Reliability:** no lost answers when a node disconnects right after finishing; a cancelled
  request keeps its slot until the node's engine has stopped; clearer errors when an agent
  request has nowhere to go. (gateway, live on aisathome.org)

### Updating a node

```
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.2.0/aisathome-node-bundle-linux-amd64.tar.xz
curl -LO https://github.com/aisathome/aisathome/releases/download/v0.2.0/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xJf aisathome-node-bundle-linux-amd64.tar.xz && cd aisathome-node-bundle-linux-amd64
sudo sed -n 's/^AISATHOME_NODE_TOKEN=//p' /etc/aisathome-node/token.env | sudo ./install.sh
```

The last line reuses your existing node token; the model already on disk is reused.

## [v0.1.0] — 2026-10-13

The first release. Notes to follow.

[Unreleased]: https://github.com/aisathome/aisathome/compare/v0.2.0...main
[v0.2.0]: https://github.com/aisathome/aisathome/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/aisathome/aisathome/commits/v0.1.0
