// The status block at the top of /science/prove2me/ (prove2me-status.ts), on fixed data: the public records as the
// gateway served them on 2026-11-18 (tests/fixtures; trial 8 running) and variations of them. Trial 8's 46 tasks were
// counted from the fixture by hand: 14 done and proved, 18 done otherwise, 3 cancelled, 2 leased, 9 queued.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { historyOf, parseCampaigns, parseResults, type Records } from '../src/data/prove2me-records';
import { barPart, builtStatus, parseNodes, statusTexts, statusView } from '../src/data/prove2me-status';

const h = historyOf(raw as unknown as Prove2me);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});
const nodes = parseNodes({ nodes: { online: 2, busy: 1, slots: 2, held: 0, paused: 0 }, updated: '2026-11-18T14:58:00Z' })!;
const at = new Date('2026-11-18T15:00:00Z');

test('the running trial: its tasks as the bar, its leaves, the submissions and the nodes', () => {
	const v = statusView(records(), nodes)!;
	assert.equal(v.source, 'live');
	assert.deepEqual(v.trial, { label: 't8', number: 8, probe: false, status: 'running', scaffold: 'v9', start: '2026-11-16T21:33:33Z' });
	assert.deepEqual(v.bar, { proved: 14, ended: 18, stopped: 3, running: 2, queued: 9, total: 46 });
	assert.deepEqual(v.leaves, { proved: 2, of: 6 }); // the trials table's count of trial 8
	assert.deepEqual(v.elsewhere, { tasks: 0, trials: 0 });
	assert.deepEqual(v.submissions, { submitted: 8, accepted: 8, pending: 0 });
	assert.equal(v.updated, '2026-11-18T14:58:39Z');
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'Trial 8 is running since 16 Nov, scaffold v9');
	assert.equal(tx.barWords, '46 tasks: 14 proved, 18 ended otherwise, 3 stopped, 2 running, 9 waiting.');
	assert.equal(tx.elsewhere, '');
	assert.deepEqual(tx.nums, { nodes: '2', slots: '1 of 2', leaves: '2 of 6', accepted: '8 of 8' });
	assert.match(tx.updated, /^Live from the gateway, updated 14:58 UTC/);
	// no percentage anywhere: there is no fixed total of work
	assert.ok(!Object.values(tx).flatMap((x) => (typeof x === 'string' ? [x] : Object.values(x))).some((x) => x.includes('%')));
});

test('a task’s part of the bar', () => {
	assert.equal(barPart({ state: 'done', outcome: 'proved' }), 'proved');
	assert.equal(barPart({ state: 'done', outcome: 'refused' }), 'ended');
	assert.equal(barPart({ state: 'done', outcome: null }), 'ended');
	assert.equal(barPart({ state: 'failed', outcome: null }), 'stopped');
	assert.equal(barPart({ state: 'cancelled', outcome: null }), 'stopped');
	assert.equal(barPart({ state: 'leased', outcome: null }), 'running');
	assert.equal(barPart({ state: 'queued', outcome: null }), 'queued');
});

test('an empty queue: every task of the trial ended', () => {
	const r = records();
	for (const t of r.tasks.tasks) if (t.labels.trial === 't8' && (t.state === 'queued' || t.state === 'leased')) t.state = 'cancelled';
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.bar, { proved: 14, ended: 18, stopped: 14, running: 0, queued: 0, total: 46 });
	assert.equal(statusTexts(v, at).barWords, '46 tasks: 14 proved, 18 ended otherwise, 14 stopped.');
});

test('a trial with no tasks yet', () => {
	const r = records();
	r.tasks.tasks = r.tasks.tasks.filter((t) => t.labels.trial !== 't8');
	const v = statusView(r, nodes)!;
	assert.deepEqual(v.bar, { proved: 0, ended: 0, stopped: 0, running: 0, queued: 0, total: 0 });
	assert.deepEqual(v.leaves, { proved: 0, of: 6 });
	assert.equal(statusTexts(v, at).barWords, 'No tasks yet.');
});

