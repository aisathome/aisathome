# aisathome.org

The website: landing page and docs, built with [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build) into static files (`dist/`) that Caddy serves.
Content is Markdown in `src/content/docs/`; the sidebar is in `astro.config.mjs`.

Build (Node ≥ 22.12; here in a container, telemetry off):

```bash
docker run --rm -e ASTRO_TELEMETRY_DISABLED=1 -v "$PWD":/w -w /w -u "$(id -u):$(id -g)" \
  -e HOME=/tmp node:22-bookworm-slim sh -c 'npm ci && npm run build'
```

Versions are pinned exactly and the lockfile is committed; upgrade deliberately. Pages that
are not final carry a `banner` in their frontmatter.

Styling: Tailwind 4 through `@astrojs/starlight-tailwind`; `src/styles/global.css` holds the one
theme (fonts, accent, grays) that both Tailwind and Starlight read. Web fonts are self-hosted
from `@fontsource-variable/*` packages; nothing loads from a CDN. `/design/a|b|c/` are
home-page design previews (noindex) sharing copy from `src/data/home.ts`; the live network
strip (`src/components/LiveStats.astro`) reads the gateway's public `/v1/stats` and stays
hidden when that fails. The network page's daily charts (`src/pages/network.astro`) read
`/v1/stats/history` and stay hidden the same way, and until there are two days with numbers.

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

Science (`/science/tauceti/`, data at `/science/tauceti.json`): built from
`src/data/science/tauceti.json`, exported from our run records by an export script in the
research notes (public fields only) and committed here; refresh it by rerunning the export and
rebuilding.
`npm run check` fails if the JSON gains a field outside the public list. The home page's science
section and the chart are left out while nothing is merged.
