# Releasing

For the maintainer. Volunteers read the site page "About the binaries"
(`site/src/content/docs/node/binaries.md`).

## With every change

Every user-visible change adds an entry under **Unreleased** in `CHANGELOG.md`, in the same
commit, tagged with where it applies: (node), (gateway), (site) or (docs).

## Before a release

1. `main` is green in CI, and the changes are on prod or reviewed on staging.
2. **Dry run on GitHub:** Actions → release → "Run workflow" on the branch (or
   `gh workflow run release.yml --ref <branch>`). It builds everything, publishes nothing and
   keeps `dist/` as a 7-day artifact. A single architecture (`cuda_archs: 89-real`) is a
   faster first check; the full build takes an estimated 1 to 2 hours.
3. **Test the artifact on a GPU** before tagging: unpack, `./install.sh --dry-run`, a real
   install with a throwaway node token from staging, one chat request through the node, then
   `uninstall.sh --purge`. Do it on at least one RTX 30 with an older driver (535) and one RTX
   40; add an RTX 50 when its build changed.

## Building locally

`./scripts/build-release.sh v0.1.0 dist` builds the static node and gateway binaries
(`aisathome-node`: linux amd64/arm64, windows amd64; `aisathome-gateway`: linux amd64/arm64)
and `SHA256SUMS`; with `LLAMA_DIR` pointing at the output of `scripts/build-llama.sh` it packs
the node bundle too. `aisathome-node -version` prints the version.

## Making a release

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

The workflow runs the CI checks, builds the Go binaries, `llama-server` with CUDA and the
bundle, writes `SHA256SUMS`, and attaches everything to a **draft** release. Review the
draft (files, sizes, `BUILD-INFO`, generated notes). Tags with a `-` (for example
`v0.2.0-rc1`) become pre-releases.

**Release notes:** move `CHANGELOG.md`'s Unreleased entries into a new section for the
version (with the date and the update commands for a node), and use that text as the
release's notes. The owner approves the text before the draft is published; then publish it.

## Changing what is built

- **llama.cpp:** change `LLAMA_TAG` and `LLAMA_COMMIT` in `scripts/build-llama.sh` together,
  rerun the benchmarks and the model-check references (a new llama.cpp can change the
  numbers the model check compares against), and say so in the release notes.
- **CUDA image:** update the digest in `release.yml` and the driver minimums in
  `install.sh`, the packaging README and the site page if the CUDA version changes.
- **Model:** the pin (repository, revision, file, SHA-256, size) lives in `install.sh`; keep
  the gateway's tokenizer pin (ops) on the same model. A new model file (even another quant of
  the same model) needs the tiers re-measured (`tier_min_free`, the speed floor) and a new
  model-check reference, and every node switches in the same release: the network never mixes
  model files.
- **Tiers:** a change to a tier's context or KV type, or to llama.cpp, needs `tier_min_free`
  re-measured on a 16 GB and a 24 GB card (memory after load and at the peak of a prompt near
  the full context).

## Not done yet

Signed releases and build provenance (for example GitHub artifact attestations), and
reproducible builds that others can verify.
