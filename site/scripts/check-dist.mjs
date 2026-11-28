// Checks the built site (dist/) in CI: internal links resolve, nothing loads from a third
// party, the live network strip and the leaderboard start hidden (they show themselves
// only with real data), and every page shares the theme choice with the account pages (the
// theme script runs before Starlight's). Also the dev log: drafts and the feed (below), and the Discord link.
// And the science data: public fields only, matching the page and the home page. And what search
// engines read: robots.txt, the sitemap, llms.txt, titles, descriptions, the share image, the JSON-LD.
// SPDX-License-Identifier: Apache-2.0
import { readdirSync, readFileSync, existsSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { createHash } from 'node:crypto';
import { datasetJsonLdProblems, jsonLdProblems, jsonLdScripts, llmsProblems, nodeJsonLdProblems, pngSize, proofJsonLdProblems, robotsProblems } from './seo-checks.mjs';

const dist = new URL('../dist/', import.meta.url).pathname;
const files = (dir) => readdirSync(dir).flatMap((f) => {
	const p = join(dir, f);
	return statSync(p).isDirectory() ? files(p) : [p];
});
const all = files(dist);
const html = all.filter((f) => f.endsWith('.html'));
const css = all.filter((f) => f.endsWith('.css'));
// Served by the gateway or Caddy, not by the static site.
const dynamic = ['/account', '/v1/', '/__login', '/stats/'];
const ownHosts = /^https:\/\/([a-z.]*\.)?aisathome\.org(\/|$)/;
const errors = [];
// As astro.config.mjs inlines it.
const themeSync = readFileSync(new URL('../src/scripts/theme-sync.js', import.meta.url), 'utf8').replace(/^(\/\/.*\n)+/, '').trim();

for (const f of html) {
	const page = relative(dist, f);
	const text = readFileSync(f, 'utf8');
	for (const [, url] of text.matchAll(/\s(?:href|src)="(\/[^"#?]*)/g)) {
		if (dynamic.some((d) => url.startsWith(d))) continue;
		const t = join(dist, url);
		if (!(existsSync(t) || existsSync(join(t, 'index.html')) || existsSync(t + '.html'))) {
			errors.push(`${page}: broken link ${url}`);
		}
	}
	// Absolute links to the dev log on our own domain must resolve too (posts may link that way).
	for (const [, url] of text.matchAll(/\shref="https:\/\/(?:www\.|staging\.)?aisathome\.org(\/devlog\/[^"#?]*)/g)) {
		if (!existsSync(join(dist, url)) && !existsSync(join(dist, url, 'index.html'))) errors.push(`${page}: broken dev log link ${url}`);
	}
	const sync = text.indexOf(`<script>${themeSync}</script>`);
	const starlight = text.indexOf('window.StarlightThemeProvider');
	if (sync < 0 || !themeSync.includes('aisathome_theme')) errors.push(`${page}: no theme script (src/scripts/theme-sync.js)`);
	else if (starlight >= 0 && starlight < sync) errors.push(`${page}: the theme script runs after Starlight's`);
	for (const [, url] of text.matchAll(/<(?:script|link|img|iframe|source)\b[^>]*\s(?:src|href)="(https?:\/\/[^"]+)"/g)) {
		const isCanonical = /rel="(canonical|alternate|sitemap)"/.test(text.slice(text.lastIndexOf('<', text.indexOf(url)), text.indexOf(url)));
		if (!ownHosts.test(url) && !isCanonical) errors.push(`${page}: loads from a third party: ${url}`);
	}
}
for (const f of css) {
	for (const [, url] of readFileSync(f, 'utf8').matchAll(/url\((?:["']?)(https?:\/\/[^)"']+)/g)) {
		if (!ownHosts.test(url)) errors.push(`${relative(dist, f)}: CSS loads from a third party: ${url}`);
	}
}
const home = readFileSync(join(dist, 'index.html'), 'utf8');
const strip = home.match(/<section[^>]*data-live-stats[^>]*>/);
if (!strip) errors.push('index.html: no live network strip');
else if (!/\shidden(\s|>|=)/.test(strip[0])) errors.push('index.html: the live strip is not hidden before data arrives');

const network = readFileSync(join(dist, 'network', 'index.html'), 'utf8');
const board = network.match(/<section[^>]*data-credit-board[^>]*>/);
if (!board) errors.push('network/index.html: no leaderboard');
else if (!/\shidden(\s|>|=)/.test(board[0])) errors.push('network/index.html: the leaderboard is not hidden before data arrives');
if (!/<section[^>]*data-live-stats[^>]*\shidden/.test(network)) errors.push('network/index.html: the live strip is not hidden before data arrives');

// The dev log: unpublished posts only in a build with DEVLOG_DRAFTS=1 (staging), and the feed
// lists exactly the posts of the list page, each of which exists.
const drafts = process.env.DEVLOG_DRAFTS === '1';
for (const f of html) {
	if (!drafts && /\sdata-devlog-draft[\s=>]/.test(readFileSync(f, 'utf8'))) {
		errors.push(`${relative(dist, f)}: an unpublished dev log post in a build without DEVLOG_DRAFTS=1`);
	}
}
// Design previews (/preview/, data-home-variant) are for the local dev server only, never a build.
for (const f of html) {
	if (/\sdata-home-variant[\s=>]/.test(readFileSync(f, 'utf8'))) errors.push(`${relative(dist, f)}: a dev log design candidate in a build`);
}
if (existsSync(join(dist, 'preview'))) errors.push('preview/: dev-only pages in a build');
const rssFile = join(dist, 'devlog', 'rss.xml');
if (!existsSync(rssFile)) errors.push('devlog/rss.xml: missing');
else {
	const rss = readFileSync(rssFile, 'utf8');
	if (!rss.startsWith('<?xml') || !rss.includes('<rss version="2.0"')) errors.push('devlog/rss.xml: not an RSS 2.0 document');
	const fed = [...rss.matchAll(/<item>[\s\S]*?<link>([^<]+)<\/link>/g)].map((m) => new URL(m[1]).pathname);
	const listed = [...readFileSync(join(dist, 'devlog', 'index.html'), 'utf8').matchAll(/data-devlog-item[^>]*>[\s\S]*?<a href="([^"]+)"/g)].map((m) => m[1]);
	if (fed.join() !== listed.join()) errors.push(`devlog: the feed (${fed}) and the list page (${listed}) differ`);
	for (const p of fed) if (!existsSync(join(dist, p, 'index.html'))) errors.push(`devlog/rss.xml: ${p} does not exist`);
	// The home page's dev log section shows the newest three (or fewer) of the same posts.
	const home = [...readFileSync(join(dist, 'index.html'), 'utf8').matchAll(/<a href="([^"]+)" data-home-post-link/g)].map((m) => m[1]);
	if (home.join() !== listed.slice(0, 3).join()) errors.push(`index.html: the dev log section (${home}) is not the newest posts (${listed.slice(0, 3)})`);
}

// Discord (src/data/discord.ts): every page's header and the home page link to /discord/, never to Discord itself
// (the visitor statistics count the clicks as views of /discord/), and the page says the server is coming soon or
// sends visitors on to its invite.
for (const f of html) {
	if (relative(dist, f) !== '404.html' && !/<a href="\/discord\/" rel="me"/.test(readFileSync(f, 'utf8'))) errors.push(`${relative(dist, f)}: no Discord link in the header`);
}
if (!/<a [^>]*href="\/discord\/"[^>]*data-home-discord/.test(home)) errors.push('index.html: no Discord link');
const discordFile = join(dist, 'discord', 'index.html');
if (!existsSync(discordFile)) errors.push('discord/index.html: missing');
else {
	const page = readFileSync(discordFile, 'utf8');
	const to = page.match(/<meta http-equiv="refresh" content="0; url=([^"]*)"/)?.[1];
	const invite = readFileSync(new URL('../src/data/discord.ts', import.meta.url), 'utf8').match(/^export const invite = '([^']*)';$/m);
	if (!invite) errors.push('src/data/discord.ts: no `export const invite = \'...\';` line');
	else if (invite[1] === '' && (!/data-discord-soon/.test(page) || to)) errors.push('discord/index.html: no invite, but the page does not say coming soon');
	else if (invite[1] !== '' && (to !== invite[1] || !page.includes(`<a href="${to}">`) || /data-discord-soon/.test(page))) {
		errors.push(`discord/index.html: sends visitors to ${to}, not to the invite ${invite[1]}`);
	}
}
for (const f of html) {
	if (/\shref="https:\/\/(discord\.(gg|com)|discordapp\.com)\//.test(readFileSync(f, 'utf8')) && relative(dist, f) !== join('discord', 'index.html')) {
		errors.push(`${relative(dist, f)}: links to Discord itself, not to /discord/`);
	}
}

// Science (/science/tauceti/, a finished project): the data the build renders holds public fields
// only (the ledger it comes from has internal notes), the page and the home page agree with
// it, and it is published as it is committed, at /science/tauceti.json (the gateway no longer has it).
const sciFile = new URL('../src/data/science/tauceti.json', import.meta.url);
if (!existsSync(sciFile)) errors.push('src/data/science/tauceti.json: missing');
else if (!existsSync(join(dist, 'science', 'tauceti.json'))) errors.push('science/tauceti.json: not published');
else if (JSON.stringify(JSON.parse(readFileSync(join(dist, 'science', 'tauceti.json'), 'utf8'))) !== JSON.stringify(JSON.parse(readFileSync(sciFile, 'utf8'))))
	errors.push('science/tauceti.json: the published data differs from src/data/science/tauceti.json');
if (existsSync(sciFile)) {
	const sci = JSON.parse(readFileSync(sciFile, 'utf8'));
	const allowed = {
		top: ['project', 'project_url', 'repository', 'model', 'generated_at', 'states_as_of', 'methodology', 'type_labels', 'totals', 'project_progress', 'prs',
			'tasks_without_pr'],
		pr: ['number', 'task', 'url', 'title', 'type', 'size', 'state', 'opened', 'merged', 'closed', 'fix_rounds', 'internal_rounds', 'first_review_approved',
			'review_url', 'rounds', 'attempts', 'qwen_tokens', 'qwen_output_tokens', 'gpu_minutes', 'attempts_without_tokens',
			'attempts_without_gpu_time', 'review_qwen_tokens', 'review_gpu_minutes', 'reviews_left_out'],
		round: ['round', 'commit', 'at', 'result', 'changes'],
		change: ['rubric', 'verdict', 'reason', 'url'],
		task: ['task', 'target', 'type', 'size', 'state', 'internal_rounds', 'last_attempt', 'attempts', 'qwen_tokens', 'qwen_output_tokens', 'gpu_minutes',
			'attempts_without_tokens', 'attempts_without_gpu_time', 'review_qwen_tokens', 'review_gpu_minutes', 'reviews_left_out'],
	};
	const only = (obj, keys, what) => { for (const k of Object.keys(obj ?? {})) if (!keys.includes(k)) errors.push(`science/tauceti.json: unexpected ${what} field ${k}`); };
	only(sci, allowed.top, 'top-level');
	for (const p of sci.prs ?? []) {
		only(p, allowed.pr, 'PR');
		for (const r of p.rounds ?? []) {
			only(r, allowed.round, 'round');
			for (const c of r.changes ?? []) {
				only(c, allowed.change, 'change');
				if (!/^https:\/\/github\.com\/[^/]+\/[^/]+\/pull\/\d+#(discussion_r|issuecomment-)\d+$/.test(c.url)) errors.push(`science/tauceti.json: #${p.number} links ${c.url}`);
			}
		}
	}
	for (const t of sci.tasks_without_pr ?? []) only(t, allowed.task, 'task');
	for (const [k, v] of Object.entries(sci.totals ?? {})) if (v !== null && typeof v !== 'number') errors.push(`science/tauceti.json: totals.${k} is not a number`);
	if (/claude|opus|anthropic|codex|gpt-|gemini|\/home\/|raw\/|prompts?\//i.test(JSON.stringify(sci))) errors.push('science/tauceti.json: mentions internal tooling, reviewer models or paths');
	const page = readFileSync(join(dist, 'science', 'tauceti', 'index.html'), 'utf8');
	for (const p of sci.prs ?? []) {
		for (const k of ['qwen_tokens', 'qwen_output_tokens', 'gpu_minutes', 'attempts']) {
			if (p[k] !== null && p[k] !== undefined && (typeof p[k] !== 'number' || p[k] < 0)) errors.push(`science/tauceti.json: #${p.number} ${k} is not a number`);
		}
		if (p.qwen_tokens === 0 || p.gpu_minutes === 0) errors.push(`science/tauceti.json: #${p.number} shows 0 for a value (unknown must be null)`);
	}
	const rows = [...page.matchAll(/<tr data-pr="(\d+)"/g)].map((m) => Number(m[1])).sort();
	if (rows.join() !== sci.prs.map((p) => p.number).sort().join()) errors.push(`science/tauceti/: the table (${rows}) is not the JSON's PRs`);
	const noPR = [...page.matchAll(/<tr data-no-pr/g)].length;
	if (noPR !== (sci.tasks_without_pr ?? []).length) errors.push(`science/tauceti/: ${noPR} rows without a PR, the JSON has ${(sci.tasks_without_pr ?? []).length}`);
	const tokenSum = [...sci.prs, ...(sci.tasks_without_pr ?? [])].reduce((a, p) => a + (p.qwen_tokens ?? 0), 0);
	if (sci.totals?.qwen_tokens !== undefined && tokenSum !== sci.totals.qwen_tokens) errors.push(`science/tauceti.json: row tokens sum to ${tokenSum}, totals say ${sci.totals.qwen_tokens}`);
	const merged = sci.prs.filter((p) => p.state === 'merged').length;
	const shown = readFileSync(join(dist, 'index.html'), 'utf8').match(/data-home-science-merged[^>]*>(\d+)</);
	if (merged > 0 && Number(shown?.[1]) !== merged) errors.push(`index.html: the science section shows ${shown?.[1]} merged, the JSON ${merged}`);
}

// Search engines and AI answer engines (src/data/seo.ts, astro.config.mjs). The deploy builds with
// PUBLIC_DOMAIN (staging.aisathome.org on staging), and every absolute address follows it.
const origin = `https://${process.env.PUBLIC_DOMAIN || 'aisathome.org'}`;
const exists = (url) => {
	const t = join(dist, new URL(url).pathname);
	return existsSync(t) && (statSync(t).isFile() || existsSync(join(t, 'index.html')));
};
const robotsFile = join(dist, 'robots.txt');
if (!existsSync(robotsFile)) errors.push('robots.txt: missing');
else for (const p of robotsProblems(readFileSync(robotsFile, 'utf8'), origin)) errors.push(`robots.txt: ${p}`);
// The sitemap robots.txt names (Caddy redirects /sitemap.xml to it): every public page, nothing else.
const locs = (f) => [...readFileSync(f, 'utf8').matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
const indexFile = join(dist, 'sitemap-index.xml');
if (!existsSync(indexFile)) errors.push('sitemap-index.xml: missing');
else {
	const maps = locs(indexFile);
	if (!maps.length) errors.push('sitemap-index.xml: lists no sitemap');
	const pages = [];
	for (const m of maps) {
		if (!m.startsWith(`${origin}/`) || !exists(m)) errors.push(`sitemap-index.xml: ${m} is not a file of this build`);
		else pages.push(...locs(join(dist, new URL(m).pathname)));
	}
	const want = html.map((f) => relative(dist, f)).filter((f) => f !== '404.html').map((f) => `${origin}/${f.replace(/index\.html$/, '')}`).sort();
	const got = [...pages].sort();
	for (const u of got) if (!want.includes(u)) errors.push(`sitemap: ${u} is not a page of this build`);
	for (const u of want) if (!got.includes(u)) errors.push(`sitemap: ${u} is missing`);
}
const llmsFile = join(dist, 'llms.txt');
if (!existsSync(llmsFile)) errors.push('llms.txt: missing');
else {
	const llms = readFileSync(llmsFile, 'utf8');
	for (const p of llmsProblems(llms, origin)) errors.push(`llms.txt: ${p}`);
	for (const [, u] of llms.matchAll(/\]\((https:\/\/[^)\s]+)\)/g)) if (u.startsWith(`${origin}/`) && !exists(u)) errors.push(`llms.txt: broken link ${u}`);
}
// The share image: on every page (the site's, or a proved theorem's own card), with its true size, and a description of
// its own on each page.
const meta = (text, key) => text.match(new RegExp(`<meta (?:name|property)="${key.replace(/[.:]/g, '\\$&')}" content="([^"]*)"`))?.[1];
const ogFile = join(dist, 'og.png');
const og = existsSync(ogFile) ? pngSize(readFileSync(ogFile)) : null;
if (!og) errors.push('og.png: missing or not a PNG');
else if (statSync(ogFile).size > 1_000_000) errors.push('og.png: over 1 MB');
const titles = new Map();
const descriptions = new Map();
for (const f of html) {
	const page = relative(dist, f);
	if (page === '404.html') continue;
	const text = readFileSync(f, 'utf8');
	// the site's card, or a card of the page's own that the build drew (a proved theorem's: og-card.ts) at the same size
	const image = meta(text, 'og:image') ?? '';
	const own = image !== `${origin}/og.png` && /^\/science\/prove2me\/theorems\/([a-z0-9-]+\/)?og\.png$/.test(image.slice(origin.length)) && image.startsWith(`${origin}/`);
	const ownFile = own ? join(dist, image.slice(origin.length)) : '';
	const size = own ? (existsSync(ownFile) ? pngSize(readFileSync(ownFile)) : null) : og;
	if (image !== `${origin}/og.png` && !own) errors.push(`${page}: og:image is ${image}`);
	if (own && !size) errors.push(`${page}: og:image ${image} is not a PNG in the build`);
	if (size && (meta(text, 'og:image:width') !== String(size.width) || meta(text, 'og:image:height') !== String(size.height))) errors.push(`${page}: og:image size is not the PNG's ${size.width}x${size.height}`);
	if (own && size && statSync(ownFile).size > 1_000_000) errors.push(`${page}: og:image ${image} is over 1 MB`);
	if (!meta(text, 'og:image:alt')) errors.push(`${page}: no og:image:alt`);
	const title = text.match(/<title>([^<]*)<\/title>/)?.[1] ?? '';
	if ((title.match(/AIs@home/g) ?? []).length > 1 && page !== 'index.html') errors.push(`${page}: the title names the brand twice: ${title}`);
	const desc = meta(text, 'description') ?? '';
	if (desc.length < 50) errors.push(`${page}: no description of its own (${desc})`);
	for (const [m, v] of [[titles, title], [descriptions, desc]]) {
		if (m.has(v)) errors.push(`${page}: the same ${m === titles ? 'title' : 'description'} as ${m.get(v)}`);
		m.set(v, page);
	}
}
if (home.match(/<title>([^<]*)<\/title>/)?.[1].match(/AIs@home/g)?.length !== 1) errors.push('index.html: the title does not name the brand exactly once');
const ld = [...home.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)];
if (ld.length !== 1) errors.push(`index.html: ${ld.length} JSON-LD scripts (one expected)`);
else {
	try {
		const data = JSON.parse(ld[0][1]);
		for (const p of jsonLdProblems(data, origin)) errors.push(`index.html: JSON-LD: ${p}`);
		const logo = data['@graph']?.find((n) => n['@type'] === 'Organization')?.logo?.url;
		if (logo && !exists(logo)) errors.push(`index.html: JSON-LD logo ${logo} is not in the build`);
	} catch (e) {
		errors.push(`index.html: JSON-LD does not parse: ${e.message}`);
	}
}

// /node/: the node software's JSON-LD, once, and the page's own start (the one-liner, the same as the home
// page's, and the Windows button).
const nodePage = readFileSync(join(dist, 'node', 'index.html'), 'utf8');
const nodeLd = [...nodePage.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)];
if (nodeLd.length !== 1) errors.push(`node/index.html: ${nodeLd.length} JSON-LD scripts (one expected)`);
else {
	try {
		for (const p of nodeJsonLdProblems(JSON.parse(nodeLd[0][1]), origin)) errors.push(`node/index.html: JSON-LD: ${p}`);
	} catch (e) {
		errors.push(`node/index.html: JSON-LD does not parse: ${e.message}`);
	}
}
// The proved theorems (src/data/theorems-jsonld.ts): the list's Dataset and each proof page's CreativeWork, once each.
const theoremsDir = join(dist, 'science', 'prove2me', 'theorems');
if (existsSync(join(theoremsDir, 'index.html'))) {
	const one = (page, check) => {
		const lds = jsonLdScripts(readFileSync(join(dist, page), 'utf8'));
		if (lds.length !== 1) errors.push(`${page}: ${lds.length} JSON-LD scripts (one expected)`);
		else if (lds[0] instanceof Error) errors.push(`${page}: JSON-LD does not parse: ${lds[0].message}`);
		else for (const p of check(lds[0])) errors.push(`${page}: JSON-LD: ${p}`);
	};
	one('science/prove2me/theorems/index.html', (d) => datasetJsonLdProblems(d, origin));
	// the records' licence (src/data/licence.ts): on the page and in each JSON file the site publishes them in
	if (!readFileSync(join(theoremsDir, 'index.html'), 'utf8').match(/<p data-th-licence>[^<]*Apache License 2\.0/)) errors.push('science/prove2me/theorems/index.html: no licence line');
	const leafJSON = readdirSync(theoremsDir).filter((f) => f.endsWith('.json')).map((f) => `science/prove2me/theorems/${f}`);
	for (const f of ['science/prove2me/theorems.json', 'science/prove2me.json', ...leafJSON]) {
		const lic = existsSync(join(dist, f)) ? JSON.parse(readFileSync(join(dist, f), 'utf8')).license : undefined;
		if (lic?.spdx !== 'Apache-2.0' || lic?.url !== 'https://www.apache.org/licenses/LICENSE-2.0') errors.push(`${f}: no Apache-2.0 license field`);
	}
	for (const leaf of readdirSync(theoremsDir).filter((f) => existsSync(join(theoremsDir, f, 'index.html')))) {
		one(`science/prove2me/theorems/${leaf}/index.html`, (d) => proofJsonLdProblems(d, origin, leaf));
	}
}
const command = (text) => text.match(/data-node-start-linux[\s\S]*?<pre[\s\S]*?<\/pre>/)?.[0].replace(/<[^>]+>/g, '').replace(/\s+/g, ' ');
if (!command(nodePage)?.includes('node.sh | sh')) errors.push('node/index.html: the start does not show the one-line install');
else if (command(nodePage) !== command(home)) errors.push('node/index.html: the one-line install differs from the home page\'s');

// The proved theorems page is easy to find (2026-11-28): the prove2me page, /science/ and the home page link it, the
// theorems page links back to the work, and the prove2me page leads with the theorems page's headline number.
const page = (f) => (existsSync(join(dist, f)) ? readFileSync(join(dist, f), 'utf8') : '');
const theoremsHref = 'href="/science/prove2me/theorems/"';
for (const [f, sel] of [['science/prove2me/index.html', 'data-p2m-theorems-link="headline"'], ['science/index.html', 'data-card-link="theorems"'], ['index.html', 'data-home-theorems-link']])
	if (!new RegExp(`<a ${theoremsHref} ${sel}`).test(page(f))) errors.push(`${f}: no link to the proved theorems page (${sel})`);
const thPage = page('science/prove2me/theorems/index.html');
if (!/<a href="\/science\/prove2me\/" data-th-work-link/.test(thPage)) errors.push('science/prove2me/theorems/index.html: no link back to the prove2me page');
const lead = (text, re) => text.match(re)?.[1];
const thN = lead(thPage, /data-th-headline-n>(\d+)</), p2mN = lead(page('science/prove2me/index.html'), /data-p2m-hl-n>(\d+)</);
if (!thN || thN !== p2mN) errors.push(`science/prove2me/: its headline (${p2mN}) is not the proved theorems page's (${thN})`);

if (errors.length) {
	console.error(errors.join('\n'));
	console.error(`\n${errors.length} problem(s) in ${html.length} pages`);
	process.exit(1);
}
console.log(`site check ok: ${html.length} pages, ${css.length} stylesheets`);
console.log(`theme script CSP hash: 'sha256-${createHash('sha256').update(themeSync).digest('base64')}'`);
