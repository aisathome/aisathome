---
section: Changed
---

- **A cancelled task that never ran can be created again under its id.** The next `create` of the
  id of a task that was cancelled before any worker claimed it replaces it, with the same content
  or another: the new task is queued and answers 201 as a new one (and reopens its finished
  campaign under `campaign_auto_finish`). Before, such a create answered 200 with the cancelled task
  (409 with other content), so withdrawn work could not be posted again under its own ids. Every
  other existing task answers as before. (gateway)
