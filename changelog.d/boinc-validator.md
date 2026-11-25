---
section: Added
---

- **The pilot app's BOINC validator.** `boinc/validator/` is BOINC's validator framework with a
  `check_set` that reads each pilot's final credit from the file the bridge writes, so BOINC grants
  exactly the credit the gateway metered (`--post_assigned_credit`). The bridge runs it on one work
  unit right after writing that work unit's file (`validator` in the bridge's config), never as a
  daemon: the framework cannot wait a day for a work unit with one result. Built against the
  project's BOINC server release; CI builds it against 1.6.3 and tests it in BOINC's framework
  against MariaDB. See `boinc/README.md`. (gateway)
