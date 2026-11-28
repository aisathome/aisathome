// The home page's prove2me chart (src/data/prove2me-chart.ts) on fixed data: the build's committed history and the
// gateway's records as served on 2026-11-18 (tests/fixtures), 8 accepted results by then: 6 proofs and 2 reuses.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { historyOf, parseCampaigns, parseResults, scienceView, type Records } from '../src/data/prove2me-records';
import { acceptedChartSVG, acceptedRowsHTML, acceptedSeries, acceptedSummary, mountAcceptedChart, shortDay } from '../src/data/prove2me-chart';

const h = historyOf(raw as unknown as Prove2me);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: fixture.tasks.tasks as unknown as PublicTask[] },
});

test('the series is every accepted proof by the model (no reuse), oldest first, from the first trial to the data’s time', () => {
	const v = scienceView(h, records());
	const s = acceptedSeries(v)!;
	assert.equal(v.kpis.reused, 2);
	assert.equal(s.accepted.length, v.kpis.proofs);
	assert.equal(s.accepted.length, v.kpis.accepted - v.kpis.reused);
	const reuseTitles = v.submissions.filter((x) => x.kind === 'reuse').map((x) => x.title);
	assert.ok(!s.accepted.some((a) => reuseTitles.includes(a.title)));
	assert.ok(s.accepted.every((a, i) => i === 0 || Date.parse(s.accepted[i - 1].at) <= Date.parse(a.at)));
	assert.equal(s.start, v.trials.find((t) => !t.probe)!.start);
	assert.ok(Date.parse(s.end) >= Date.parse(s.accepted[s.accepted.length - 1].at), 'the line stops before the last proof');
	assert.ok(!s.trials.some((t) => Number.isNaN(Date.parse(t.start))));
	assert.deepEqual(s.trials.map((t) => t.number), [...new Set(v.trials.filter((t) => !t.probe).map((t) => t.number))].sort((a, b) => a - b));
});

test('the build’s own history draws too, and nothing accepted draws nothing', () => {
	assert.equal(acceptedSeries(scienceView(h, null))!.accepted.length, scienceView(h, null).kpis.proofs);
	const v = scienceView(h, null);
	assert.equal(acceptedSeries({ ...v, submissions: v.submissions.map((x) => ({ ...x, verdict: 'pending' })) }), null);
});

test('the SVG: one point per proof, the count at the end, sized as asked, no projection', () => {
	const s = acceptedSeries(scienceView(h, records()))!;
	const svg = acceptedChartSVG(s, 400);
	assert.match(svg, /^<svg viewBox="0 0 400 200" width="400" height="200" role="img" aria-label="[^"]+"/);
	assert.equal((svg.match(/<circle /g) ?? []).length, s.accepted.length);
	assert.doesNotMatch(svg, /class="pt reuse"|closed by reusing/);
	assert.match(svg, new RegExp(`class="tick end-n">${s.accepted.length}<`));
	// the step line ends at the right edge of the plot (the data's time), never beyond
	const pts = svg.match(/<polyline points="([^"]+)"/)![1].split(' ').map((p) => Number(p.split(',')[0]));
	assert.ok(Math.max(...pts) <= 400 - 30 + 0.01);
	assert.ok(pts.every((x, i) => i === 0 || x >= pts[i - 1]), 'the line goes back in time');
	assert.ok(svg.includes(`>${shortDay(s.start)}</text>`) && svg.includes(`>${shortDay(s.end)}</text>`));
	assert.equal(acceptedChartSVG(s, 10).slice(0, 30), '<svg viewBox="0 0 240 200" wid');
});

test('titles and links are escaped; the table lists newest first with the running total', () => {
	const s = acceptedSeries(scienceView(h, records()))!;
	const evil = { ...s, accepted: [...s.accepted, { at: s.end, trial: 9, title: '<img src=x onerror=alert(1)>', url: 'https://prove2.me/theorems/"x' }] };
	const svg = acceptedChartSVG(evil);
	const rows = acceptedRowsHTML(evil);
	assert.ok(!svg.includes('<img') && !rows.includes('<img') && !rows.includes('/"x'));
	const totals = [...acceptedRowsHTML(s).matchAll(/<td class="num">(\d+)<\/td><\/tr>/g)].map((m) => Number(m[1]));
	assert.deepEqual(totals, s.accepted.map((_, i) => s.accepted.length - i));
	assert.match(acceptedSummary(s), new RegExp(`^${s.accepted.length} proofs by the model accepted by prove2me from .*\\. Leaves closed by reuse are not counted\\.$`));
});

/** The figure of components/AcceptedChart.astro as far as mountAcceptedChart touches it: the SVG box, the table body
 * and the build's series; `hidden` as the build left it. */
function fakeFigure(series: unknown, hidden: boolean) {
	const box = { innerHTML: series ? '<svg>build</svg>' : '', clientWidth: 500 };
	const rows = { innerHTML: series ? '<tr>build</tr>' : '' };
	const json = { textContent: JSON.stringify(series) };
	const fig = {
		hidden,
		dataset: {} as Record<string, string>,
		querySelector: (sel: string) => ({ '[data-p2m-chart-svg]': box, '[data-p2m-chart-rows]': rows, '[data-p2m-chart-series]': json })[sel] ?? null,
	};
	return { fig, box, rows };
}

test('live: a read with nothing accepted hides the chart (never the old proofs under a zero); a page built with none fills it', () => {
	const g = globalThis as unknown as { window?: unknown; requestAnimationFrame?: unknown; cancelAnimationFrame?: unknown };
	const had = { window: g.window, raf: g.requestAnimationFrame, caf: g.cancelAnimationFrame };
	g.window = { addEventListener: () => {} };
	g.requestAnimationFrame = () => 0;
	g.cancelAnimationFrame = () => {};
	try {
		const v = scienceView(h, records());
		const none = { ...v, submissions: v.submissions.map((x) => ({ ...x, verdict: 'pending' })) };
		// built with proofs; the gateway then has none accepted: hidden and emptied, not the build's proofs
		const a = fakeFigure(acceptedSeries(scienceView(h, null)), false);
		const chartA = mountAcceptedChart(a.fig as unknown as HTMLElement);
		chartA.update(none);
		assert.deepEqual([a.fig.hidden, a.box.innerHTML, a.rows.innerHTML, a.fig.dataset.source], [true, '', '', 'live']);
		// the next read has them again: shown and drawn at the box's width
		chartA.update(v);
		assert.equal(a.fig.hidden, false);
		assert.match(a.box.innerHTML, /^<svg viewBox="0 0 500 200"/);
		assert.equal((a.box.innerHTML.match(/<circle /g) ?? []).length, v.kpis.proofs);
		assert.equal((a.rows.innerHTML.match(/<tr>/g) ?? []).length, v.kpis.proofs);
		// built with nothing accepted: the figure is there, hidden, and the live records fill it
		const b = fakeFigure(null, true);
		const chartB = mountAcceptedChart(b.fig as unknown as HTMLElement);
		assert.equal(b.box.innerHTML, '');
		chartB.update(v);
		assert.equal(b.fig.hidden, false);
		assert.equal((b.box.innerHTML.match(/<circle /g) ?? []).length, v.kpis.proofs);
	} finally {
		g.window = had.window;
		g.requestAnimationFrame = had.raf;
		g.cancelAnimationFrame = had.caf;
	}
});
