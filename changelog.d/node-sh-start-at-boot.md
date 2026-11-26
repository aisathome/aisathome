---
section: Changed
---

- **The one-line Linux install starts the node at boot.**
  `curl -fsSL https://aisathome.org/node.sh | sh` now runs the installer with `--enable-at-boot`, so
  a node without an account comes back after a reboot, as the Windows node does; it says so before
  it asks to install. `sudo systemctl disable aisathome-llama aisathome-node` turns it off. (site)
