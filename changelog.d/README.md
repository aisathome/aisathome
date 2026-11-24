# Changelog fragments

A change that people who run a node or use the API would notice gets its `CHANGELOG.md` entry as
a file here, not as an edit to `CHANGELOG.md`: two pull requests that each add a file never
conflict, two that both edit the top of the Unreleased section always do.

One file per change, named `<short-slug>.md` (lower case, digits and dashes, for example
`node-token-redirect.md`). It starts with front matter naming the section, then the entry
exactly as it would stand in `CHANGELOG.md`: a bullet with a bold first sentence, wrapped at 100
columns, ending with where it applies, (node), (gateway), (site) or (docs). More than one bullet
is fine when they belong to the same change.

```markdown
---
section: Fixed
---

- **A node token never follows a redirect.** The node used to send its token on to wherever the
  gateway redirected it; now it stops and says so. (node)
```

The section is one of `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed` or `Security`
([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)). To change an entry before it is
released, edit its file.

`python3 scripts/changelog.py check` checks every file here (CI runs it). At a release,
`python3 scripts/changelog.py fold <version>` moves what is under Unreleased in `CHANGELOG.md`
and every fragment, in file-name order within each section, into a new section for the version,
deletes the fragments and updates the links at the end; see
[docs/RELEASING.md](../docs/RELEASING.md).
