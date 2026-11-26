---
section: Added
---

- **The node's local page says when a newer release is out.** "v0.8.0 is out (you run v0.7.0)",
  with a link to the release notes and how to update: on Linux the one-line install to copy
  (`curl -fsSL https://aisathome.org/node.sh | sh`) and the update page, on Windows the installer's
  download page; the Windows tray's tooltip says it too. The agent asks its own gateway's
  `/v1/node/release` (no token), at most once an hour and only while the page or the tray is open,
  so the node contacts no other host for it; `AISATHOME_UPDATE_CHECK=off` turns the check off. The
  page installs nothing. (node)
