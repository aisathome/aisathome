---
section: Changed
---

- **The home page puts running a node first.** Right after the hero and the live numbers, a "Run
  a node" block has the one-line start without an account
  (`curl -fsSL https://aisathome.org/node.sh | sh`, science only), what it does, a link to the
  script's source, the Windows download, the GPU a node needs and the node's own page on your
  computer; the dev log follows it. The command, version, sizes and driver minimums come from
  one file (`site/src/data/node-release.ts`), shared with `/node/`. The prove2me line reads
  "proved 12 of the 42 open lemmas it tried" instead of "12 of 42 open leaves tried". (site)
- **`/node/` has every requirement and a "Stop, remove, link later" section.** Driver minimums,
  the 18+ and terms requirement, the node's own page, pause, stop, link and uninstall on Linux
  and Windows, with a checked download of the bundle to uninstall when the one-line start's
  temporary folder is gone. (docs)
- **Clearer titles and facts for search and AI answers.** Descriptive page titles on `/node/`,
  `/use/api/`, `/use/chat/`, `/about/` and the science pages; "aisathome" as the alternate name
  in the home page's structured data and in `llms.txt`; Tau Ceti's stop date in `llms.txt` is 7
  November 2026; dev log 4 gets a dated correction (27 of Qwen's 33 pull requests merged; the 28th
  was a hand edit). (site)
