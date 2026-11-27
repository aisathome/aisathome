---
section: Fixed
---

- **A Linux update whose engine check fails keeps the node's start at boot.** When the speed test
  after an update failed (most often because another program was using the GPU), the installer
  stopped the node and turned start at boot off, so the node stayed offline, even after a reboot,
  until the installer was run again. Now an update leaves start at boot as it was, and after a slow
  speed test each service goes back to how it was before the update: a node that was running runs
  again. Out of GPU memory, the node is stopped and a reboot starts it again. A first install that
  fails a check still ends stopped and disabled, and so does any install where llama-server found no
  GPU, so nothing runs on the CPU. An update that had moved the node's token aside (a gateway change,
  or a token from before v0.6.0) puts the token back and ends the same way. (node)
