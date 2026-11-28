---
section: Added
---

- **Failures per node and tier.** The gateway counts, per UTC day, node and model, how each attempt
  ended: answered, cut at `max_tokens` or at the end of the node's context, context exceeded, a tool
  call the engine could not parse, an unusable tool call, other failures by category, and the requests
  refused before any node because no node had the context. Counts only, kept 35 days in the state dir;
  `aisathome-gateway accounts node-failures [days] [node]` prints them per node and per model (the
  16 GB and 24 GB tiers serve different model names). (gateway)
