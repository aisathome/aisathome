// Every hole of an outline in one of a few states, Waiting, Working, Solved, Failed (and why, in a few words) or
// Cancelled, with the detail (why it waits, how it was solved, its tries) in a tooltip only; every hole shows, from
// the label `hole_states` that every task of the outline carries (set by the prove2me runner, 2026-11-25). Before it the page guessed "closed before the hole tasks, or left in place"
// for each hole number without a task below the highest hole task, and left out the holes above it: trial 11's
// p2m-dabfa9 outline 1 held holes 1, 2 and 4 back behind hole 3 (scaffold v13's riskiest step first), and hole 4 did
// not show.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseCurrent, trialQueue, type PublicTask } from '../src/data/prove2me-current';
import { liveLeaves, type TaskRow } from '../src/data/prove2me-live';
import { taskRef } from '../src/data/prove2me-outcomes';
import { leafPlan, leafPlanHTML, outlineView, parseHoleStates } from '../src/data/prove2me-outlines';

const L = 'p2m-dabfa9', S = `t11-${L}-s1`;
const DABFA9 = '1=wait:3 2=wait:3 3=task 4=wait:3';

/** One task of GET /v1/tasks (gateway tasks_public.go) of the leaf's outline 1. */
function task(id: string, role: string, state: PublicTask['state'], more: Partial<PublicTask> & { labels?: Record<string, string> } = {}): PublicTask {
	const { labels, ...rest } = more;
	return {
		project: 'prove2me', id, kind: 'p2m-v9', role, state, outcome: null, try: 1, parent: null, needs: [], retry_of: null, replaces: null,
		closed_by: null, labels: { trial: 't11', leaf: L, alias: 'N30', group: 'open', scaffold: 'v13', outline: '1', ...labels },
		steps: null, model_min: null, prompt_tokens: null, cached_tokens: null, completion_tokens: null,
		created_at: '2026-11-24T18:44:30Z', started_at: null, finished_at: null, updated_at: '2026-11-24T19:00:00Z', live: null, ...rest,
	};
}
const doc = parseCurrent({
	schema: 'prove2me-current/2', project: 'prove2me', updated_at: '2026-11-24T19:00:00Z',
	trial: { id: 't11', number: 11, scaffold: 'v13', start: '2026-11-24', status: 'running', description: '', budget: '' },
	leaves: [{ id: L, alias: 'N30', role: 'open', theorem: 'DQIShattered.erasure_recovery_iff', url: 'https://prove2.me/theorems/dabfa900-0000-4000-8000-000000000000' }],
	submissions: [],
})!;
const planOf = (tasks: PublicTask[]) => {
	const [leaf] = liveLeaves(trialQueue({ updated: '2026-11-24T19:00:00Z', tasks }, doc), 't11');
	return leafPlan(leaf.detail);
};
const lines = (p: ReturnType<typeof planOf>, gen = 1) => p.outlines.find((o) => o.gen === gen)!.holes.map((h) => [h.label, h.state, h.text, h.title]);

const outline = task(S, 'outline', 'done', { outcome: 'outline', steps: 40, model_min: 20, started_at: '2026-11-24T17:00:00Z', finished_at: '2026-11-24T18:44:26Z' });
const hole3 = (labels: Record<string, string> = { hole_states: DABFA9 }) =>
	task(`${S}-h03`, 'hole', 'leased', { parent: S, steps: 12, started_at: '2026-11-24T18:50:00Z', labels: { hole: '3', ...labels } });
const WAIT3 = ['Waiting', 'starts when hole 3 is done'];

