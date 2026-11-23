// "Working on now" (prove2me-now.ts): a block per open benchmark and per trial not finished, on the records of
// 2026-11-18 (trial 8 running) with a replay benchmark added as the gateway served bench-v13's tasks on 2026-11-23
// (trial bench-v13, scaffold v13, leaf, alias, group, outline; no campaign).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { PublicTask } from '../src/data/prove2me-current';
import { parseCampaigns, parseResults, type Records } from '../src/data/prove2me-records';
import { benchDocs, benchText, nowAll, nowBlocksHTML, nowHead } from '../src/data/prove2me-now';
import { statusView } from '../src/data/prove2me-status';

const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});

/** bench-v13 on two leaves trial 8 lists (N11 p2m-754bbc, N14 p2m-580c57): one done, one running, one waiting; trial
 * 8's running tasks wait again, so it waits behind the benchmark. */
const withBench = (open = true) => {
	const r = records();
	for (const t of r.tasks.tasks) if (t.labels.trial === 't8' && t.state === 'leased') Object.assign(t, { state: 'queued', started_at: null });
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	const mk = (id: string, leaf: string, alias: string, state: PublicTask['state'], role = 'hole'): PublicTask => ({
		...base, id: `bench-v13-${leaf}-${id}`, role, state, outcome: state === 'done' ? 'proved' : null, parent: null, needs: [], retry_of: null, replaces: null, closed_by: null,
		labels: { alias, group: 'open', leaf, outline: '1', scaffold: 'v13', trial: 'bench-v13', ...(role === 'hole' ? { hole: id.slice(-1) } : {}) },
		created_at: `2026-11-18T14:0${id.slice(-1)}:00Z`, started_at: state === 'queued' ? null : '2026-11-18T14:30:00Z', finished_at: null,
	});
	r.tasks.tasks.push(mk('s1-h1', 'p2m-754bbc', 'N11', 'done'), mk('s1-h2', 'p2m-580c57', 'N14', open ? 'leased' : 'done'), mk('s1-h3', 'p2m-580c57', 'N14', open ? 'queued' : 'cancelled'));
	return r;
};

test('a benchmark with open tasks is a block of its own, before the trials; its leaves from its tasks, theorems from the campaigns', () => {
	const live = withBench();
	const bs = benchDocs(live);
	assert.equal(bs.length, 1);
	const b = bs[0];
	assert.deepEqual([b.schema, b.trial.id, b.trial.scaffold, b.trial.start, b.submissions], ['bench', 'bench-v13', 'v13', '2026-11-18', []]);
	assert.deepEqual(b.leaves.map((l) => [l.id, l.alias]), [['p2m-754bbc', 'N11'], ['p2m-580c57', 'N14']]);
	const t8 = live.campaigns.find((c) => c.id === 't8')!;
	assert.equal(b.leaves[0].theorem, t8.items.find((i) => i.id === 'p2m-754bbc')!.labels.theorem);
	assert.deepEqual(nowAll(null, live).map((d) => d.trial.id), ['bench-v13', 't8']);
	assert.equal(nowHead(b), 'Benchmark bench-v13, since 18 November 2026 · a replay on scaffold v13, not a trial');
	// ended: no block
	assert.deepEqual(benchDocs(withBench(false)), []);
	assert.deepEqual(nowAll(null, withBench(false)).map((d) => d.trial.id), ['t8']);
	// a test's tasks (e2e1) and a trial's are no benchmark
	assert.ok(benchDocs(records()).length === 0);
});

test('the blocks: the benchmark says what it is and runs first; the trial waits behind it; anchors stay on the trial’s rows', () => {
	const live = withBench();
	const status = statusView(live, null);
	const html = nowBlocksHTML(nowAll(null, live), live.tasks, { status });
	const [bench, trial] = html.split('<div class="now-trial"').slice(1);
	assert.match(bench, /^ data-now-trial="bench-v13" data-status="running" data-bench>/);
	assert.ok(bench.includes(benchText(nowAll(null, live)[0], status).replace(/’/g, '’')));
	assert.match(bench, /Its tasks run before trial 8’s in the queue\./);
	assert.match(bench, /It is not a trial: it submits nothing/);
	assert.ok(!bench.includes('id="leaf-'), 'a benchmark row took a leaf’s anchor');
	assert.match(bench, /data-current-leaf="N14"[^>]*data-state="running"/);
	assert.match(trial, /^ data-now-trial="t8" data-status="running">/);
	assert.match(trial, /data-now-behind>Waiting: its tasks wait behind benchmark bench-v13, which the queue runs first\.</);
	assert.match(trial, /id="leaf-p2m-754bbc"/);
	// without a benchmark running ahead, no such line
	assert.ok(!nowBlocksHTML(nowAll(null, records()), records().tasks, { status: statusView(records(), null) }).includes('data-now-behind'));
});

test('a leaf’s earlier trials: before the trial for a trial, every trial for a benchmark', () => {
	const live = withBench();
	const seen: [string, number][] = [];
	nowBlocksHTML(nowAll(null, live), live.tasks, { earlier: (leaf, n) => (seen.push([leaf, n]), [7]) });
	assert.ok(seen.some(([l, n]) => l === 'p2m-754bbc' && n === Infinity));
	assert.ok(seen.some(([l, n]) => l === 'p2m-754bbc' && n === 8));
	const html = nowBlocksHTML(nowAll(null, live), live.tasks, { earlier: () => [7, 8] });
	assert.match(html, /tried in trials 7 and 8/); // the benchmark's
	assert.match(html, /tried before in trials 7 and 8/); // the trial's
});

test('every value is escaped', () => {
	const live = withBench();
	live.campaigns.find((c) => c.id === 't8')!.items.find((i) => i.id === 'p2m-754bbc')!.labels.theorem = '<img src=x onerror=alert(1)>';
	for (const t of live.tasks.tasks) if (t.labels.trial === 'bench-v13') t.labels.scaffold = '<b>v13</b>';
	const html = nowBlocksHTML(nowAll(null, live), live.tasks, { status: statusView(live, null) });
	assert.ok(!html.includes('<img') && !html.includes('<b>'));
	assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt;'));
});
