---
section: Changed
---

- **The one-line Linux install starts the node at boot.**
  `curl -fsSL https://aisathome.org/node.sh | sh` now runs the installer with `--enable-at-boot`, so
  a node without an account comes back after a reboot, as the Windows node does; it says so before
  it asks to install. `sudo systemctl disable aisathome-llama aisathome-node` turns it off. (site)

- **The one-line Linux install updates a node that is there already.** It used to install every
  machine as a node without an account. Now, finding an installed node, it says "Existing node
  found: updating it, keeping its settings" with the old and new versions and runs the installer
  as an update: a linked node stays linked, one without an account stays so, the node id, gateway,
  model, local password, counters and start at boot are kept, and a node from before v0.6.0 gets
  the installer's link question. (site)
