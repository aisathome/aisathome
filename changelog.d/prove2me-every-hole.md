---
section: Fixed
---

- **The prove2me page shows every hole of an outline as Waiting, Working or Solved.** A hole
  without a task of its own used to read "closed before the hole tasks, or left in place", a
  guess, and holes after the last hole task did not show. Tasks now carry the outline's holes in a
  public label (`hole_states`), so every hole from 1 to n shows in one of three states: Waiting
  (held back, not started, failed, or not known), Working (its own task is queued or running, with
  the try and its steps) or Solved (by any route). Why a hole waits and how it was solved are in its
  tooltip only. (site)
