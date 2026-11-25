// prove2me's outcome rules (prove2me-outcomes.ts) on fixed data, through every view that uses them: the leaf's
// outlines in words (prove2me-outlines.ts), its task table and counts (prove2me-live.ts) and the table of every task
// (prove2me-tasks.ts). Each case is a shape a trial produced: an assembly that passed or failed, the holes in place
// (`rest_done`), holes closed before a model ran (reuse, ledger, pre-pass), cancellations, a hole disproved, a try
// that left a sorry, and the tasks cancelled when an outline was abandoned (trial 8's p2m-03d4b4, 2026-11-18).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	attemptOf, cancelReason, closedByText, inplaceStanding, isStaleCancel, kindOf, provesLeaf, resultLabel, staleCancelReason, taskRef, tryWord, type Attempt,
} from '../src/data/prove2me-outcomes';
import { leafDetail, detailProved, type Outline, type TaskRow } from '../src/data/prove2me-live';
import { leafPlan, outlineView } from '../src/data/prove2me-outlines';
import { taskResult } from '../src/data/prove2me-tasks';
import type { TaskTableRow } from '../src/data/task-table';

/** A row of the leaf view (TaskRow). */
function row(id: string, role: string, state: string, outcome: string | null, more: Partial<TaskRow> = {}): TaskRow {
	const ref = taskRef(id)!;
	return {
		id, leaf: 'N13', role, gen: ref.gen, hole: ref.hole, attempt: ref.attempt, state, outcome, steps: state === 'finished' ? 100 : null, minutes: null,
		total: null, computed: null, cached: null, output: null, closed_by: null, started: state !== 'queued' && state !== 'cancelled', ...more,
	};
}
/** The same task as a row of the table of every task. */
function trow(r: TaskRow): TaskTableRow {
	const kind = kindOf(r.role);
	return {
		project: 'prove2me', id: r.id, kind, state: r.state === 'finished' ? 'done' : r.state, outcome: r.state === 'finished' ? r.outcome : null, try: r.attempt,
		parent: null, needs: [], retry_of: null, replaces: null, closed_by: r.closed_by ?? null, steps: r.steps, model_min: null, tokens: null,
		started_at: r.started ? '2026-11-18T01:00:00Z' : null, finished_at: null,
		labels: { trial: '8', leaf: 'p2m-03d4b4', alias: 'N13', outline: String(r.gen), ...(r.hole ? { hole: String(r.hole) } : {}) },
	};
}
const holesOf = (rows: TaskRow[], gen = 1, outlines: Outline[] = []) =>
	leafPlan(leafDetail(rows, outlines)).outlines.find((o) => o.gen === gen)!.holes.map((h) => [h.label, h.state, h.text, h.title]);

// Trial 8's p2m-03d4b4 (N13) as the gateway listed it on 2026-11-18: outline 1's four holes all failed their first try,
// hole 1 its second too; the runner then cancelled the other second tries, closed_by naming hole 1's second try, and
// outline 2 started.
const S = 't8-p2m-03d4b4-s1';
const n13 = [
	row(S, 'skeleton', 'finished', 'outline', { steps: 15 }),
	row(`${S}-h01`, 'hole', 'finished', 'compile_error'),
	row(`${S}-h01-a2`, 'hole', 'finished', 'refused'),
	row(`${S}-h02`, 'hole', 'finished', 'refused'),
	row(`${S}-h02-a2`, 'hole', 'cancelled', null, { closed_by: `${S}-h01-a2`, started: true }),
	row(`${S}-h03`, 'hole', 'finished', 'refused'),
	row(`${S}-h03-a2`, 'hole', 'cancelled', null, { closed_by: `${S}-h01-a2` }),
	row(`${S}-h04`, 'hole', 'finished', 'refused'),
	row(`${S}-h04-a2`, 'hole', 'cancelled', null, { closed_by: `${S}-h01-a2` }),
	row('t8-p2m-03d4b4-s2', 'skeleton', 'running', null, { steps: 3 }),
];

