---
section: Changed
---

- **The Linux installer asks "Link this node to your account? [Y/n]".** y (or Enter) ends the
  install by linking the node in the browser, as before; n runs it without an account (science
  only), as `--anonymous` does, with the terms shown at the question. An installed node with a
  token for the same gateway keeps it without a question. Pasting a node token is no longer part
  of the question: `--token-stdin` reads one (hidden at a terminal, or piped); a script that pipes
  a token without a flag works as before. (node)
