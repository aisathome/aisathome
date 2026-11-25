// The home page's prove2me chart (src/data/prove2me-chart.ts) on fixed data: the build's committed history and the
// gateway's records as served on 2026-11-18 (tests/fixtures), 8 accepted results by then.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { historyOf, parseCampaigns, parseResults, scienceView, type Records } from '../src/data/prove2me-records';
import { acceptedChartSVG, acceptedRowsHTML, acceptedSeries, acceptedSummary, shortDay } from '../src/data/prove2me-chart';

const h = historyOf(raw as unknown as Prove2me);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: fixture.tasks.tasks as unknown as PublicTask[] },
});

test('the series is every accepted submission, oldest first, from the first trial to the data’s time', () => {
	const v = scienceView(h, records());
	const s = acceptedSeries(v)!;
	assert.equal(s.accepted.length, v.kpis.accepted);
	assert.equal(s.accepted.filter((a) => a.kind === 'reuse').length, v.kpis.reused);
	assert.ok(s.accepted.every((a, i) => i === 0 || Date.parse(s.accepted[i - 1].at) <= Date.parse(a.at)));
	assert.equal(s.start, v.trials.find((t) => !t.probe)!.start);
	assert.ok(Date.parse(s.end) >= Date.parse(s.accepted[s.accepted.length - 1].at), 'the line stops before the last proof');
	assert.ok(!s.trials.some((t) => Number.isNaN(Date.parse(t.start))));
	assert.deepEqual(s.trials.map((t) => t.number), [...new Set(v.trials.filter((t) => !t.probe).map((t) => t.number))].sort((a, b) => a - b));
});

test('the build’s own history draws too, and nothing accepted draws nothing', () => {
	assert.equal(acceptedSeries(scienceView(h, null))!.accepted.length, scienceView(h, null).kpis.accepted);
	const v = scienceView(h, null);
	assert.equal(acceptedSeries({ ...v, submissions: v.submissions.map((x) => ({ ...x, verdict: 'pending' })) }), null);
});

test('the SVG: one point per proof, hollow for a reuse, the count at the end, sized as asked, no projection', () => {
	const s = acceptedSeries(scienceView(h, records()))!;
	const svg = acceptedChartSVG(s, 400);
	assert.match(svg, /^<svg viewBox="0 0 400 200" width="400" height="200" role="img" aria-label="[^"]+"/);
	assert.equal((svg.match(/<circle /g) ?? []).length, s.accepted.length);
	assert.equal((svg.match(/class="pt reuse"/g) ?? []).length, s.accepted.filter((a) => a.kind === 'reuse').length);
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
	const evil = { ...s, accepted: [...s.accepted, { at: s.end, trial: 9, kind: 'proof' as const, title: '<img src=x onerror=alert(1)>', url: 'https://prove2.me/theorems/"x' }] };
	const svg = acceptedChartSVG(evil);
	const rows = acceptedRowsHTML(evil);
	assert.ok(!svg.includes('<img') && !rows.includes('<img') && !rows.includes('/"x'));
	const totals = [...acceptedRowsHTML(s).matchAll(/<td class="num">(\d+)<\/td><\/tr>/g)].map((m) => Number(m[1]));
	assert.deepEqual(totals, s.accepted.map((_, i) => s.accepted.length - i));
	assert.match(acceptedSummary(s), new RegExp(`^${s.accepted.length} proofs accepted by prove2me from `));
});