test('a hole cancelled because its outline was abandoned says so, after its earlier try; never "closed by another task"', () => {
	const holes = holesOf(n13);
	assert.deepEqual(holes, [
		['Hole 1', 'waiting', 'Waiting', 'not proved in 2 tries (did not compile, then a sorry left); a new outline is being made'],
		['Hole 2', 'waiting', 'Waiting', 'try 1: a sorry left; try 2 cancelled: outline abandoned after hole 1 failed; a new outline is being made'],
		['Hole 3', 'waiting', 'Waiting', 'try 1: a sorry left; try 2 not tried: outline abandoned after hole 1 failed; a new outline is being made'],
		['Hole 4', 'waiting', 'Waiting', 'try 1: a sorry left; try 2 not tried: outline abandoned after hole 1 failed; a new outline is being made'],
	]);
	for (const [, , text] of holes) assert.doesNotMatch(String(text), /closed by another task/);
	const o1 = leafPlan(leafDetail(n13)).outlines.find((o) => o.gen === 1)!;
	assert.equal(o1.state, 'abandoned');
	assert.equal(o1.text, 'abandoned: hole 1 not proved in 2 tries');
	// the table of every task: the cancelled try says why, in the same words
	const rows = n13.map(trow);
	assert.deepEqual(taskResult(rows[4], rows), { text: 'Cancelled: outline abandoned after hole 1 failed', show: 'not_proved' });
	assert.deepEqual(taskResult(rows[3], rows), { text: 'A sorry left', show: 'not_proved' });
	// without the named task's row (another page of the list), its id alone says which hole
	assert.equal(staleCancelReason({ ...rows[4], kind: 'hole', gen: 1, hole: 2 } as Attempt, []), 'outline abandoned after hole 1 failed');
});

test('a stale cancel names what ended the outline: a false hole, a proved leaf, a newer outline', () => {
	const a = (id: string, state: string, outcome: string | null, closed_by: string | null = null): Attempt => {
		const r = taskRef(id)!;
		return { id, kind: r.kind, state, outcome, gen: r.gen, hole: r.hole, try: r.attempt, closed_by };
	};
	const peers = [a('t9-p2m-aaaaaa-s1-h02', 'done', 'disproved'), a('t9-p2m-aaaaaa-s1-asm', 'done', 'proved'), a('t9-p2m-aaaaaa-s2', 'done', 'outline'), a('t9-p2m-aaaaaa-s1-rest', 'done', 'check_error')];
	const c = (by: string) => a('t9-p2m-aaaaaa-s1-h03', 'cancelled', null, by);
	assert.equal(staleCancelReason(c('t9-p2m-aaaaaa-s1-h02'), peers), 'outline refuted: hole 2 is false');
	assert.equal(staleCancelReason(c('t9-p2m-aaaaaa-s1-asm'), peers), 'leaf proved');
	assert.equal(staleCancelReason(c('t9-p2m-aaaaaa-s2'), peers), 'outline replaced');
	assert.equal(staleCancelReason(c('t9-p2m-aaaaaa-s1-rest'), peers), 'outline abandoned after the holes in place failed');
	assert.equal(isStaleCancel(c('x')), true);
	// a task that ended "closed" keeps its closer: it is no cancellation
	assert.equal(isStaleCancel(a('t9-p2m-aaaaaa-s1-h04', 'done', 'closed', 'reuse:ledger')), false);
	assert.equal(staleCancelReason(a('t9-p2m-aaaaaa-s1-h04', 'done', 'closed', 'reuse:ledger'), peers), '');
});

