---
section: Added
---

- **Stop and uninstall a Linux node from its local page, and an uninstaller that stays.** The
  installer now keeps a copy of the uninstaller at `/usr/local/sbin/aisathome-node-uninstall`
  (`sudo aisathome-node-uninstall`; it removes itself last), so removing the node no longer needs
  the bundle's folder, which the one-line install unpacks into `/tmp`. The end of the install,
  `aisathome-node status` and the local page say so. The local page (behind its password) gets
  **Stop and free the GPU**, which stops the agent and llama-server and says how to start them
  again (`sudo systemctl start aisathome-llama aisathome-node`: the page goes away with the
  agent), and **Uninstall**, with a confirm step and a box to remove the model too. Both go
  through a root-owned systemd path unit, as the engine restart does; without it the page shows
  the commands. (node)
