# Releasing

For the maintainer. Volunteers read the site page "About the binaries"
(`site/src/content/docs/node/binaries.md`).

## Before a release

1. `main` is green in CI, and the changes are on prod or reviewed on staging.
2. **Dry run on GitHub:** Actions → release → "Run workflow" on the branch (or
   `gh workflow run release.yml --ref <branch>`). It builds everything, publishes nothing and
   keeps `dist/` as a 7-day artifact. A single architecture (`cuda_archs: 89-real`) is a
   faster first check; the full build takes an estimated 1 to 2 hours.
3. **Test the artifact on a GPU** before tagging: unpack, `./install.sh --dry-run`, a real
   install with a throwaway node token from staging, one chat request through the node, then
   `uninstall.sh --purge`. Do it on at least one RTX 30 with an older driver (535) and one RTX
   40; add an RTX 50 when its build changed. The test plan with costs is in the research repo
   (`bench/modelcheck/PLAN.md`, bundle test).

## Making a release

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

The workflow runs the CI checks, builds the Go binaries, `llama-server` with CUDA and the
bundle, writes `SHA256SUMS`, and attaches everything to a **draft** release. Review the
draft (files, sizes, `BUILD-INFO`, generated notes), then publish it. Tags with a `-` (for
example `v0.2.0-rc1`) become pre-releases.

## Changing what is built

- **llama.cpp:** change `LLAMA_TAG` and `LLAMA_COMMIT` in `scripts/build-llama.sh` together,
  rerun the benchmarks and the model-check references (a new llama.cpp can change the
  numbers the model check compares against), and say so in the release notes.
- **CUDA image:** update the digest in `release.yml` and the driver minimums in
  `install.sh`, the packaging README and the site page if the CUDA version changes.
- **Model:** the pin (repository, revision, file, SHA-256, size) lives in `install.sh`; keep
  the gateway's tokenizer pin (ops) on the same model.

## Not done yet

Signed releases and build provenance (for example GitHub artifact attestations), and
reproducible builds that others can verify.
