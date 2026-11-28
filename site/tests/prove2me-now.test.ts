// "Working on now" (prove2me-now.ts): a block per trial not finished and per open bench, on the records of 2026-11-18
// (trial 8 running) with a bench added as the gateway served bench-v13's tasks on 2026-11-23 (trial bench-v13, scaffold
// v13, leaf, alias, group, outline; no campaign).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { PublicTask } from '../src/data/prove2me-current';
import { parseCampaigns, parseResults, type Records } from '../src/data/prove2me-records';
import { benchDocs, holesDone, nowAll, nowBlocksHTML, nowHead, shortTitle } from '../src/data/prove2me-now';

const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});

/** bench-v13 on two leaves trial 8 lists (N11 p2m-754bbc, N14 p2m-580c57): one done, one running, one waiting. */
const withBench = (open = true) => {
	const r = records();
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	const mk = (id: string, leaf: string, alias: string, state: PublicTask['state'], role = 'hole'): PublicTask => ({
		...base, id: `bench-v13-${leaf}-${id}`, role, state, outcome: state === 'done' ? 'proved' : null, parent: null, needs: [], retry_of: null, replaces: null, closed_by: null,
		labels: { alias, group: 'open', leaf, outline: '1', scaffold: 'v13', trial: 'bench-v13', ...(role === 'hole' ? { hole: id.slice(-1) } : {}) },
		created_at: `2026-11-18T14:0${id.slice(-1)}:00Z`, started_at: state === 'queued' ? null : '2026-11-18T14:30:00Z', finished_at: null,
	});
	r.tasks.tasks.push(mk('s1-h1', 'p2m-754bbc', 'N11', 'done'), mk('s1-h2', 'p2m-580c57', 'N14', open ? 'leased' : 'done'), mk('s1-h3', 'p2m-580c57', 'N14', open ? 'queued' : 'cancelled'));
	return r;
};

test('an open bench is a block as a trial is, after the trials; its leaves from its tasks, theorems from the campaigns', () => {
	const live = withBench();
	const bs = benchDocs(live);
	assert.equal(bs.length, 1);
	const b = bs[0];
	assert.deepEqual([b.schema, b.trial.id, b.trial.scaffold, b.trial.start, b.submissions], ['bench', 'bench-v13', 'v13', '2026-11-18', []]);
	assert.deepEqual(b.leaves.map((l) => [l.id, l.alias]), [['p2m-754bbc', 'N11'], ['p2m-580c57', 'N14']]);
	const t8 = live.campaigns.find((c) => c.id === 't8')!;
	assert.equal(b.leaves[0].theorem, t8.items.find((i) => i.id === 'p2m-754bbc')!.labels.theorem);
	assert.deepEqual(nowAll(null, live).map((d) => d.trial.id), ['t8', 'bench-v13']);
	assert.equal(nowHead(b), 'Bench v13, since 18 November 2026 · scaffold v13');
	// ended: no block; a test's or a trial's tasks are no bench
	assert.deepEqual(nowAll(null, withBench(false)).map((d) => d.trial.id), ['t8']);
	assert.deepEqual(benchDocs(records()), []);
});

test('the blocks: the bench’s leaves and tasks as a trial’s; a leaf’s anchors on its first row, the trial’s', () => {
	const live = withBench();
	const html = nowBlocksHTML(nowAll(null, live), live.tasks);
	const [trial, bench] = html.split('<div class="now-trial"').slice(1);
	assert.match(trial, /^ data-now-trial="t8" data-status="running">/);
	assert.match(trial, /id="leaf-p2m-754bbc"/);
	assert.match(bench, /^ data-now-trial="bench-v13" data-status="running">/);
	assert.match(bench, /data-now-head>Bench v13, since 18 November 2026 · scaffold v13</);
	assert.match(bench, /data-current-leaf="N14"[^>]*data-state="running"/);
	assert.ok(!bench.includes('id="leaf-'));
	// no description of the trial or its budget
	assert.ok(!html.includes('data-now-about') && !html.includes('data-now-budget') && !html.includes('Budget:'));
});

