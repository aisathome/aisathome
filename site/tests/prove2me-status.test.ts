// The status block at the top of /science/prove2me/ (prove2me-status.ts), on fixed data: the public records as the
// gateway served them on 2026-11-18 (tests/fixtures; trial 8 running) and variations of them (a second trial open, as on
// 2026-11-19, when trial 9 started while trial 8 still had tasks; none open). Trial 8's 46 tasks were counted from the
// fixture by hand: 14 done and proved, 18 done otherwise, 3 cancelled, 2 leased, 9 queued. A bench (labels as the
// gateway served bench-v13's on 2026-11-23: trial bench-v13, scaffold v13, leaf, alias, group, outline) is added to them
// in the last tests.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { historyOf, parseCampaigns, parseResults, scienceView, type Records } from '../src/data/prove2me-records';
import { barPart, builtStatus, isBench, parseNodes, queueBarHTML, shortName, statusTexts, statusView, trialLine } from '../src/data/prove2me-status';

const h = historyOf(raw as unknown as Prove2me);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});
const nodes = parseNodes({ nodes: { online: 2, busy: 1, slots: 2, held: 0, paused: 0 }, updated: '2026-11-18T14:58:00Z' })!;
const at = new Date('2026-11-18T15:00:00Z');
const t8 = { proved: 14, ended: 18, stopped: 3, running: 2, queued: 9, total: 46 };

/** The records with trial 9 started beside trial 8 (scaffold v10): 2 tasks done, 1 running, 4 waiting. */
const twoOpen = () => {
	const r = records();
	r.campaigns.push({ ...r.campaigns.find((c) => c.id === 't8')!, id: 't9', started_at: '2026-11-18T20:46:49Z', labels: { kind: 'trial', scaffold: 'v10' } });
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	const mk = (id: string, state: PublicTask['state'], outcome: string | null = null) => ({ ...base, id, state, outcome, labels: { ...base.labels, trial: 't9', leaf: 'p2m-000000' } });
	r.tasks.tasks.push(mk('n1', 'done', 'outline'), mk('n2', 'done', 'refused'), mk('n3', 'leased'), mk('n4', 'queued'), mk('n5', 'queued'), mk('n6', 'queued'), mk('n7', 'queued'));
	return r;
};

test('one trial open: the queue is its running and waiting tasks; its line has every state and its leaves', () => {
	const v = statusView(records(), nodes)!;
	assert.equal(v.source, 'live');
	assert.deepEqual(v.trials.map((t) => [t.label, t.status, t.scaffold, t.start]), [['t8', 'running', 'v9', '2026-11-16T21:33:33Z']]);
	assert.deepEqual(v.trials[0].counts, t8);
	assert.deepEqual(v.trials[0].leaves, { proved: 2, of: 6 }); // the trials table's count of trial 8
	assert.deepEqual(v.queue, [{ label: 't8', running: 2, queued: 9 }]);
	assert.deepEqual(v.submissions, { submitted: 8, accepted: 6, pending: 0 }); // 2 of the 8 accepted are reuses, not proofs
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'Trial 8 is running, scaffold v9, since 16 Nov.');
	assert.equal(tx.queue, '11 tasks in the queue: 2 running, 9 waiting.');
	assert.deepEqual(tx.lines, ['Trial 8, 46 tasks: 14 proved, 18 ended otherwise, 3 stopped, 2 running, 9 waiting; 2 of 6 leaves proved.']);
	assert.deepEqual(tx.nums, { nodes: '2', slots: '1 of 2', accepted: '6 of 8' });
	assert.match(tx.updated, /^Live from the gateway, updated 14:58 UTC/);
	// no percentage anywhere: there is no fixed total of work
	assert.ok(![tx.head, tx.queue, ...tx.lines, tx.updated, ...Object.values(tx.nums)].some((x) => x.includes('%')));
});

