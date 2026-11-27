---
section: Added
---

- **Pilots come in tiers, and a project can keep its sessions off small contexts.** A connector's
  `pilot_tiers` names the kinds of pilot it brings by context (16 GB cards at 81,920 tokens, 24 GB
  cards at 196,608); a pilot is created for one tier, and the demand says how many of each are
  wanted. A project's `min_node_ctx` keeps its requests for a role off models with less context
  and asks only for pilots of a tier that large; a request that names a model is not held to it.
  (gateway)
