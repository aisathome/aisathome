---
section: Fixed
---

- **The home page and /science/ count a leaf proved by its in-place task, and refresh every minute.**
  Their live count of open leaves proved left out a leaf whose in-place task passed the full check
  itself (no assembly after it), so they said one fewer than the prove2me page; they now ask the
  gateway for those tasks too. Both read the gateway's records again every minute while the page is
  open, as the live strip and the prove2me page do, instead of once. The chart of accepted proofs
  hides when a live read has no accepted proof instead of keeping the old ones under a zero, and a
  page built before any acceptance fills it once there is one. The prove2me page dates the JSON of
  its recorded history and links the gateway's live records beside it. (site)
