// "Working on now" (prove2me-now.ts): a block per trial not finished and per open bench, on the records of 2026-11-18
// (trial 8 running) with a bench added as the gateway served bench-v13's tasks on 2026-11-23 (trial bench-v13, scaffold
// v13, leaf, alias, group, outline; no campaign).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { PublicTask } from '../src/data/prove2me-current';
import { parseCampaigns, parseResults, type Records } from '../src/data/prove2me-records';
import { benchDocs, nowAll, nowBlocksHTML, nowHead } from '../src/data/prove2me-now';

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
