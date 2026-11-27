---
section: Fixed
---

- **A Linux update whose engine check fails keeps the node's start at boot.** When the speed test
  after an update failed (most often because another program was using the GPU), the installer
  stopped the node and turned start at boot off, so the node stayed offline, even after a reboot,
  until the installer was run again. Now an update leaves start at boot as it was, and after a slow
  speed test each service goes back to how it was before the update: a node that was running runs
  again, and a start that systemd refuses is said. Out of GPU memory, the node is stopped and, if it
  started at boot, tries again at the next boot. A first install that fails a check still ends
  stopped and disabled, and so does any install where llama-server ran on the CPU (also when it then
  ran out of memory) or the speed test failed instead of finishing slowly. An update that had moved the node's token aside (a gateway change,
  or a token from before v0.6.0) puts the token back and ends the same way. (node)