test('p2m-dabfa9: holes 1, 2 and 4 wait for hole 3, hole 3 is working; every hole shows, the reason in a tooltip', () => {
	const p = planOf([outline, hole3()]);
	assert.equal(p.summary, 'Running: outline 1, hole 3');
	assert.equal(p.outlines[0].text, 'in progress');
	assert.deepEqual(lines(p), [
		['Hole 1', 'waiting', ...WAIT3],
		['Hole 2', 'waiting', ...WAIT3],
		['Hole 3', 'working', 'Working (try 1, 12 steps so far)', ''],
		['Hole 4', 'waiting', ...WAIT3],
	]);
	const html = leafPlanHTML('N30', p);
	assert.match(html, /<li data-hole="4" data-hole-kind="lemma" data-hole-state="waiting" title="starts when hole 3 is done"><span class="p2m-hname">Hole 4<\/span><span class="p2m-htext">Waiting<\/span><\/li>/);
	assert.match(html, /<li data-hole="3" data-hole-kind="lemma" data-hole-state="working"><span class="p2m-hname">Hole 3<\/span><span class="p2m-htext">Working \(try 1, 12 steps so far\)<\/span><\/li>/);
});

test('without the label a hole number without a task waits, "not known yet" only in its tooltip; nothing is guessed', () => {
	const p = planOf([outline, hole3({})]);
	assert.deepEqual(lines(p), [
		['Hole 1', 'waiting', 'Waiting', 'not known yet'],
		['Hole 2', 'waiting', 'Waiting', 'not known yet'],
		['Hole 3', 'working', 'Working (try 1, 12 steps so far)', ''],
	]);
});

test('once hole 3 is solved the held holes work on their own; when it fails they wait for the new outline', () => {
	const own = (h: number, state: PublicTask['state'], outcome: string | null = null) =>
		task(`${S}-h0${h}`, 'hole', state, { parent: S, outcome, ...(state === 'done' ? { steps: 30, started_at: '2026-11-24T19:00:00Z' } : {}), labels: { hole: String(h), hole_states: DABFA9 } });
	assert.deepEqual(lines(planOf([outline, own(3, 'done', 'proved'), own(1, 'queued'), own(2, 'queued'), own(4, 'queued')])), [
		['Hole 1', 'working', 'Working (queued)', ''],
		['Hole 2', 'working', 'Working (queued)', ''],
		['Hole 3', 'solved', 'Solved', 'own task'],
		['Hole 4', 'working', 'Working (queued)', ''],
	]);
	// hole 3 failed twice; a new outline is queued in this one's place
	const a2 = task(`${S}-h03-a2`, 'hole', 'done', { parent: S, try: 2, outcome: 'refused', steps: 30, started_at: '2026-11-24T20:00:00Z', retry_of: `${S}-h03`, labels: { hole: '3', hole_states: DABFA9 } });
	const s2 = task(`t11-${L}-s2`, 'outline', 'queued', { replaces: S, labels: { outline: '2' } });
	const held = ['Waiting', 'hole 3 was not solved; a new outline is being made'];
	assert.deepEqual(lines(planOf([outline, own(3, 'done', 'compile_error'), a2, s2]), 1), [
		['Hole 1', 'waiting', ...held],
		['Hole 2', 'waiting', ...held],
		['Hole 3', 'failed', 'Failed: not proved (a sorry left)', 'not proved in 2 tries (did not compile, then a sorry left); a new outline is being made'],
		['Hole 4', 'waiting', ...held],
	]);
	// a second try queued after the first failed: working again, the earlier try in the tooltip
	const q2 = task(`${S}-h03-a2`, 'hole', 'queued', { parent: S, try: 2, retry_of: `${S}-h03`, labels: { hole: '3', hole_states: DABFA9 } });
	assert.deepEqual(lines(planOf([outline, own(3, 'done', 'refused'), q2]))[2], ['Hole 3', 'working', 'Working (queued, try 2)', 'try 1: a sorry left']);
});

