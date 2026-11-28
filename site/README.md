# aisathome.org

The website: landing page and docs, built with [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build) into static files (`dist/`) that Caddy serves.
Content is Markdown in `src/content/docs/`; the sidebar is in `astro.config.mjs`.

Build (Node ≥ 22.12; here in a container, telemetry off):

```bash
docker run --rm -e ASTRO_TELEMETRY_DISABLED=1 -v "$PWD":/w -w /w -u "$(id -u):$(id -g)" \
  -e HOME=/tmp node:22-bookworm-slim sh -c 'npm ci && npm run build'
```

Unit tests of the pure TypeScript in `src/data` are `tests/*.test.ts` (`npm test`: `node:test`, the
modules loaded through Vite as the build loads them); CI runs them before the build.

Versions are pinned exactly and the lockfile is committed; upgrade deliberately. Pages that
are not final carry a `banner` in their frontmatter.

Styling: Tailwind 4 through `@astrojs/starlight-tailwind`; `src/styles/global.css` holds the one
theme (fonts, accent, grays) that both Tailwind and Starlight read. Web fonts are self-hosted
from `@fontsource-variable/*` packages; nothing loads from a CDN. `/design/a|b|c/` are
home-page design previews (noindex) sharing copy from `src/data/home.ts`; the live network
strip (`src/components/LiveStats.astro`) reads the gateway's public `/v1/stats` and stays
hidden when that fails. The network page's daily charts (`src/pages/network.astro`) read
`/v1/stats/history` and stay hidden the same way, and until there are two days with numbers.

Search engines and AI answer engines: `src/data/seo.ts` writes the home page's title and JSON-LD,
`/robots.txt` and `/llms.txt` (`src/pages/*.txt.ts`); the sitemap is Starlight's
(`/sitemap-index.xml`; Caddy redirects `/sitemap.xml` there). The share image of every page
(`public/og.png`, tags in `astro.config.mjs`) and the logo of the JSON-LD (`public/logo-512.png`)
are drawn by `scripts/share-image.mjs` with headless Chrome from the site's fonts and committed;
run it again after changing their copy or design. `npm run check` checks them in the build
(`scripts/seo-checks.mjs`, also used by `tests/seo.test.ts`), and that every page has a
description of its own: give new pages a specific `description`.

Dev log (`/devlog/`, feed `/devlog/rss.xml`, the newest two also on the home page): one
Markdown file per post in `src/content/devlog/`, named `<number>-<slug from the title>.md`
(the file name is the URL), with frontmatter `title`, `number` (1, 2, 3, ...: shown as
"Dev log #N" above the title; the build fails on a reused number or a file name that does
not start with it), `date`, `summary` (one line for the list and the feed) and `publish`. Only
`publish: true` posts are built; `DEVLOG_DRAFTS=1` builds the drafts too, marked as drafts
(the staging deploy sets it, so a post can be reviewed there before it is published;
`npm run check` fails if a build without the flag contains a draft). Images go in
`src/content/devlog/img/` and are referenced as `![alt](./img/name.svg)`. Posts carry no
author name.

Discord (`/discord/`): the header's Discord icon and the home page link there, never to Discord
itself, so the visitor statistics count the clicks as views of `/discord/`. Without an invite the
page says the server is coming soon; setting `invite` in `src/data/discord.ts` (an invite that never
expires) makes it send visitors on to the server. `npm run check` checks both.

prove2me (`/science/prove2me/`): built from `src/data/science/prove2me.json` (committed, exported from
the trial records, public fields only), which stays the history (trials 1 to 5 are frozen there) and
the fallback. In the browser the headline numbers, the submissions, the trials table and each trial's
list from trial 6 on, the home page's prove2me line and the `/science/` card come from the gateway's
public campaigns, results and tasks (`/v1/projects/prove2me/campaigns`, `/results`, `/v1/tasks`;
`src/data/prove2me-records.ts`, drawn by `prove2me-view.ts` for the build and the browser alike), and
"Working on now" from the project's data document or the newest trial campaign with its tasks, on load
and every minute; the build's copy stays when they fail. What a task's outcome means (a leaf proved,
the holes in place, `closed_by`, an outcome in words) lives in `prove2me-outcomes.ts`. Unit tests on
fixed data are `tests/prove2me-*.test.ts`; the page in a browser against fixed gateway answers is
`e2e/browser/test_site_prove2me.py` (the live parts) and `test_site_prove2me_content.py` (what the pages say and show,
the overview and the home page's line too; both need `site/dist`).

What the site's pages say is checked in a browser here, against `site/dist` built from the fixed records (as CI's
browser job builds it): `e2e/browser/test_site_pages.py` (the dev log, the privacy notice, the node docs, the footer,
the about page, the live strip, the menu, the credit tiles), `test_site_tauceti.py` (the Tau Ceti page) and
`test_site_network.py` (the network page's history and pressure). A change of a page's wording or structure changes its
test in the same pull request:

    cd site && npm ci && P2M_RECORDS=tests/fixtures/prove2me-records-2026-11-18.json npm run build
    cd ../e2e/browser && uv run --with-requirements requirements.txt pytest -q

Proved theorems (`/science/prove2me/theorems/`; one page per accepted leaf at `/science/prove2me/theorems/<leaf>/`, and
`theorems.json` and `<leaf>.json`): every proof prove2me accepted, the denominator and three costs, computed from the same
records (`src/data/prove2me-theorems.ts`) by the build (`prove2me-theorems-build.ts`) and again in the browser every
minute (`src/scripts/theorems-live.ts`). A reuse is the gateway's `reuse` record on a result, nothing the site keeps.
Each page's share card is drawn at build time by `src/data/og-card.ts`: the static `@fontsource` fonts read with
`fontkitten`, a small rasterizer, and a PNG through `node:zlib`; no browser or native image library.

Science (`/science/tauceti/`, data at `/science/tauceti.json`): a finished project (2026-11-15).
Built from `src/data/science/tauceti.json`, the final data (public fields only, frozen from the
gateway's last daily export), with `src/data/science-insights.ts` (numbers from `science-stats.ts`;
`science-model.ts` holds the shape); nothing is loaded live, and `src/pages/science/tauceti.json.ts`
publishes the same file. Its styles are global (`.sci-` prefix) because the rendered HTML is not
part of the template.
`npm run check` fails if the JSON gains a field outside the public list. The home page's science
section and the chart are left out while nothing is merged. Who closed a pull request is not in the
data: the ones we closed when Tau Ceti asked us to stop are listed in
`science-render.ts` (`pause`), which also says that tasks that passed their checks are not sent.
