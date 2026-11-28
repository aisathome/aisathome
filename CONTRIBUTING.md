# Contributing to AIs@home

Thanks for helping. AIs@home is experimental and in pilot: things change quickly, and a short
question before a large change saves everyone time. How the pieces fit together:
[docs/architecture.md](docs/architecture.md).

## Ways to contribute

- **Run a node.** A Linux or Windows machine with an NVIDIA GPU of 16 GB or more:
  [what you need](https://aisathome.org/node/), [install on Linux](https://aisathome.org/node/install/),
  [install on Windows](https://aisathome.org/node/windows/). Telling us what worked or failed on
  your card and driver is useful on its own.
- **Report a bug** in [GitHub issues](https://github.com/aisathome/aisathome/issues): what you
  did, what you expected, what happened, and versions (`aisathome-node -version`, OS, GPU,
  driver). For a node, `aisathome-node status` and the last lines of its log help. **Never
  paste node tokens, API keys, or prompts you would not publish.** Security problems go by
  email instead (below).
- **Code and docs.** Bug fixes, tests, docs and the site are welcome as pull requests. For a
  new feature or a change to the protocol, the privacy rules or the routing, open an issue
  first so we can agree on the shape.

## Development setup

| Component | You need | Path |
|---|---|---|
| Gateway, node, protocol, e2e | Go, the version in `go.mod` | `gateway/`, `node/`, `protocol/`, `e2e/` |
| Site | Node.js 22.12 or newer | `site/` |
| Harness | Python 3.12 or newer, standard library only (Docker and opencode only for real sessions, not for the tests) | `harness/` |
| Windows node and installer | Windows, Go; Inno Setup 6 for the installer | `node/packaging/windows/` |

A local gateway and node, next to a `llama-server` on port 8000:

```bash
go build -o aisathome-gateway ./gateway/cmd/aisathome-gateway
go build -o aisathome-node ./node/cmd/aisathome-node
./aisathome-gateway -config examples/gateway.json
./aisathome-node -config examples/node.json
curl -H "Authorization: Bearer dev-user-key-change-me" http://localhost:8080/v1/chat/completions \
  -d '{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

The site: `cd site && npm ci && npm run dev`.

## Tests

Run what CI runs for the parts you touched:

```bash
gofmt -l .                                    # must print nothing
go vet ./...
# staticcheck as CI runs it: the Linux and Windows builds at once, so code only Windows uses counts as used
printf 'linux: GOOS=linux\nwindows: GOOS=windows\n' | go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -matrix ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go test -race ./...                           # e2e included (it needs the release tags: git fetch --tags)
./node/packaging/linux/install_test.sh        # the Linux installer
./scripts/node_sh_test.sh                     # the one-line install without an account (site/public/node.sh)
./scripts/bundle_test.sh                      # the prebuilt bundle's packaging
python3 scripts/changelog.py check            # your changelog.d/ fragment

cd harness && python3 -m unittest discover -s tests -t .

cd site && npm ci && npm test && npm run typecheck && npm run build && npm run check

# the node's setup page and the website's pages in a browser (Playwright; once: playwright install
# chromium), against site/dist built from the tests' fixed records, as CI builds it
(cd site && P2M_RECORDS=tests/fixtures/prove2me-records-2026-11-18.json npm run build)
cd e2e/browser && uv run --with-requirements requirements.txt pytest
```

The Windows jobs (node tests, the agent as a real service with `llama-server` on the CPU, and the
installer's install, update and uninstall) run in CI on Windows runners;
`.github/workflows/ci.yml` shows how to run them by hand.

What we expect of tests:

- **A bug fix comes with a test that fails without the fix.**
- Logic in the gateway or node gets Go tests; a flow across gateway and node gets an e2e test
  in `e2e/`.
- No test needs a GPU, a network or a real model: the e2e tests use a mock engine, and the
  harness tests use a fake opencode.

## Rules the code keeps

- **Hardened, not confidential.** Never claim, in code, docs or UI text, that prompts are
  confidential or not retained on nodes. Never log, store or publish prompts or answers, and
  never add them, user identities or node owners to a public view. The one exception is the
  guards' evidence: a short fragment of a node's answer that a guard stopped or
  flagged, secrets redacted, for admins only and for 14 days (docs/gateway.md, "Guards"). See "Trust boundaries and
  privacy" in [docs/architecture.md](docs/architecture.md).
- **The gateway is generic.** A science project's workflow (what its tasks mean, what comes
  after a result) belongs in the project's own code, not in the gateway; the gateway knows
  projects only from its configuration.
- **Compatibility.** The gateway keeps accepting nodes of the previous release. A change to
  the messages bumps the protocol version and updates `protocol/PROTOCOL.md`.
- **Public sources.** Code comments and docs cite public sources; a CI lint step checks this.
- Each source file starts with an `SPDX-License-Identifier: Apache-2.0` line.
- A change users or node operators would notice adds a changelog entry, tagged (node),
  (gateway), (site) or (docs), as a small file in `changelog.d/` (for example
  `changelog.d/node-token-redirect.md`; the format is in
  [changelog.d/README.md](changelog.d/README.md)). Do not edit `CHANGELOG.md` itself: entries
  move there at a release, and a pull request that edits its Unreleased section fails the
  `changelog` check.

## Pull requests

1. Fork the repository or create a branch from an up-to-date `main`. One topic per pull
   request.
2. Keep your branch current by rebasing onto `main`, not by merging `main` into it: `main` has
   a linear history.
3. Open the pull request with what it changes, why, and how you tested it.
4. **CI must pass.** The required checks are `check` (gofmt, vet, staticcheck, govulncheck, the
   internal-references lint, `go test -race`, the installer and bundle tests, the e2e tests and
   the browser test) and `site` (tests, `astro check`, build and `npm run check`). The Windows
   and harness jobs run on every push too and should be green.
5. A maintainer reviews and merges, usually with "Rebase and merge". Gateway changes go live
   after a run on a staging gateway; node changes ship with the next tagged release.

## Commit messages

[Conventional commits](https://www.conventionalcommits.org/), short:
`type(scope): what changed`, for example `feat(gateway): live model-call counts for running tasks`
or `fix(harness): checkpoint save, lease deadline, arrow binders`. Types: `feat`, `fix`, `docs`,
`test`, `ci`, `refactor`, `chore`. Scopes: `gateway`, `node`, `site`, `harness`, `e2e`,
`protocol`. A longer body is welcome when the reason is not obvious. No co-author trailers for
tools.

## Licence

The code is Apache-2.0 ([LICENSE](LICENSE)). Contributions you submit are under the same
licence (section 5 of the licence). The name "AIs@home" is not licensed with the code.

## Security

Please do not open a public issue for a security problem. Email
[contact@aisathome.org](mailto:contact@aisathome.org) with "security" in the subject, what you
found and how to reproduce it.

Worth reporting: ways to learn who sent a request, to make the gateway send a node more than
the documented request body, to read another account's data, to get around the guards, the
privacy rules or the limits, or to run code on a node or a harness worker. Not a vulnerability:
a node operator can read the prompts their node serves. That is the documented design
("hardened, not confidential"); ideas that make it harder are welcome as issues.