// The shape of the campaign leaf p2m-9c0937 (2026-11-26): outline 1's holes 4 and 6 had ended without a proof and
// showed "Waiting", and outline 1 said "a newer outline was started" while outline 2 had been cancelled before it ran.
test('a hole whose task ended without a proof says Failed and why; only a newer outline that ran was started', () => {
	const HS = '1=inplace 2=wait:inplace 3=prepass:simp 4=wait:inplace 5=wait:inplace 6=wait:inplace 7=wait:inplace';
	const lb = (h?: number) => ({ hole_states: HS, ...(h ? { hole: String(h) } : {}) });
	const ran = { started_at: '2026-11-24T19:00:00Z' };
	const hole = (h: number, state: PublicTask['state'], outcome: string | null = null) =>
		task(`${S}-h0${h}`, 'hole', state, { parent: S, outcome, ...(state === 'queued' ? {} : ran), ...(state === 'done' ? { steps: 30 } : {}), labels: lb(h) });
	const tasks = [
		outline,
		task(`${S}-rest`, 'rest', 'done', { parent: S, outcome: 'rest_done', steps: 12, ...ran, labels: lb() }),
		hole(2, 'leased'), hole(4, 'done', 'refused'), hole(5, 'leased'), hole(6, 'done', 'compile_error'), hole(7, 'leased'),
	];
	const s2 = (state: PublicTask['state'], more: Partial<PublicTask> = {}) => task(`t11-${L}-s2`, 'outline', state, { replaces: S, labels: { outline: '2' }, ...more });
	const working = (h: number) => [`Hole ${h}`, 'working', 'Working (try 1)', ''];
	const want = [
		['Hole 1', 'working', 'Working (the assembly checks it)', 'in place'],
		working(2),
		['Hole 3', 'solved', 'Solved', 'solved automatically: simp'],
		['Hole 4', 'failed', 'Failed: not proved (a sorry left)', ''],
		working(5),
		['Hole 6', 'failed', 'Failed: compile error', ''],
		working(7),
	];
	// outline 2 cancelled before it started: outline 1 is still the current one
	const p = planOf([...tasks, s2('cancelled')]);
	assert.deepEqual(p.outlines.map((o) => [o.gen, o.state, o.text]), [[2, 'abandoned', 'abandoned: cancelled before it started'], [1, 'in_progress', 'in progress']]);
	assert.deepEqual(lines(p), want);
	assert.equal(p.summary, 'Running: outline 1, holes 2, 5 and 7');
	assert.match(leafPlanHTML('N30', p), /<li data-hole="6" data-hole-kind="lemma" data-hole-state="failed"><span class="p2m-hname">Hole 6<\/span><span class="p2m-htext">Failed: compile error<\/span><\/li>/);
	// queued, it has not started; taken by a node, it has
	assert.equal(planOf([...tasks, s2('queued')]).outlines[1].text, 'in progress (a newer outline is queued)');
	assert.equal(planOf([...tasks, s2('leased', ran)]).outlines[1].text, 'in progress (a newer outline was started)');
	// a newer outline cancelled while it was being written did start
	assert.equal(planOf([...tasks, s2('cancelled', { ...ran, steps: 4 })]).outlines[1].text, 'in progress (a newer outline was started)');
	// the holes keep their own states whatever the newer outline does
	assert.deepEqual(lines(planOf([...tasks, s2('queued')])), want);
	// a try stopped by a fault, a try that was out of time, and a hole whose task never ran
	const more = planOf([outline, task(`${S}-h01`, 'hole', 'failed', { parent: S, ...ran, labels: { hole: '1', hole_states: '1=task 2=task 3=task' } }),
		task(`${S}-h02`, 'hole', 'done', { parent: S, outcome: 'timeout', steps: 50, ...ran, labels: { hole: '2', hole_states: '1=task 2=task 3=task' } }),
		task(`${S}-h03`, 'hole', 'queued', { parent: S, labels: { hole: '3', hole_states: '1=task 2=task 3=task' } })]);
	assert.deepEqual(lines(more), [
		['Hole 1', 'failed', 'Failed: stopped by a fault', ''],
		['Hole 2', 'failed', 'Failed: not proved within its budget', ''],
		['Hole 3', 'working', 'Working (queued)', ''],
	]);
});

/** A row of the leaf view (TaskRow). */
function row(id: string, role: string, state: string, outcome: string | null, more: Partial<TaskRow> = {}): TaskRow {
	const ref = taskRef(id)!;
	return {
		id, leaf: 'N30', role, gen: ref.gen, hole: ref.hole, attempt: ref.attempt, state, outcome, steps: null, minutes: null,
		total: null, computed: null, cached: null, output: null, closed_by: null, started: state !== 'queued', ...more,
	};
}