test('a real closed_by keeps its own words: the index, the ledger, the pre-pass, another task', () => {
	assert.deepEqual(closedByText('reuse:formalpedia:Foo.bar'), { state: 'index', text: 'closed by the index (reuse)' });
	assert.deepEqual(closedByText('reuse:ledger:h-3ad4181432a9'), { state: 'ledger', text: 'closed by the ledger' });
	assert.deepEqual(closedByText('reuse:prepass:simp'), { state: 'prepass', text: 'closed by the pre-pass' });
	assert.deepEqual(closedByText('reuse:other'), { state: 'closed', text: 'closed by reuse' });
	assert.deepEqual(closedByText('t6-p2m-3ad60f-s1'), { state: 'prepass', text: 'closed by the pre-pass' }); // run by that outline task
	assert.deepEqual(closedByText('t6-p2m-3ad60f'), { state: 'prepass', text: 'closed by the pre-pass' });
	assert.deepEqual(closedByText('t6-p2m-3ad60f-s1-h02'), { state: 'closed', text: 'closed by another task (t6-p2m-3ad60f-s1-h02)' });
	// in the outline view and the task table
	const rows = [
		row('t6-p2m-3ad60f-s1', 'skeleton', 'finished', 'outline'),
		row('t6-p2m-3ad60f-s1-h01', 'hole', 'finished', 'closed', { closed_by: 'reuse:formalpedia:X.y', steps: 0 }),
		row('t6-p2m-3ad60f-s1-h02', 'hole', 'finished', 'closed', { closed_by: 'reuse:ledger', steps: 0 }),
		row('t6-p2m-3ad60f-s1-h03', 'hole', 'finished', 'proved'),
		row('t6-p2m-3ad60f-s1-asm', 'asm', 'finished', 'proved'),
	];
	assert.deepEqual(holesOf(rows).slice(0, 3), [
		['Hole 1', 'solved', 'Solved', 'already known: X.y'],
		['Hole 2', 'solved', 'Solved', 'proved earlier'],
		['Hole 3', 'solved', 'Solved', 'own task'],
	]);
	const t = rows.map(trow);
	assert.deepEqual(taskResult(t[2], t), { text: 'Closed by the ledger', show: 'proved' });
	assert.equal(leafPlan(leafDetail(rows), [], 'Foo.proved').summary, 'Closed by reuse: imports Foo.proved');
});

test('an assembly or an in-place task that passed proves the leaf, an assembly that failed does not; a hole proved alone never does', () => {
	for (const [kind, state, outcome, want] of [
		['assembly', 'done', 'proved', true], ['asm', 'finished', 'proved', true], ['outline', 'done', 'proved', true], ['skeleton', 'finished', 'proved', true],
		['run', 'done', 'proved', true], ['assembly', 'done', 'assembly_failed', false], ['assembly', 'running', null, false], ['hole', 'done', 'proved', false],
		['rest', 'done', 'proved', true], ['rest', 'done', 'rest_done', false], ['outline', 'done', 'outline', false],
	] as const)
		assert.equal(provesLeaf({ kind, state, outcome }), want, `${kind} ${state} ${outcome}`);
	const base = [row('t7-p2m-bbbbbb-s1', 'skeleton', 'finished', 'outline'), row('t7-p2m-bbbbbb-s1-h01', 'hole', 'finished', 'proved')];
	const ok = leafDetail([...base, row('t7-p2m-bbbbbb-s1-asm', 'asm', 'finished', 'proved')]);
	const bad = leafDetail([...base, row('t7-p2m-bbbbbb-s1-asm', 'asm', 'finished', 'assembly_failed')]);
	assert.equal(detailProved(ok), true);
	assert.equal(detailProved(bad), false);
	assert.equal(leafPlan(ok).summary, 'Proved: outline 1 assembled');
	assert.equal(leafPlan(bad).summary, 'Not proved: outline 1 ended (the assembly did not check)');
	assert.equal(leafPlan(bad).outlines[0].holes.at(-1)!.text, 'did not check');
});

