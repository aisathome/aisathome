---
section: Changed
---

- **A pilot whose engine fails after it served exits 0.** A pilot whose engine keeps failing used to
  exit 1, which BOINC reports as an error and never validates, so answers it had served earned no
  BOINC credit. Now it exits 0 once the task has served an answer, in this run or an earlier run of
  the same task (a file next to the checkpoint records it), and BOINC grants the gateway's credit;
  the close reason and the summary still say "engine". A pilot that never served exits 1 as before.
  (node)
