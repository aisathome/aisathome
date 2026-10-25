// Checks the built site (dist/) in CI: internal links resolve, nothing loads from a third
// party, the live network strip and the leaderboard start hidden (they show themselves
// only with real data), and every page shares the theme choice with the account pages (the
// theme script runs before Starlight's). Also the dev log: drafts and the feed (below).
// And the science data: public fields only, matching the page and the home page.
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

// Science (/science/tauceti/): the published JSON holds public fields only (the research ledger it
// comes from has internal notes), and the page and the home page agree with it.
const sciFile = join(dist, 'science', 'tauceti.json');
if (!existsSync(sciFile)) errors.push('science/tauceti.json: missing');
else {
	const sci = JSON.parse(readFileSync(sciFile, 'utf8'));
	const allowed = {
		top: ['project', 'project_url', 'repository', 'model', 'generated_at', 'states_as_of', 'methodology', 'type_labels', 'totals', 'prs',
			'tasks_without_pr'],
		pr: ['number', 'url', 'title', 'type', 'size', 'state', 'opened', 'merged', 'closed', 'fix_rounds', 'first_review_approved',
			'review_url', 'rounds', 'attempts', 'qwen_tokens', 'qwen_output_tokens', 'gpu_minutes', 'attempts_without_tokens',
			'attempts_without_gpu_time'],
		round: ['round', 'commit', 'at', 'result', 'changes'],
		change: ['rubric', 'verdict', 'reason', 'url'],
		task: ['target', 'type', 'size', 'state', 'last_attempt', 'attempts', 'qwen_tokens', 'qwen_output_tokens', 'gpu_minutes',
			'attempts_without_tokens', 'attempts_without_gpu_time'],
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

if (errors.length) {
	console.error(errors.join('\n'));
	console.error(`\n${errors.length} problem(s) in ${html.length} pages`);
	process.exit(1);
}
console.log(`site check ok: ${html.length} pages, ${css.length} stylesheets`);
console.log(`theme script CSP hash: 'sha256-${createHash('sha256').update(themeSync).digest('base64')}'`);
