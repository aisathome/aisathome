// One count of proofs on every page that shows one: the proved theorems page, the prove2me page, the home page's line
// and the /science/ card. A proof is a submission by the model that prove2me accepted; a reuse is accepted too but is
// never a proof. The numbers are read back from the rendered text (not from the view), on the build's render and on the
// browser's live redraw (scripts/theorems-live.ts run against a fake page and a stubbed gateway), and must agree:
// the headline is the accepted proofs, the KPIs, the submissions line, the charts, the costs and the list derive from the
// same counts, and a reuse is never in a proof count.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import snapshot from '../src/data/science/prove2me-records.json';
import type { Prove2me } from '../src/data/prove2me';
import { historyOf, liveSummary, scienceView, type History, type Records } from '../src/data/prove2me-records';
import { recordsOf } from '../src/data/prove2me-build';
import { acceptedChartSVG } from '../src/data/prove2me-chart';
import { cardNumbers, homeLine, kpisHTML } from '../src/data/prove2me-view';
import { costsHTML, headlineHTML, listCountText, theoremKpisHTML, theoremRowsHTML, theoremsView, trialBarsSVG } from '../src/data/prove2me-theorems';
import { liveTheorems } from '../src/scripts/theorems-live';

const MODEL = 'Qwen3.8 27B';
const h = historyOf(raw as unknown as Prove2me);
const text = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim();
const num = (s: string, re: RegExp, what: string) => {
	const m = re.exec(s);
	assert.ok(m, `${what}: no match for ${re} in ${JSON.stringify(s.slice(0, 400))}`);
	return Number(m[1]);
};
const kpi = (html: string, key: string) => {
	const m = new RegExp(`<div data-kpi="${key}"><dt>(.*?)</dt><dd>(\\d+)</dd></div>`).exec(html);
	assert.ok(m, `no KPI ${key} in ${html}`);
	return { dt: text(m[1]), dd: Number(m[2]) };
};
/** "59 accepted proofs, 3 reuses, 1 rejected, 1 not judged" by what became of each submission. */
const parts = (s: string) => {
	const out: Record<string, number> = { proofs: 0, reuses: 0, rejected: 0, judged: 0, waiting: 0 };
	for (const p of s.split(', ')) {
		const m = /^(\d+) (accepted proofs?|reuses?|rejected|not judged|waiting)$/.exec(p);
		assert.ok(m, `submission part ${JSON.stringify(p)}`);
		out[m[2].startsWith('accepted') ? 'proofs' : m[2].startsWith('reuse') ? 'reuses' : m[2] === 'not judged' ? 'judged' : m[2]] = Number(m[1]);
	}
	return out;
};

/** What the proved theorems page shows, as its parts' HTML (the build's render or the live redraw). */
interface TheoremsPage { headline: string; kpis: string; chart: string; bars: string; costs: string; count: string; rows: string }

/** Every number of the page read back from its text, checked against each other; returns the proof count and the
 * reuses. */
