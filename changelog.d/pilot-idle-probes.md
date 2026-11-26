---
section: Fixed
---

- **The model check no longer keeps an idle pilot running.** A pilot leaves after 30 minutes
  without a request, and the gateway's model-check probes count as requests, so after its work
  ended a pilot stayed on the volunteer's GPU for well over an hour (before its first check, until
  the task's 4-hour limit), with no work and no credit. Now a pilot that has had no request but
  probes for `model_check.pilot_idle_seconds` (default 600) gets no probe until a request comes
  again; a pilot serving work, and every other node, is checked as before. (gateway)
