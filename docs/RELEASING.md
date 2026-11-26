# Releasing

For the maintainer. Volunteers read the site page "About the binaries"
(`site/src/content/docs/node/binaries.md`).

## With every change

Every user-visible change adds its changelog entry as a file in `changelog.d/`, in the same
commit, tagged with where it applies: (node), (gateway), (site) or (docs). The format is in
[changelog.d/README.md](../changelog.d/README.md). Pull requests do not edit `CHANGELOG.md`'s
Unreleased section (the CI job `changelog` fails when one does); only a release moves entries
into `CHANGELOG.md`.

## Before a release

1. `main` is green in CI, and the changes are on prod or reviewed on staging.
2. **Dry run on GitHub:** Actions → release → "Run workflow" on the branch (or
   `gh workflow run release.yml --ref <branch>`). It builds everything, publishes nothing and
   keeps `dist/` as a 7-day artifact. A single architecture (`cuda_archs: 89-real`) is a
   faster first check; the full build takes an estimated 1 to 2 hours.
   In a dry run, `llama-server` (most of that time) comes from the Actions cache when nothing
   that goes into it changed: the llama.cpp pin, `scripts/build-llama.sh`, the CUDA image, the
   CUDA architectures and the build command in `release.yml` make up the cache key, so a change
   to any of them builds it afresh. The job summary says which it was. A dry run's files are
   for testing only. A tag run never uses the cache: it always builds `llama-server` from
   scratch, since a cache entry is not a trusted source for a published binary.
3. **Test the artifact on a GPU** before tagging: unpack, `./install.sh --dry-run`, a real
   install with a throwaway node token from staging, one chat request through the node, then
   `uninstall.sh --purge`. Do it on at least one RTX 30 with an older driver (535) and one RTX
   40; add an RTX 50 when its build changed. On each, `aisathome-node modelcheck` must pass;
   when llama.cpp or a model file changed, record the new reference on the RTX 40 first
   ([modelcheck.md](modelcheck.md)).

## Building locally

`./scripts/build-release.sh v0.1.0 dist` builds the static node and gateway binaries
(`aisathome-node`: linux amd64/arm64, windows amd64; `aisathome-gateway`: linux amd64/arm64)
and `SHA256SUMS`; with `LLAMA_DIR` pointing at the output of `scripts/build-llama.sh` it packs
the node bundle too. `aisathome-node -version` prints the version. The Windows installer
(`AIsAtHome-node-setup-<version>.exe`) builds only on Windows with Inno Setup 6:
`node/packaging/windows/build-installer.ps1 -Version v0.1.0 -OutDir dist-win`.

## Making a release

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

The workflow runs the CI checks, builds the Go binaries, `llama-server` with CUDA, the
bundle and the Windows installer (on a Windows runner; its agent must be byte-identical to
`aisathome-node-windows-amd64.exe`), writes `SHA256SUMS`, and attaches everything to a
**draft** release. The installer is attached twice, as `AIsAtHome-node-setup-<version>.exe` and
as `AIsAtHome-node-setup.exe` (the same bytes), so that the site's download button
(`releases/latest/download/AIsAtHome-node-setup.exe`) follows every release. The installer is not
signed yet: Windows shows a SmartScreen warning. Review the
draft (files, sizes, `BUILD-INFO`, generated notes). Tags with a `-` (for example
`v0.2.0-rc1`) become pre-releases.

**Release notes:** on the release branch, `python3 scripts/changelog.py fold v0.7.0` (add
`--dry-run` to see the result first) moves whatever is under Unreleased in `CHANGELOG.md` and
every fragment in `changelog.d/` into a new section `## [v0.7.0] — <today>`, one `### Added`,
`### Changed` and so on each, deletes the fragments and updates the compare links at the end.
Then edit that section by hand: a short lead paragraph, the order of the entries, and the
**Known issues** and **Updating a node** sections (with the update commands for a node). Commit
it as the release commit and use that text as the release's notes. A maintainer reviews the text
before the draft is published; then publish it.

**The one-line install against the draft, before publishing:** on one of the GPU machines above,
fetch the draft's bundle (`gh release download <tag> -p 'aisathome-node-bundle-linux-amd64.tar.xz'
-p SHA256SUMS -D /tmp/draft`; a draft's files are not public yet) and run the one-liner on it:
`AISATHOME_RELEASE_BASE=file:///tmp/draft sh site/public/node.sh`, answering y. It must check the
bundle, pass the dry run and install a node without an account. Reboot (the one-liner unpacks
into `/tmp`), check that the node started by itself (`aisathome-node status`; the one-liner turns on
start at boot), that `sudo aisathome-node-uninstall` still works, and remove the node with it.
An `install.sh` change that breaks the one-liner (a flag, a question, the bundle's layout) shows
here, before anyone runs it.

**After publishing:** set the version and the installer's size in bytes in
`site/src/data/node-release.ts` (the download block on `/node/` shows them) and deploy the site.
The first time a release carries `AIsAtHome-node-setup.exe`, also set `versionFreeInstaller: true`
there: the button then links to `releases/latest/download` and needs no change per release.

## Changing what is built

- **llama.cpp:** change `LLAMA_TAG` and `LLAMA_COMMIT` in `scripts/build-llama.sh` together,
  rerun the benchmarks and record a model-check reference on the new build with
  `aisathome-node modelcheck record` (a new llama.cpp moves the numbers the model check
  compares against; [modelcheck.md](modelcheck.md)), and say so in the release notes.
- **CUDA image:** update the digest in `release.yml` and the driver minimums in
  `install.sh`, the packaging README and the site page if the CUDA version changes.
- **Model:** the pin (repository, revision, file, SHA-256, size) lives in `install.sh`; keep
  the gateway's tokenizer pin (in the deployment) on the same model. A new model file (even another quant of
  the same model) needs the tiers re-measured (`tier_min_free`, the speed floor) and a new
  model-check reference, and every node switches in the same release: the network never mixes
  model files.
- **Tiers:** a change to a tier's context or KV type, or to llama.cpp, needs `tier_min_free`
  re-measured on a 16 GB and a 24 GB card (memory after load and at the peak of a prompt near
  the full context).

## Not done yet

Signed releases and build provenance (for example GitHub artifact attestations), and
reproducible builds that others can verify.
