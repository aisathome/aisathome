---
section: Fixed
---

- **The prove2me page says a hole failed, and why.** A hole whose task had ended without a proof
  showed "Waiting"; it now says Failed with the reason from its task's outcome, for example
  "Failed: compile error" or "Failed: not proved (a sorry left)", or Cancelled when its task was
  cancelled while it ran. Waiting is left for a hole whose task has not run. An outline no longer
  says a newer outline was started when that outline was cancelled before it ran. (site)