test('two trials open: the queue is every trial’s, the head names both, a line each', () => {
	const v = statusView(twoOpen(), nodes)!;
	assert.deepEqual(v.trials.map((t) => t.label), ['t8', 't9']);
	assert.deepEqual(v.queue, [{ label: 't8', running: 2, queued: 9 }, { label: 't9', running: 1, queued: 4 }]);
	assert.deepEqual(v.trials[1].counts, { proved: 0, ended: 2, stopped: 0, running: 1, queued: 4, total: 7 });
	assert.deepEqual(v.trials[1].leaves, { proved: 0, of: 6 });
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'Trials 8 (scaffold v9) and 9 (scaffold v10) are running.');
	assert.equal(tx.queue, '16 tasks in the queue: 3 running, 13 waiting (Trial 8: 2 running, 9 waiting; Trial 9: 1 running, 4 waiting).');
	assert.equal(tx.lines[1], 'Trial 9, 7 tasks: 2 ended otherwise, 1 running, 4 waiting; 0 of 6 leaves proved.');
});

test('a trial whose campaign ended keeps its open tasks in the queue', () => {
	const r = twoOpen();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.trials.map((t) => t.label), ['t8', 't9']);
	assert.equal(v.queue!.reduce((a, q) => a + q.running + q.queued, 0), 16);
});

test('a probe round beside a trial, a test’s tasks left out', () => {
	const r = records();
	r.campaigns.push({ ...r.campaigns.find((c) => c.id === 't7p')!, id: 't9p', status: 'running', started_at: '2026-11-18T18:00:00Z', ended_at: null, labels: { kind: 'probes', for: 't9', scaffold: 'v10' } });
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8' && t.state === 'queued')!;
	r.tasks.tasks.push(
		{ ...base, id: 'x1', state: 'queued', labels: { ...base.labels, trial: 't9p' } },
		{ ...base, id: 'x2', state: 'leased', labels: { ...base.labels, trial: 't9p' } },
		{ ...base, id: 'x3', state: 'queued', labels: { ...base.labels, trial: 'e2e1' } },
	);
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.trials.map((t) => t.label), ['t8', 't9p']);
	assert.equal(v.trials[1].leaves, null);
	assert.deepEqual(v.queue![1], { label: 't9p', running: 1, queued: 1 });
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'Trial 8 (scaffold v9) and outline probes for trial 9 (scaffold v10) are running.');
	assert.equal(tx.lines[1], 'Outline probes for trial 9, 2 tasks: 1 running, 1 waiting.');
	assert.equal(shortName('t9p'), 'Probes 9');
});

test('an empty queue: every task ended, the trial still open', () => {
	const r = records();
	for (const t of r.tasks.tasks) if (t.state === 'queued' || t.state === 'leased') t.state = 'cancelled';
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.queue, []);
	assert.deepEqual(v.trials[0].counts, { proved: 14, ended: 18, stopped: 14, running: 0, queued: 0, total: 46 });
	assert.equal(statusTexts(v, at).queue, 'Nothing in the queue now.');
});

test('a trial with no tasks yet', () => {
	const r = records();
	r.tasks.tasks = r.tasks.tasks.filter((t) => t.labels.trial !== 't8');
	const v = statusView(r, nodes)!;
	assert.equal(trialLine(v.trials[0]), 'Trial 8, no tasks yet; 0 of 6 leaves proved.');
	assert.equal(statusTexts(v, at).queue, 'Nothing in the queue now.');
});

test('no trial open: the last that ended, no lines', () => {
	const r = records();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	r.campaigns.find((c) => c.id === 't8')!.ended_at = '2026-11-18T12:00:00Z';
	for (const t of r.tasks.tasks) if (t.state === 'queued' || t.state === 'leased') t.state = 'cancelled';
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.trials, []);
	assert.deepEqual(v.lastEnded, { number: 8, end: '2026-11-18T12:00:00Z' });
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'No trial is running. Trial 8 ended on 18 Nov.');
	assert.deepEqual(tx.lines, []);
	assert.equal(tx.queue, 'Nothing in the queue now.');
});

test('tasks of a trial with no campaign yet', () => {
	const r = records();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	r.tasks.tasks = [{ ...base, id: 'y1', state: 'queued', outcome: null, labels: { ...base.labels, trial: 't9' } }];
	const v = statusView(r, null)!;
	assert.deepEqual(v.trials.map((t) => [t.label, t.scaffold, t.leaves]), [['t9', '', null]]);
	assert.equal(statusTexts(v, at).head, 'Trial 9 is running.');
});

