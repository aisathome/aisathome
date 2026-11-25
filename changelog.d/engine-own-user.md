---
section: Security
---

- **llama-server runs as its own user, and the installer never follows a link in the models
  folder.** On Linux the engine now runs as `aisathome-llama`, not as the agent's `aisathome`
  user: it can read the model but cannot write the agent's state, through which the agent asks
  root to restart, stop or uninstall the node. The models folder belongs to the engine's user; the
  installer downloads, checks and moves files there as that user, and as root only sets owners
  without following links, so a link planted there can no longer make it change a system file.
  An update moves the folder and its files to the new user. (node)