test('every way a hole goes is one of the page’s few states; the route or the reason only in the tooltip', () => {
	const hs = '1=prepass:simp 2=known:Nat.Prime.two_le 3=reused 4=inplace 5=inplace 6=inplace 7=busy 8=wait:inplace 9=task 10=frobnicated';
	const rows = [row(S, 'skeleton', 'finished', 'outline', { steps: 10 }), row(`${S}-rest`, 'rest', 'running', null, { steps: 4, hole_states: hs })];
	const o = outlineView(1, rows, undefined, undefined, false);
	assert.equal(o.state, 'in_progress');
	const working = ['working', 'Working (try 1, 4 steps so far)', 'in place'];
	assert.deepEqual(o.holes.map((h) => [h.label, h.kind, h.state, h.text, h.title]), [
		['Hole 1', 'lemma', 'solved', 'Solved', 'solved automatically: simp'],
		['Hole 2', 'lemma', 'solved', 'Solved', 'already known: Nat.Prime.two_le'],
		['Hole 3', 'lemma', 'solved', 'Solved', 'proved earlier'],
		['Hole 4', 'inplace', ...working],
		['Hole 5', 'inplace', ...working],
		['Hole 6', 'inplace', ...working],
		['Hole 7', 'lemma', 'waiting', 'Waiting', 'a task of an earlier outline works on it'],
		['Hole 8', 'lemma', 'waiting', 'Waiting', 'starts when the holes in place are done'],
		['Hole 9', 'lemma', 'waiting', 'Waiting', 'its own task is not made yet'],
		['Hole 10', 'lemma', 'waiting', 'Waiting', 'not known yet'],
	]);
	// the holes in place once the assembly passed: solved with the outline
	const done = [row(S, 'skeleton', 'finished', 'outline'), row(`${S}-rest`, 'rest', 'finished', 'rest_done', { hole_states: '1=prepass:decide 2=inplace' }),
		row(`${S}-asm`, 'asm', 'finished', 'proved', { hole_states: '1=prepass:decide 2=inplace' })];
	assert.deepEqual(outlineView(1, done, undefined, undefined, false).holes.map((h) => [h.label, h.state, h.text, h.title]), [
		['Hole 1', 'solved', 'Solved', 'solved automatically: decide'],
		['Hole 2', 'solved', 'Solved', 'in place, with the outline'],
		['Assembly', 'proved', 'proved: our full check passed', ''],
	]);
	// a counterexample refutes the outline; the false hole failed, the untried one waits
	const f = outlineView(1, [row(S, 'skeleton', 'finished', 'outline'), row(`${S}-h01`, 'hole', 'cancelled', null, { started: false, hole_states: '1=task 2=false' })], undefined, undefined, false);
	assert.equal(f.text, 'refuted: hole 2 is false');
	assert.deepEqual(f.holes.map((h) => [h.label, h.state, h.text, h.title]), [
		['Hole 1', 'waiting', 'Waiting', 'not tried (outline refuted)'],
		['Hole 2', 'failed', 'Failed: the hole is false', 'false: a counterexample was found'],
	]);
	// the tooltip is in the page's HTML
	assert.match(leafPlanHTML('N30', { summary: '', outlines: [o] }), /title="already known: Nat.Prime.two_le"/);
});

test('the label: one word per hole, anything else left out or unknown', () => {
	assert.deepEqual([...parseHoleStates(DABFA9)!], [[1, { state: 'wait', arg: '3' }], [2, { state: 'wait', arg: '3' }], [3, { state: 'task' }], [4, { state: 'wait', arg: '3' }]]);
	assert.deepEqual([...parseHoleStates('1=prepass:exact? 2=known 3=prepass:<b> x=task 4=')!], [[1, { state: 'prepass', arg: 'exact?' }], [2, { state: 'known' }], [3, { state: 'unknown' }]]);
	assert.equal(parseHoleStates(''), null);
	assert.equal(parseHoleStates(undefined), null);
	assert.equal(parseHoleStates('task task'), null);
});