function checkTheoremsPage(p: TheoremsPage) {
	const n = num(p.headline, /data-th-headline-n>(\d+)</, 'headline');
	const scope = text(p.headline);
	const sm = /Proofs of (\d+) of the (\d+) open leaves tried\. (\d+) submissions?: (.*?)(?:; a reuse is not counted as a proof)?\. From trial/.exec(scope);
	assert.ok(sm, `scope line: ${scope}`);
	const [openAcc, tried, submitted] = [Number(sm[1]), Number(sm[2]), Number(sm[3])];
	const sub = parts(sm[4]);
	// the list: its rows, proofs and reuses apart
	const proofRows = (p.rows.match(/data-kind="proof"/g) ?? []).length;
	const reuseRows = (p.rows.match(/data-kind="reuse"/g) ?? []).length;
	assert.equal(n, proofRows, 'the headline is the list’s proofs (reuses left out)');
	assert.equal(sub.proofs, n, 'the submissions line’s accepted proofs are the headline');
	assert.equal(sub.reuses, reuseRows, 'the submissions line’s reuses are the list’s');
	assert.equal(sub.proofs + sub.reuses + sub.rejected + sub.judged + sub.waiting, submitted, 'every submission counted once');
	assert.ok(openAcc <= n && tried >= openAcc, `open leaves with an accepted proof ${openAcc} of ${tried}, proofs ${n}`);
	// the KPIs: the same counts
	const open = kpi(p.kpis, 'open'), subs = kpi(p.kpis, 'submitted');
	assert.equal(open.dd, openAcc);
	assert.equal(num(open.dt, /of (\d+) tried/, 'open KPI'), tried);
	assert.equal(subs.dd, submitted);
	assert.deepEqual(parts(subs.dt.replace(/^submissions: /, '')), sub);
	if (reuseRows) {
		const r = kpi(p.kpis, 'reused');
		assert.equal(r.dd, reuseRows);
		assert.match(r.dt, /not counted as proofs/);
	} else assert.doesNotMatch(p.kpis, /data-kpi="reused"/);
	// the chart of accepted proofs: one point each, the count at the end, its text alternative
	assert.equal((p.chart.match(/<circle /g) ?? []).length, n, 'the chart’s points are the proofs');
	assert.equal(num(p.chart, /class="tick end-n">(\d+)</, 'chart end'), n);
	assert.equal(num(p.chart, /aria-label="(\d+) proofs? by the model accepted/, 'chart label'), n);
	assert.doesNotMatch(p.chart, /reuse"|closed by reusing/);
	// the per-trial bars add up to the headline
	assert.equal(num(p.bars, /: (\d+) accepted of \d+ open leaves tried in all/, 'bars'), n);
	// the costs: all-in over every trial is over the headline's proofs; the median names how many of them it counts
	const costs = text(p.costs);
	assert.equal(num(costs, /since the first: [\d.]+ GPU-hours over (\d+) accepted proofs?/, 'all-in'), n);
	const med = /; (?:(\d+) of the (\d+) proofs|(\d+) proofs?), trials? /.exec(costs);
	assert.ok(med, `median scope: ${costs}`);
	if (med[3]) assert.equal(Number(med[3]), n);
	else {
		assert.equal(Number(med[2]), n, 'the median says out of how many proofs');
		assert.ok(Number(med[1]) < n);
		assert.match(costs, /left out|without a recorded run/, 'the median says why its count is smaller');
	}
	// the line over the list
	const cm = /^(\d+) lea(?:f|ves), newest first: (\d+) proved by the model(?:, (\d+) closed by reuse)?\.$/.exec(text(p.count));
	assert.ok(cm, `count line: ${p.count}`);
	assert.deepEqual([Number(cm[1]), Number(cm[2]), Number(cm[3] ?? 0)], [proofRows + reuseRows, n, reuseRows]);
	return { n, reused: reuseRows, openAcc, tried, submitted };
}

/** The prove2me page's KPIs, the home page's count and line, the /science/ card, against the theorems page's counts. */
function checkOtherPages(c: { n: number; reused: number; openAcc: number; tried: number; submitted: number }, k: { kpisHtml: string; homeCount: number; homeLine: string; card: { key: string; value: string }[] }) {
	const acc = kpi(k.kpisHtml, 'accepted');
	assert.equal(acc.dd, c.n, 'prove2me page: proofs accepted are the theorems page’s headline');
	assert.match(acc.dt, new RegExp(`of ${c.submitted} submitted${c.reused ? ` \\(${c.reused} of them reuses?\\)` : ''}$`));
	assert.equal(kpi(k.kpisHtml, 'open').dd, c.openAcc);
	assert.match(kpi(k.kpisHtml, 'open').dt, new RegExp(`of ${c.tried} tried`));
	if (c.reused) assert.equal(kpi(k.kpisHtml, 'reused').dd, c.reused);
	assert.equal(k.homeCount, c.n, 'home page: its count is the proofs');
	assert.match(k.homeLine, new RegExp(`it proved ${c.openAcc} of the ${c.tried} open lemmas it tried${c.reused ? ` and closed ${c.reused} more by reusing` : '\\.'}`));
	assert.equal(k.card.find((x) => x.key === 'accepted')!.value, String(c.n));
	assert.equal(k.card.find((x) => x.key === 'open')!.value, `${c.openAcc} of ${c.tried}`);
}

/** The accepted proofs counted from the raw records and the history, not by the page's functions: the gateway's
 * accepted results without a reuse record, and the history's accepted proofs of leaves the gateway does not hold. */
function rawProofs(hist: History, r: Records) {
	const held = new Set(r.results.map((x) => x.item));
	const gw = r.results.filter((x) => x.state === 'accepted' && !x.reuse && x.labels.kind !== 'reuse').length;
	return gw + hist.submissions.filter((s) => !held.has(s.leaf) && s.verdict === 'accepted' && s.kind === 'proof').length;
}

/** The records as the live page will find them: newer than the build's, one rejected proof accepted since, and one
 * accepted proof recorded as a reuse since (as N18's backfill did on 2026-11-28, between a build and a live read). */
function newerRecords(): Records {
	const r = recordsOf(snapshot)!;
	const rejected = r.results.find((x) => x.state === 'rejected')!;
	const proof = r.results.find((x) => x.state === 'accepted' && !x.reuse && x.campaign === 't12')!;
	return {
		...r,
		results: r.results.map((x) =>
			x === rejected ? { ...x, state: 'accepted' as const, verdict_at: x.verdict_at ?? x.submitted_at }
			: x === proof ? { ...x, reuse: { name: 'Someone.else_theorem', author: 'someone', relation: 'same' }, labels: { ...x.labels, kind: 'reuse', reuses: 'Someone.else_theorem' } }
			: x),
	};
}

test('the build’s render: one count of proofs on the theorems page, the prove2me page, the home page and /science/', () => {
	const r = recordsOf(snapshot)!;
	const v = theoremsView(h, r);
	// the parts as pages/science/prove2me/theorems/index.astro renders them
	const c = checkTheoremsPage({
		headline: headlineHTML(v, MODEL), kpis: theoremKpisHTML(v).html, chart: acceptedChartSVG(v.series!), bars: trialBarsSVG(v.perTrial),
		costs: costsHTML(v), count: listCountText(v), rows: theoremRowsHTML(v, new Set()),
	});
	assert.equal(c.n, rawProofs(h, r), 'the headline is the accepted proofs counted from the raw records');
	assert.equal(c.reused, 3);
	assert.equal(c.openAcc, c.n, 'every accepted proof is of an open leaf tried');
	const sv = scienceView(h, r);
	checkOtherPages(c, { kpisHtml: kpisHTML(sv.kpis).html, homeCount: sv.kpis.proofs, homeLine: homeLine(sv, MODEL), card: cardNumbers(sv) });
});

/** An element of the fake page: what the live scripts read and write. */
class El {
	innerHTML = '';
	textContent = '';
	hidden = false;
	dataset: Record<string, string> = {};
	style = { setProperty: () => {} };
	clientWidth = 640;
	kids: Record<string, El> = {};
	querySelector(sel: string) {
		return this.kids[sel] ?? null;
	}
	querySelectorAll(sel: string) {
		return this.kids[sel] ? [this.kids[sel]] : [];
	}
}

test('the live redraw: the page’s script draws the same one count from newer records, and so do the home page and /science/', async () => {
	const live = newerRecords();
	// the page as the build left it: the parts the script fills, the known history and the chart's figures
	const page = new El();
	const sels = ['[data-th-kpis]', '[data-th-headline]', '[data-th-selection]', '[data-th-costs]', '[data-th-rows]', '[data-th-count]', '[data-th-updated]'];
	for (const s of sels) page.kids[s] = new El();
	page.kids['[data-th-known]'] = Object.assign(new El(), { textContent: JSON.stringify({ history: h, pages: [], model: MODEL }) });
	const chart = new El();
	for (const s of ['[data-p2m-chart-svg]', '[data-p2m-chart-rows]', '[data-p2m-chart-series]']) chart.kids[s] = new El();
	chart.kids['[data-p2m-chart-series]'].textContent = 'null';
	const bars = new El();
	for (const s of ['[data-th-bars-svg]', '[data-th-bars-rows]']) bars.kids[s] = new El();
	page.kids['[data-p2m-chart]'] = chart;
	page.kids['[data-th-bars]'] = bars;
	page.kids['[data-p2m-chart], [data-th-bars]'] = chart;
	// the gateway: its public records, page by page as it answers them
	const answer = (u: string) => {
		const path = new URL(u, 'https://x').pathname;
		const raw = snapshot as unknown as { campaigns: object; results: object; tasks: object };
		if (path === '/v1/projects/prove2me/campaigns') return { ...raw.campaigns, campaigns: live.campaigns };
		if (path === '/v1/projects/prove2me/results') return { ...raw.results, results: live.results };
		if (path === '/v1/tasks') return raw.tasks;
		throw new Error(`404 ${u}`);
	};
	const g = globalThis as unknown as Record<string, unknown>;
	const had = { document: g.document, window: g.window, fetch: g.fetch, setInterval: g.setInterval, raf: g.requestAnimationFrame, caf: g.cancelAnimationFrame };
	const fetch = async (u: string) => ({ ok: true, status: 200, json: async () => answer(u) });
	g.document = { querySelector: (s: string) => page.querySelector(s), querySelectorAll: (s: string) => page.querySelectorAll(s), visibilityState: 'visible' };
	g.window = { fetch, addEventListener: () => {} };
	g.fetch = fetch;
	g.setInterval = () => 0;
	g.requestAnimationFrame = () => 0;
	g.cancelAnimationFrame = () => {};
	try {
		liveTheorems();
		for (let i = 0; i < 200 && page.kids['[data-th-rows]'].dataset.source !== 'live'; i++) await new Promise((r) => setTimeout(r, 5));
		assert.equal(page.kids['[data-th-rows]'].dataset.source, 'live', 'the live redraw ran');
		const c = checkTheoremsPage({
			headline: page.kids['[data-th-headline]'].innerHTML, kpis: page.kids['[data-th-kpis]'].innerHTML, chart: chart.kids['[data-p2m-chart-svg]'].innerHTML,
			bars: bars.kids['[data-th-bars-svg]'].innerHTML, costs: page.kids['[data-th-costs]'].innerHTML, count: page.kids['[data-th-count]'].textContent,
			rows: page.kids['[data-th-rows]'].innerHTML,
		});
		// the newer records moved the counts: one proof more (accepted since), one fewer (now a reuse), one reuse more
		const built = theoremsView(h, recordsOf(snapshot)!).counts;
		assert.equal(c.n, rawProofs(h, live));
		assert.equal(c.n, built.proofs);
		assert.equal(c.reused, built.reused + 1);
		// the home page and /science/ redraw from liveSummary with the same records
		const sv = (await liveSummary(h, async (u) => answer(u)))!;
		checkOtherPages(c, { kpisHtml: kpisHTML(sv.kpis).html, homeCount: sv.kpis.proofs, homeLine: homeLine(sv, MODEL), card: cardNumbers(sv) });
	} finally {
		Object.assign(g, { document: had.document, window: had.window, fetch: had.fetch, setInterval: had.setInterval, requestAnimationFrame: had.raf, cancelAnimationFrame: had.caf });
	}
});

test('the checker catches the old mix: a KPI or a chart that counts a reuse as a proof fails', () => {
	const v = theoremsView(h, recordsOf(snapshot)!);
	const good: TheoremsPage = {
		headline: headlineHTML(v, MODEL), kpis: theoremKpisHTML(v).html, chart: acceptedChartSVG(v.series!), bars: trialBarsSVG(v.perTrial),
		costs: costsHTML(v), count: listCountText(v), rows: theoremRowsHTML(v, new Set()),
	};
	checkTheoremsPage(good);
	// the headline counting the reuses (accepted submissions) disagrees with the list's proofs
	assert.throws(() => checkTheoremsPage({ ...good, headline: good.headline.replace(`data-th-headline-n>${v.counts.proofs}<`, `data-th-headline-n>${v.counts.accepted}<`) }));
	// the open leaves from our own check, not prove2me's acceptance, disagree with the scope line
	assert.throws(() => checkTheoremsPage({ ...good, kpis: good.kpis.replace(`<dd>${v.counts.openAccepted}</dd>`, `<dd>${v.counts.openAccepted + 3}</dd>`) }));
	// a chart drawn with the reuses has more points than proofs
	const withReuses = acceptedChartSVG({ ...v.series!, accepted: [...v.series!.accepted, ...v.series!.accepted.slice(0, v.counts.reused)].sort((a, b) => a.at.localeCompare(b.at)) });
	assert.throws(() => checkTheoremsPage({ ...good, chart: withReuses }));
});
