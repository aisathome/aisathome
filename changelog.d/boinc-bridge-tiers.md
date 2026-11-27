---
section: Added
---

- **The BOINC bridge serves pilot tiers.** With `tiers` in its configuration, each of the gateway
  connector's tiers gets its own BOINC app and model: the bridge creates each tier's pilots as work
  units of that app, cancels unsent ones per tier, and runs the validator with each work unit's app,
  so a 16 GB card only ever gets work units whose model fits it. A configuration with `app` and
  `model` works as before. (gateway)
