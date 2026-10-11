// Checks the built site (dist/) in CI: internal links resolve, nothing loads from a third
// party, and the live network strip and the leaderboard start hidden (they show themselves
// only with real data).
// SPDX-License-Identifier: Apache-2.0
import { readdirSync, readFileSync, existsSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

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

if (errors.length) {
	console.error(errors.join('\n'));
	console.error(`\n${errors.length} problem(s) in ${html.length} pages`);
	process.exit(1);
}
console.log(`site check ok: ${html.length} pages, ${css.length} stylesheets`);
