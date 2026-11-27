---
section: Added
---

- **A page of proved theorems.** `/science/prove2me/theorems/` leads with the number of the model's
  proofs prove2me accepted and what it is out of, charts them over time and per trial, asks for GPUs
  (with who can join today), and lists every proof prove2me accepted, with the selection of easier leaves from trial 11
  on, and three costs (the median GPU-hours per proof, and all-in for recent trials and for every
  trial), all computed from the gateway's records at the build and again every minute in the browser.
  Each accepted leaf has a page of its own with a share card, and the list and each leaf are also JSON.
  Reuses come from the gateway's `reuse` record; the site's own correction file is gone. TeX in
  prove2me's titles reads as plain text (`$\Pr[\Sigma_S=\sigma^-_S]\ge\pi^-$` as "Pr[Σ_S = σ⁻_S] ≥ π⁻")
  on these pages, the prove2me page and the share cards. Dev log 5's
  "~24 GPU-hours per proof" now carries a dated note that points to the current numbers. (site)