test('the holes in place count when the assembly passes, wait for it before, and are not proved when it fails', () => {
	for (const [asm, want] of [[{ state: 'done', outcome: 'proved' }, 'proved'], [{ state: 'queued', outcome: null }, 'waiting'], [{ state: 'done', outcome: 'compile_error' }, 'not_proved'], [undefined, 'waiting']] as const)
		assert.equal(inplaceStanding({ state: 'done', outcome: 'rest_done' }, asm)?.state, want);
	assert.equal(inplaceStanding({ state: 'finished', outcome: 'proved' }, undefined)?.state, 'proved', 'its own file passed our full check');
	assert.equal(inplaceStanding({ state: 'running', outcome: null }, { state: 'done', outcome: 'proved' }), undefined);
	assert.equal(inplaceStanding(undefined, { state: 'done', outcome: 'proved' }), undefined);
	// trial 6's N03 outline 2: holes 1, 3, 4 as lemmas, hole 2 in place, the in-place task rest_done
	const o: Outline = { gen: 1, holes: 4, lemmas: 3, inplace: 1, prepass: { tried: 3, closed: 0, seconds: 5, by: {} }, prepass_holes: [1, 3, 4].map((h) => ({ hole: h, closed_by: null, timed_out: false })) };
	const S2 = 't6-p2m-dbd69f-s1';
	const rows = (asm: TaskRow | null) => [
		row(S2, 'skeleton', 'finished', 'outline'),
		...[1, 3, 4].map((h) => row(`${S2}-h0${h}`, 'hole', 'finished', 'proved')),
		row(`${S2}-rest`, 'rest', 'finished', 'rest_done'),
		...(asm ? [asm] : []),
	];
	const passed = rows(row(`${S2}-asm`, 'asm', 'finished', 'proved'));
	const failed = rows(row(`${S2}-asm`, 'asm', 'finished', 'compile_error'));
	assert.equal(leafDetail(passed, [o]).gens[0].proved, 4, 'the in-place hole counts once the assembly passed');
	assert.equal(leafDetail(failed, [o]).gens[0].proved, 3);
	assert.equal(leafDetail(rows(null), [o]).gens[0].proved, 3);
	assert.deepEqual(holesOf(passed, 1, [o]).find(([l]) => l === 'Hole 2'), ['Hole 2', 'solved', 'Solved', 'in place, with the outline']);
	assert.deepEqual(holesOf(failed, 1, [o]).find(([l]) => l === 'Hole 2'), ['Hole 2', 'waiting', 'Waiting', 'in place, filled in, but the assembly did not check']);
	assert.deepEqual(holesOf(rows(null), 1, [o]).find(([l]) => l === 'Hole 2'), ['Hole 2', 'working', 'Working (the assembly checks it)', 'in place']);
	// the table of every task: the in-place task is in "Proved" only once the assembly passed
	const restOf = (rs: TaskRow[]) => {
		const t = rs.map(trow);
		return taskResult(t.find((x) => x.kind === 'rest')!, t);
	};
	assert.deepEqual(restOf(passed), { text: 'Filled in, checked by the assembly', show: 'proved' });
	assert.deepEqual(restOf(failed), { text: 'Filled in, checked by the assembly', show: 'not_proved' });
	assert.equal(resultLabel('finished', 'rest_done'), 'Filled in, checked by the assembly');
});

// Trial 11's p2m-a5e581 (N32) as the gateway listed it on 2026-11-25: an outline of two holes, both in place, and its
// in-place task's own file passed our full check, so it ended `proved`, the leaf was done with no assembly, and
// prove2me accepted the proof (the trial runner's outline check returns the full check's verdict; a proved in-place
// task ends the leaf).
test('an in-place task whose own file passed our full check proves the leaf, with no assembly (trial 11’s p2m-a5e581)', () => {
	const S5 = 't11-p2m-a5e581-s1';
	const o: Outline = { gen: 1, holes: 2, lemmas: 0, inplace: 2, prepass: { tried: 0, closed: 0, seconds: 0, by: {} }, prepass_holes: [] };
	const rows = [row(S5, 'skeleton', 'finished', 'outline'), row(`${S5}-rest`, 'rest', 'finished', 'proved')];
	assert.equal(provesLeaf(attemptOf(rows[1])), true);
	const d = leafDetail(rows, [o]);
	assert.equal(detailProved(d), true);
	assert.equal(d.gens[0].proved, 2, 'both holes in place count');
	const p = leafPlan(d);
	assert.equal(p.summary, 'Proved: outline 1, its holes filled in place');
	assert.deepEqual([p.outlines[0].state, p.outlines[0].text], ['proved', 'proved (its holes filled in place)']);
	assert.deepEqual(p.outlines[0].holes.map((h) => [h.label, h.state, h.title]), [['Hole 1', 'solved', 'in place, with the outline'], ['Hole 2', 'solved', 'in place, with the outline']], 'no assembly row: none ran');
	const t = rows.map(trow);
	assert.deepEqual(taskResult(t[1], t), { text: 'Proved: our full check passed', show: 'proved' });
	assert.equal(resultLabel('finished', 'proved'), 'Proved');
	// a task its outline still had open is cancelled for the proved leaf
	const late = row(`${S5}-h01`, 'hole', 'cancelled', null, { closed_by: `${S5}-rest` });
	assert.equal(staleCancelReason(attemptOf(late), [...rows, late].map(attemptOf)), 'leaf proved');
	// `rest_done` still waits for the assembly and proves nothing alone
	const filled = [rows[0], row(`${S5}-rest`, 'rest', 'finished', 'rest_done')];
	assert.equal(detailProved(leafDetail(filled, [o])), false);
	assert.equal(leafDetail(filled, [o]).gens[0].proved, 0);
	assert.equal(leafPlan(leafDetail(filled, [o])).outlines[0].holes[0].text, 'Working (the assembly checks it)');
});

