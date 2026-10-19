// Checks the built site (dist/) in CI: internal links resolve, nothing loads from a third
// party, the live network strip and the leaderboard start hidden (they show themselves
// only with real data), and every page shares the theme choice with the account pages (the
// theme script runs before Starlight's). Also the dev log: drafts and the feed (below).
// SPDX-License-Identifier: Apache-2.0
import { readdirSync, readFileSync, existsSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { createHash } from 'node:crypto';

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
const rssFile = join(dist, 'devlog', 'rss.xml');
if (!existsSync(rssFile)) errors.push('devlog/rss.xml: missing');
else {
	const rss = readFileSync(rssFile, 'utf8');
	if (!rss.startsWith('<?xml') || !rss.includes('<rss version="2.0"')) errors.push('devlog/rss.xml: not an RSS 2.0 document');
	const fed = [...rss.matchAll(/<item>[\s\S]*?<link>([^<]+)<\/link>/g)].map((m) => new URL(m[1]).pathname);
	const listed = [...readFileSync(join(dist, 'devlog', 'index.html'), 'utf8').matchAll(/data-devlog-item[^>]*>[\s\S]*?<a href="([^"]+)"/g)].map((m) => m[1]);
	if (fed.join() !== listed.join()) errors.push(`devlog: the feed (${fed}) and the list page (${listed}) differ`);
	for (const p of fed) if (!existsSync(join(dist, p, 'index.html'))) errors.push(`devlog/rss.xml: ${p} does not exist`);
	// The home page's dev log box shows the newest three (or fewer) of the same posts.
	const home = [...readFileSync(join(dist, 'index.html'), 'utf8').matchAll(/<a href="([^"]+)" data-home-post-link/g)].map((m) => m[1]);
	if (home.join() !== listed.slice(0, 3).join()) errors.push(`index.html: the dev log box (${home}) is not the newest posts (${listed.slice(0, 3)})`);
}

if (errors.length) {
	console.error(errors.join('\n'));
	console.error(`\n${errors.length} problem(s) in ${html.length} pages`);
	process.exit(1);
}
console.log(`site check ok: ${html.length} pages, ${css.length} stylesheets`);
console.log(`theme script CSP hash: 'sha256-${createHash('sha256').update(themeSync).digest('base64')}'`);
