---
section: Added
---

- **A project's campaigns can finish by themselves.** With `campaign_auto_finish` in a project's
  config (off by default), the gateway finishes a running campaign once none of its tasks has been
  queued or leased for `idle_hours` (default 6) and at least one has finished, with `ended_at` the
  last task's end. A task belongs to a campaign through the labels the config names (`trial`,
  `campaign`), so a waiting submission keeps its campaign open, and a new task for a finished
  campaign reopens it. Finishing a campaign by hand still works. (gateway)