test('a hole disproved refutes its outline; the outline’s other tasks are cancelled for it', () => {
	const S3 = 't9-p2m-cccccc-s1';
	const rows = [
		row(S3, 'skeleton', 'finished', 'outline'),
		row(`${S3}-h01`, 'hole', 'finished', 'proved'),
		row(`${S3}-h02`, 'hole', 'finished', 'disproved'),
		row(`${S3}-h03`, 'hole', 'cancelled', null),
	];
	const o = outlineView(1, rows, undefined, undefined, false);
	assert.equal(o.state, 'refuted');
	assert.equal(o.text, 'refuted: hole 2 is false');
	assert.deepEqual(o.holes.map((h) => [h.label, h.state, h.text, h.title]), [
		['Hole 1', 'solved', 'Solved', 'own task'],
		['Hole 2', 'waiting', 'Waiting', 'false, disproved by the model'],
		['Hole 3', 'waiting', 'Waiting', 'not tried (outline refuted)'],
	]);
	const t = rows.map(trow);
	assert.deepEqual(taskResult(t[2], t), { text: 'Disproved: the hole is false', show: 'not_proved' });
	assert.deepEqual(taskResult(t[3], t), { text: 'Cancelled (outline refuted)', show: 'not_proved' });
	// the trial's data refutes an outline the tasks alone do not (a hole re-judged false outside a task)
	const plain = [trow(row(`${S3}-h04`, 'hole', 'cancelled', null))];
	assert.deepEqual(taskResult(plain[0], plain, new Set(['8|p2m-03d4b4|1'])), { text: 'Cancelled (outline refuted)', show: 'not_proved' });
	assert.equal(cancelReason({ ...plain[0], kind: 'hole', gen: 1, hole: 4 } as Attempt, [], false).text, '');
});

test('cancellations without closed_by: the newer outline, the proved leaf', () => {
	const rows = [
		row('t6-p2m-f181df-s1', 'skeleton', 'finished', 'proved'),
		row('t6-p2m-f181df-s2', 'skeleton', 'cancelled', null),
	];
	const t = rows.map(trow);
	assert.deepEqual(taskResult(t[1], t), { text: 'Cancelled (leaf proved)', show: 'not_proved' });
	assert.equal(leafPlan(leafDetail(rows)).outlines[0].text, 'abandoned: cancelled before it started');
	const r2 = [row('t6-p2m-dbd69f-s1', 'skeleton', 'finished', 'outline'), row('t6-p2m-dbd69f-s1-rest', 'rest', 'cancelled', null, { started: true }), row('t6-p2m-dbd69f-s2', 'skeleton', 'finished', 'outline')];
	const t2 = r2.map(trow);
	assert.deepEqual(taskResult(t2[1], t2), { text: 'Cancelled (outline replaced)', show: 'not_proved' });
});

test('an outcome in plain words: a sorry left, did not compile, never the raw word', () => {
	assert.equal(tryWord('refused'), 'a sorry left');
	assert.equal(tryWord('compile_error'), 'did not compile');
	assert.equal(tryWord('some_new_word'), 'some new word');
	assert.equal(tryWord(null), 'ended');
	assert.equal(resultLabel('finished', 'refused'), 'Not proved: a sorry left');
	assert.equal(resultLabel('done', 'refused'), 'Not proved: a sorry left');
	assert.equal(resultLabel('leased', null), 'Running');
	assert.equal(resultLabel('done', 'new_thing'), 'Ended: new thing');
	const one = [row('t9-p2m-dddddd-s1', 'skeleton', 'finished', 'outline'), row('t9-p2m-dddddd-s1-h01', 'hole', 'finished', 'refused')];
	assert.deepEqual(holesOf(one)[0], ['Hole 1', 'waiting', 'Waiting', 'not proved: a sorry left']);
	const t = one.map(trow);
	assert.deepEqual(taskResult(t[1], t), { text: 'A sorry left', show: 'not_proved' });
});