test('no trial open: the last that ended, no bar', () => {
	const r = records();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	r.campaigns.find((c) => c.id === 't8')!.ended_at = '2026-11-18T12:00:00Z';
	for (const t of r.tasks.tasks) if (t.state === 'queued' || t.state === 'leased') t.state = 'cancelled';
	const v = statusView(r, nodes)!;
	assert.equal(v.trial, null);
	assert.equal(v.bar, null);
	assert.equal(v.leaves, null);
	assert.deepEqual(v.lastEnded, { number: 8, end: '2026-11-18T12:00:00Z' });
	const tx = statusTexts(v, at);
	assert.equal(tx.head, 'No trial is running. Trial 8 ended on 18 Nov.');
	assert.equal(tx.nums.leaves, '');
});

test('several trials open: the trial in the bar, the probe round’s tasks as waiting elsewhere', () => {
	const r = records();
	r.campaigns.push({ ...r.campaigns.find((c) => c.id === 't7p')!, id: 't9p', status: 'running', ended_at: null, labels: { kind: 'probes', for: 't9', scaffold: 'v10' } });
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8' && t.state === 'queued')!;
	r.tasks.tasks.push(
		{ ...base, id: 'x1', state: 'queued', labels: { ...base.labels, trial: 't9p' } },
		{ ...base, id: 'x2', state: 'leased', labels: { ...base.labels, trial: 't9p' } },
		{ ...base, id: 'x3', state: 'queued', labels: { ...base.labels, trial: 'e2e1' } }, // a test's task: not a trial
	);
	const v = statusView(r, nodes)!;
	assert.equal(v.trial!.label, 't8');
	assert.equal(v.bar!.total, 46);
	assert.deepEqual(v.elsewhere, { tasks: 2, trials: 1 });
	assert.equal(statusTexts(v, at).elsewhere, '2 more tasks waiting or running in another trial.');
	// with trial 8 ended, the probe round is the bar's
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	for (const t of r.tasks.tasks) if (t.labels.trial === 't8' && (t.state === 'queued' || t.state === 'leased')) t.state = 'cancelled';
	const w = statusView(r, nodes)!;
	assert.equal(w.trial!.label, 't9p');
	assert.deepEqual(w.bar, { proved: 0, ended: 0, stopped: 0, running: 1, queued: 1, total: 2 });
	assert.equal(w.leaves, null);
	assert.equal(statusTexts(w, at).head, 'Outline probes for trial 9 are running since 15 Nov, scaffold v10');
});

test('tasks of a trial with no campaign yet', () => {
	const r = records();
	r.campaigns.find((c) => c.id === 't8')!.status = 'finished';
	const base = r.tasks.tasks.find((t) => t.labels.trial === 't8')!;
	r.tasks.tasks = [{ ...base, id: 'y1', state: 'queued', outcome: null, labels: { ...base.labels, trial: 't9' } }];
	const v = statusView(r, null)!;
	assert.deepEqual(v.trial, { label: 't9', number: 9, probe: false, status: 'running', scaffold: '', start: null });
	assert.equal(v.bar!.queued, 1);
	assert.equal(statusTexts(v, at).head, 'Trial 9 is running');
});

test('the gateway down: the build’s copy stays, nodes alone join it', () => {
	assert.equal(statusView(null, null), null);
	const b = builtStatus(h);
	assert.equal(b.source, 'build');
	assert.equal(b.bar, null);
	assert.equal(b.nodes, null);
	assert.deepEqual(b.submissions, { submitted: h.submissions.length, accepted: h.submissions.filter((s) => s.verdict === 'accepted').length, pending: 0 });
	const tb = statusTexts(b, at);
	assert.match(tb.updated, /^As of the site’s last build/);
	assert.deepEqual([tb.nums.nodes, tb.nums.slots, tb.nums.leaves], ['', '', '']);
	const v = statusView(null, nodes, b)!;
	assert.equal(v.source, 'build');
	assert.equal(v.trial, b.trial);
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
