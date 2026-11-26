---
section: Added
---

- **A running task shows its model calls in flight.** `live.in_flight` counts the calls that named
  the task and have not ended yet, in the public task views and the project's own task list, and
  `live` now shows while a task's first call is still in flight; a long generation no longer looks
  like a task doing nothing with the model. (gateway)