test('a task’s state as the block counts it', () => {
	assert.equal(barPart({ state: 'done', outcome: 'proved' }), 'proved');
	assert.equal(barPart({ state: 'done', outcome: 'refused' }), 'ended');
	assert.equal(barPart({ state: 'failed', outcome: null }), 'stopped');
	assert.equal(barPart({ state: 'cancelled', outcome: null }), 'stopped');
	assert.equal(barPart({ state: 'leased', outcome: null }), 'running');
	assert.equal(barPart({ state: 'queued', outcome: null }), 'queued');
});

test('the gateway down: the build’s copy stays, nodes alone join it', () => {
	assert.equal(statusView(null, null), null);
	const b = builtStatus(h);
	assert.equal(b.source, 'build');
	assert.equal(b.queue, null);
	const tb = statusTexts(b, at);
	assert.match(tb.updated, /^As of the site’s last build/);
	assert.equal(tb.queue, '');
	assert.deepEqual(tb.lines, []);
	assert.deepEqual([tb.nums.nodes, tb.nums.slots], ['', '']);
	const v = statusView(null, nodes, b)!;
	assert.equal(v.source, 'build');
	assert.deepEqual(v.nodes, nodes);
	assert.match(statusTexts(v, at).updated, /nodes live from the gateway, updated 14:58 UTC/);
});

test('stale: the data stops refreshing', () => {
	const v = statusView(records(), nodes)!;
	assert.match(statusTexts(v, new Date('2026-11-18T15:30:00Z')).updated, /the gateway has not answered since/);
});

test('nodes: only a well-formed answer, busy never above slots', () => {
	assert.equal(parseNodes(null), null);
	assert.equal(parseNodes({ nodes: { online: 1, busy: 'x', slots: 1 }, updated: '2026-11-18T14:58:00Z' }), null);
	assert.equal(parseNodes({ nodes: { online: 1, busy: 1, slots: 1 } }), null);
	assert.deepEqual(parseNodes({ nodes: { online: 3, busy: 5, slots: 4 }, updated: '2026-11-18T14:58:00Z' }), { online: 3, busy: 4, slots: 4, updated: '2026-11-18T14:58:00Z' });
});

/** The records with a bench open, as on 2026-11-23 (bench-v13): its tasks name 3 leaves; 2 done, 2 running, 3 waiting. */
const withBench = (open = true) => {
	const r = records();
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	const mk = (id: string, leaf: string, state: PublicTask['state'], outcome: string | null = null): PublicTask => ({
		...base, id: `bench-v13-${leaf}-${id}`, state, outcome, parent: null, needs: [], retry_of: null, replaces: null,
		labels: { alias: 'N14', group: 'open', leaf, outline: '1', scaffold: 'v13', trial: 'bench-v13' },
		created_at: '2026-11-18T14:00:00Z', started_at: state === 'queued' ? null : '2026-11-18T14:30:00Z', finished_at: null,
	});
	const o = (s: PublicTask['state']) => (open ? s : 'done');
	r.tasks.tasks.push(
		mk('s1', 'p2m-580c57', 'done', 'outline'), mk('s1-h01', 'p2m-580c57', 'done', 'proved'),
		mk('s1-h02', 'p2m-580c57', o('leased')), mk('s1-rest', 'p2m-3a17bb', o('leased')),
		mk('s1-h03', 'p2m-3a17bb', o('queued')), mk('s1-h04', 'p2m-b807fd', o('queued')), mk('s1-asm', 'p2m-b807fd', o('queued')),
	);
	return r;
};

