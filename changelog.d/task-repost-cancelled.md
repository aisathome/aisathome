---
section: Added
---

- **A cancelled task that never ran can be created again under its id.** A `create` with
  `"repost_cancelled": true` of the id of a task that was cancelled before any worker claimed it
  replaces it, with the same content or another: the new task is queued and answers 201 as a new
  one (and reopens its finished campaign under `campaign_auto_finish`). Without the flag, and for
  every other existing task, a create answers as before (200 for the same content, 409 for other),
  so repeated creates never bring withdrawn work back. (gateway)
