---
section: Changed
---

- **MTP speculative decoding is on by default on 24 GB cards.** On the big tier the Linux
  installer now runs `--speculation mtp` unless told otherwise (about 1.7-1.9x output speed on our
  24 GB nodes; the model check passes with it); it downloads the 1.4 GB draft model with the model,
  and runs without it, saying why, when the card lacks the free memory or the engine is too old.
  `--speculation off` (or `none`) turns it off and is remembered on updates; an earlier
  `--speculation mtp` or `dflash2` stays. A node that never chose gets the default on its next
  update. Nodes updated from an older installer get MTP on the big tier even if `off` was stored
  before; to keep it off, pass `--speculation none` (remembered). 16 GB cards and the Windows node
  stay off. (node)