test('every value is escaped', () => {
	const live = withBench();
	live.campaigns.find((c) => c.id === 't8')!.items.find((i) => i.id === 'p2m-754bbc')!.labels.theorem = '<img src=x onerror=alert(1)>';
	for (const t of live.tasks.tasks) if (t.labels.trial === 'bench-v13') t.labels.scaffold = '<b>v13</b>';
	const html = nowBlocksHTML(nowAll(null, live), live.tasks);
	assert.ok(!html.includes('<img') && !html.includes('<b>'));
	assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt;'));
});

test('a leaf is one row: id, short title, its line, holes solved and GPU-hours; the rest opens under it, closed', () => {
	const live = records();
	const t8 = live.campaigns.find((c) => c.id === 't8')!;
	const titles = Object.fromEntries(t8.items.map((i) => [i.id, i.title]));
	const html = nowBlocksHTML(nowAll(null, live), live.tasks, { titles });
	const rows = html.split(/<li(?=[^>]*data-current-leaf)/).slice(1);
	assert.equal(rows.length, 6);
	for (const r of rows) {
		assert.match(r, /<details class="now-leaf" data-leaf-detail="[^"]+"><summary class="now-row"><span class="leaf">p2m-[0-9a-f]{6}<\/span><span class="now-title" data-now-title/);
		assert.ok(!/<details class="now-leaf"[^>]* open/.test(r), 'closed until opened');
		// the outlines, the holes and the task table sit after the row, not in it
		const [summary] = r.split('</summary>');
		assert.ok(!summary.includes('data-leaf-plan') && !summary.includes('data-leaf-tasks') && !summary.includes('<a '));
	}
	// trial 8's p2m-527634 (N12), proved by outline 1 with its 8 holes: all solved, its GPU-hours from its tasks
	const n12 = rows.find((r) => r.includes('data-current-leaf="N12"'))!;
	assert.match(n12, /data-leaf-summary>Proved: outline 1 assembled</);
	assert.match(n12, /data-now-holes="8\/8">8 of 8 holes solved</);
	assert.match(n12, /data-now-hours="[\d.]+">\d+\.\d\d GPU-h</);
	assert.match(n12, new RegExp(`data-now-title title="[^"]+">${shortTitle(titles['p2m-527634'], '').replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}<`));
	// a running leaf says "so far"
	assert.match(rows.find((r) => r.includes('data-state="running"'))!, /GPU-h so far</);
});

test('a short title: prove2me’s title without TeX, cut; else the Lean name', () => {
	assert.equal(shortTitle('Lemma $x \\le y$', 'T.x'), 'Lemma x ≤ y');
	assert.equal(shortTitle('', 'Some.lean_name'), 'Some.lean_name');
	const long = shortTitle('a'.repeat(100), 'T', 70);
	assert.equal(long.length, 70);
	assert.ok(long.endsWith('…'));
	assert.equal(holesDone(null), null);
	assert.equal(holesDone({ summary: '', outlines: [{ gen: 1, state: 'writing', text: '', reason: '', cost: '', holes: [] }] }), null);
	assert.deepEqual(
		holesDone({
			summary: '',
			outlines: [
				{
					gen: 2, state: 'in_progress', text: '', reason: '', cost: '',
					holes: [
						{ hole: 1, label: 'Hole 1', kind: 'lemma', state: 'solved', text: '', title: '' },
						{ hole: 2, label: 'Hole 2', kind: 'lemma', state: 'working', text: '', title: '' },
						{ hole: null, label: 'Assembly', kind: 'assembly', state: 'queued', text: '', title: '' },
					],
				},
			],
		}),
		{ solved: 1, total: 2, text: '1 of 2 holes solved' },
	);
});
