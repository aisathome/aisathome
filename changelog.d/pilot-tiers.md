---
section: Added
---

- **A BOINC pilot serves 16 GB cards too.** It picks its tier from the model file its task
  brings (the standard tier's UD-Q3_K_XL answers as `qwen3.8-27b-q3` with 81,920 tokens), checks
  the card the client assigned as the engine sees it (`llama-server --list-devices`: big enough for
  the tier, enough free memory), and runs the installer's speed test before it connects (at least
  20 tokens/s after a 32,000-token prompt; a pass is remembered for 7 days per card, tier and
  engine). A card that fails either check exits 1 without connecting. `boinc/app-version.sh` takes
  `--app`, for the 16 GB tier's app `pilot_std`. (node)
- **Pilots come in tiers, and a project can keep its sessions off small contexts.** A connector's
  `pilot_tiers` names the kinds of pilot it brings by context (16 GB cards at 81,920 tokens, 24 GB
  cards at 196,608); a pilot is created for one tier, and the demand says how many of each are
  wanted. A project's `min_node_ctx` keeps its requests for a role off models with less context
  and asks only for pilots of a tier that large; a request that names a model is not held to it.
  (gateway)
