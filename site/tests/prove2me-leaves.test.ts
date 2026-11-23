// "Every leaf" (prove2me-records.ts leavesView): from the build's history alone it must say what the export's own
// leaves and by_role said (the page showed those until 2026-11-23), and with the gateway's records it takes trials 6 on
// from them (the records of 2026-11-18, trial 8 running) and never a benchmark's tasks.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { historyOf, leavesView, parseCampaigns, parseResults, scienceView, type Records } from '../src/data/prove2me-records';
import { leafFiltersHTML, leafRowsHTML, leafScoreHTML } from '../src/data/prove2me-view';

const p = raw as unknown as Prove2me;
const h = historyOf(p);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});
const r1 = (x: number | null) => (x === null ? null : Math.round(x * 10) / 10);

test('from the build alone: the export’s own leaves and scorecard, row for row', () => {
	const v = leavesView(h, null);
	assert.deepEqual(
		v.leaves.map((l) => [l.id, l.aliases, l.roles, l.trials, l.runs, l.provedRuns, l.outcome, l.first, r1(l.minutes)]),
		p.leaves.map((l) => [l.id, l.aliases, l.roles, l.trials, l.runs, l.proved_runs, l.outcome, l.first_tried, l.model_minutes]),
	);
	for (const l of p.leaves) {
		const row = v.leaves.find((x) => x.id === l.id)!;
		assert.equal(row.theorem, l.theorem);
		assert.equal(row.computed, l.prompt_tokens === null ? null : l.prompt_tokens - l.cached_tokens! + l.output_tokens!);
	}
	assert.deepEqual(
		v.roles.map((r) => [r.role, r.leaves, r.runs, r.proved, r.accepted, r.reused, r.hours.toFixed(1), r.computed]),
		p.by_role.map((r) => [r.role, r.leaves, r.runs, r.proved, r.accepted, r.reused, r.model_hours.toFixed(1), r.prompt_tokens - r.cached_tokens + r.output_tokens]),
	);
	const t = p.totals;
	assert.deepEqual([v.all.leaves, v.all.runs, v.all.proved, v.all.accepted, v.all.reused], [t.leaves, t.runs, t.proved, t.accepted - t.reused, t.reused]);
	assert.equal(v.all.hours.toFixed(1), (t.model_hours + t.probes.model_hours).toFixed(1));
	assert.equal(v.probes!.leaves, 11);
	assert.equal(v.notScored, t.runs - t.scored);
});

test('with the gateway’s records: trial 6 on from them, the build’s rows of those trials replaced', () => {
	const live = records();
	const v = leavesView(h, live);
	const s = scienceView(h, live);
	assert.equal(v.all.runs, s.kpis.runs); // 54: as the headline numbers
	assert.equal(v.all.proved, s.kpis.proved);
	assert.equal(v.all.accepted + v.all.reused, s.kpis.accepted);
	// trial 8's leaves are in, with their theorems from the campaign's items; N12 accepted from the results
	const n12 = v.leaves.find((l) => l.aliases.includes('N12'))!;
	assert.deepEqual([n12.id, n12.outcome, n12.trials, n12.first], ['p2m-527634', 'accepted', [8], '2026-11-16']);
	assert.equal(n12.theorem, 'DistLTLSynth.Char.lemma_2_3');
	// a leaf run in trials 3, 6, 7 and 8 (the calibration leaf C07): one row, every trial
	const c07 = v.leaves.find((l) => l.id === 'p2m-3ad60f')!;
	assert.deepEqual([c07.trials, c07.roles], [[3, 6, 7, 8], ['calibration']]); // its role in each campaign
	// the outline probes: trial 6's and 7's rounds from the records (11 leaves), not counted twice with the build's
	assert.equal(v.probes!.leaves, 11);
	// the order: first tried, the leaves closed by reuse last
	assert.deepEqual(v.leaves.slice(-2).map((l) => l.outcome), ['reused', 'reused']);
	assert.equal(new Set(v.leaves.map((l) => l.id)).size, v.leaves.length);
});

test('a benchmark’s tasks are no leaf’s run', () => {
	const live = records();
	const base = live.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	live.tasks.tasks.push({ ...base, id: 'bench-v13-x', state: 'done', outcome: 'proved', role: 'assembly', started_at: '2026-11-18T14:00:00Z', labels: { ...base.labels, trial: 'bench-v13', leaf: 'p2m-fe7aa1', scaffold: 'v13' } });
	assert.deepEqual(leavesView(h, live), leavesView(h, records()));
});

test('the HTML: every value escaped, the filters for the roles and outcomes there are', () => {
	const v = leavesView(h, records());
	v.leaves[0].theorem = '<img src=x>';
	const rows = leafRowsHTML(v);
	assert.ok(!rows.includes('<img') && rows.includes('&lt;img src=x&gt;'));
	assert.equal((rows.match(/<tr /g) ?? []).length, v.leaves.length);
	assert.match(rows, /data-leaf-id="p2m-527634"[^>]*data-outcome="accepted"/);
	const score = leafScoreHTML(v);
	assert.match(score.body, /data-role="calibration"/);
	assert.match(score.body, /data-role="probes"/);
	assert.match(score.foot, new RegExp(`data-cell="leaves">${v.all.leaves}<`));
	const f = leafFiltersHTML(v);
	assert.match(f, /data-filter="role" data-value="comparison"/);
	assert.ok(!f.includes('data-value="stopped"')); // no leaf was stopped
});

test('a leaf whose only runs were stopped by us is "stopped", one cut short by a fault "not scored"', () => {
	const hh = structuredClone(h);
	for (const r of hh.runs) if (r.alias === 'B02') Object.assign(r, { scored: false, stopped: true });
	const v = leavesView(hh, null);
	assert.equal(v.leaves.find((l) => l.aliases.includes('B02'))!.outcome, 'stopped');
	assert.equal(v.leaves.find((l) => l.aliases.includes('B05'))!.outcome, 'not_scored');
});
