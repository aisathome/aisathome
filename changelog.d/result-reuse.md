---
section: Added
---

- **An external result records a reuse.** A project's result can carry `reuse`, what it reused instead
  of the project's own work (prove2me: an accepted submission whose proof is a theorem another user had
  proved): its name, page, author and relation. The writer sends it with the result, a later write
  without it keeps it, and the new `PATCH /v1/projects/{id}/results/{result}` sets, changes or clears it
  by the result's public id, for corrections and backfills. `scripts/prove2me_reuse_backfill.py`
  records the three reuses from before the field. (gateway)
