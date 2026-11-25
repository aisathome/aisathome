---
section: Security
---

- **The installer's "Link this node to your account? [Y/n]" answer is no longer shown as it is
  typed.** The older installer asked for a node token at this point, so a token pasted there out of
  habit would have stood in full on the screen and in terminal logs. The answer is now read without
  echo: echo is off before the question is printed and stays off through every retry. Anything
  that is not y or n is dropped unseen, and an answer that looks like a token is told that it was
  neither stored nor shown and that `--token-stdin` gives a node a token. (node)