test('a bench is shown as a trial: its part of the queue, its line, named after its label', () => {
	const v = statusView(withBench(), parseNodes({ nodes: { online: 2, busy: 2, slots: 2 }, updated: '2026-11-18T14:58:00Z' }))!;
	assert.deepEqual(v.trials.map((t) => [t.label, t.bench, t.scaffold, t.start, t.leaves]), [['t8', false, 'v9', '2026-11-16T21:33:33Z', { proved: 2, of: 6 }], ['bench-v13', true, 'v13', '2026-11-18T14:30:00Z', null]]);
	// every prove2me task running or waiting is in the queue
	assert.deepEqual(v.queue, [{ label: 't8', running: 2, queued: 9 }, { label: 'bench-v13', running: 2, queued: 3 }]);
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'Trial 8 (scaffold v9) and bench v13 (scaffold v13) are running.');
	assert.equal(tx.queue, '16 tasks in the queue: 4 running, 12 waiting (Trial 8: 2 running, 9 waiting; Bench v13: 2 running, 3 waiting).');
	assert.deepEqual(tx.lines, [
		'Trial 8, 46 tasks: 14 proved, 18 ended otherwise, 3 stopped, 2 running, 9 waiting; 2 of 6 leaves proved.',
		'Bench v13, 7 tasks: 1 proved, 1 ended otherwise, 2 running, 3 waiting.',
	]);
	assert.equal(shortName('bench-v13'), 'Bench v13');
	// a bench alone
	const r = withBench();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	for (const t of r.tasks.tasks) if (t.labels.trial === 't8' && (t.state === 'queued' || t.state === 'leased')) t.state = 'cancelled';
	assert.equal(statusTexts(statusView(r, nodes)!, at).head, 'Bench v13 is running, scaffold v13, since 18 Nov.');
});

test('a bench that has ended is not shown', () => {
	const v = statusView(withBench(false), nodes)!;
	assert.deepEqual(v.trials.map((t) => t.label), ['t8']);
	assert.deepEqual(v.queue!.map((q) => q.label), ['t8']);
});

test('a bench counts in no result: trial 8, the submissions and the page’s numbers, trials and leaves as without it', () => {
	const plain = records();
	const bench = withBench();
	const v = statusView(bench, nodes)!;
	assert.deepEqual(v.trials[0].counts, t8);
	assert.deepEqual(v.trials[0].leaves, { proved: 2, of: 6 });
	assert.deepEqual(v.submissions, statusView(plain, nodes)!.submissions);
	const a = scienceView(h, plain);
	const b = scienceView(h, bench);
	assert.deepEqual(b.trials, a.trials);
	assert.deepEqual(b.kpis, a.kpis);
	assert.deepEqual(b.submissions, a.submissions);
	assert.ok(isBench('bench-v13') && isBench('bench-v12b') && !isBench('t10') && !isBench('e2e1') && !isBench(undefined));
});

test('the queue bar as HTML: a part per trial or bench, alternating, escaped', () => {
	const b = queueBarHTML(statusView(withBench(), nodes)!);
	assert.match(b.bar, /^<span class="grp grp-0" data-trial="t8" style="flex-grow: 11"><span class="seg q-running" data-part="running" style="flex-grow: 2"><\/span><span class="seg q-queued"/);
	assert.match(b.bar, /<span class="grp grp-1" data-trial="bench-v13" style="flex-grow: 5">/);
	assert.equal(b.labels, '<span data-trial="t8" style="flex-grow: 11">Trial 8 · 11</span><span data-trial="bench-v13" style="flex-grow: 5">Bench v13 · 5</span>');
	assert.deepEqual(queueBarHTML(builtStatus(h)), { bar: '', labels: '' });
});

test('the build’s copy from the records: the queue and the lines, as of the build; a saved copy says so', () => {
	const v = { ...statusView(withBench(), null)!, source: 'build' as const };
	const tx = statusTexts(v, at);
	assert.equal(tx.lines.length, 2);
	assert.match(tx.queue, /^16 tasks in the queue/);
	assert.equal(tx.updated, 'As of the gateway’s records at the site’s last build, 18 Nov, 14:58 UTC. The page refreshes it every minute when it can reach the gateway.');
	assert.equal(
		statusTexts({ ...v, saved: true }, at).updated,
		'As of a saved copy of the gateway’s records, 18 Nov, 14:58 UTC: the gateway did not answer the site’s last build. The page refreshes it every minute when it can reach the gateway.',
	);
});
