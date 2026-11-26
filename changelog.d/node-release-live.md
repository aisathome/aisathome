---
section: Added
---

- **The site's download block shows the latest release within minutes of its publication.** The
  gateway reads the node's latest release from GitHub's public API every 10 minutes (no token),
  keeps the last good one when GitHub fails, and serves it at `GET /v1/node/release`: version,
  installer and bundle sizes, whether the version-free installer exists. Turned on with
  `node_release_url`. (gateway)
- **The Windows download on the home page, `/node/` and `/node/windows/` reads that release in the
  browser**, so it no longer shows the previous release until the site is changed and deployed; its
  button then links that release's own file, so the version, size and file always match. The
  release the site was built with stays the copy without JavaScript, for search engines' JSON-LD
  and whenever the gateway does not answer. (site)
