---
section: Changed
---

- **The Linux installer leaves node.json and token decisions to the node agent.** The checks it
  used to repeat in shell (a canonical node.json, the gateway address rule, a token kept or sent
  only to the gateway that issued it, the moved-aside token, `--anonymous` over a token) are now
  `aisathome-node config preflight`, the agent's own code, which the installer runs before it
  changes anything. A `--node-bin` or `--node-release` older than the installer is refused with a
  message, before anything is installed. (node)
