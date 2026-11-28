// A proved theorem's tasks (prove2me-proof.ts) on fixed data: the committed records copy and the build's history. The
// leaf's tasks are the trial list's (the prove2me page's), counted here from the raw records; who served each task comes
// from the gateway's `served_by` only, by public name or the kind of GPU, a dash when it is not recorded.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import snapshot from '../src/data/science/prove2me-records.json';
import type { Prove2me } from '../src/data/prove2me';
import { builtDetail, historyOf, type Records } from '../src/data/prove2me-records';
import { recordsOf, withoutServed } from '../src/data/prove2me-build';
import { parseTasks, servedOf, type ServedRow } from '../src/data/prove2me-current';
import { servedLabel, taskTableHTML } from '../src/data/prove2me-live';
import { proofLeaf, proofTasksHTML, servedSummary } from '../src/data/prove2me-proof';

const p2m = raw as unknown as Prove2me;
const h = historyOf(p2m);
const live = () => recordsOf(snapshot)!;
type RawTask = { id: string; role: string | null; steps: number | null; labels: Record<string, string> };
const rawTasks = (snapshot as unknown as { tasks: { tasks: RawTask[] } }).tasks.tasks;
const LEAF = 'p2m-057d7f';
const TRIAL = 15;
const mine = rawTasks.filter((t) => t.labels.trial === `t${TRIAL}` && t.labels.leaf === LEAF);

/** The records with served_by set on tasks by id. */
function withServed(by: Record<string, unknown>): Records {
	const r = live();
	return { ...r, tasks: { ...r.tasks, tasks: r.tasks.tasks.map((t) => (t.id in by ? { ...t, served_by: servedOf(by[t.id]) } : t)) } };
}

test('served_by is read as the gateway sends it; anything else is dropped, the task kept', () => {
	const ok = [{ name: 'ada', class: 'volunteer', nodes: 2, requests: 30 }, { name: null, class: 'pilot', nodes: 1, requests: 3 }];
	assert.deepEqual(servedOf(ok), ok);
	assert.deepEqual(servedOf([{ name: "O'Neil", class: 'trusted', nodes: 1, requests: 1, extra: 'x' }]), [{ name: "O'Neil", class: 'trusted', nodes: 1, requests: 1 }]);
	for (const bad of [null, [], {}, 'ada', [{ name: '<b>x</b>', class: 'volunteer', nodes: 1, requests: 1 }], [{ name: 'ada', class: 'gpu', nodes: 1, requests: 1 }],
		[{ name: 'ada', class: 'volunteer', nodes: 0, requests: 1 }], [{ name: 'ada', class: 'volunteer', nodes: 1, requests: -1 }], [{ name: 'ab', class: 'volunteer', nodes: 1, requests: 1 }],
		[{ name: 7, class: 'volunteer', nodes: 1, requests: 1 }], [{ class: 'volunteer', nodes: 1, requests: 1 }]])
		assert.equal(servedOf(bad), null, JSON.stringify(bad));
	const page = { updated: '2026-11-28T10:00:00Z', tasks: [
		{ ...snapshot.tasks.tasks[0], id: 'a', served_by: ok },
		{ ...snapshot.tasks.tasks[0], id: 'b', served_by: [{ name: 'x<y', class: 'volunteer', nodes: 1, requests: 1 }] },
		{ ...snapshot.tasks.tasks[0], id: 'c' },
	] };
	const v = parseTasks(page)!;
	assert.deepEqual(v.tasks.map((t) => [t.id, t.served_by]), [['a', ok], ['b', null], ['c', undefined]]);
});

test('a GPU in words: its owner’s public name, else only the kind of GPU', () => {
	const row = (r: Partial<ServedRow>): ServedRow => ({ name: null, class: 'volunteer', nodes: 1, requests: 1, ...r });
	assert.equal(servedLabel(row({ name: 'ada' })), 'ada');
	assert.equal(servedLabel(row({ name: 'ada', nodes: 3 })), 'ada (3 GPUs)');
	assert.equal(servedLabel(row({})), 'a volunteer’s GPU');
	assert.equal(servedLabel(row({ nodes: 2 })), '2 volunteers’ GPUs');
	assert.equal(servedLabel(row({ class: 'pilot' })), 'a volunteer’s GPU through BOINC');
	assert.equal(servedLabel(row({ class: 'trusted' })), 'a GPU of the maintainer');
});

test('a proof’s tasks are the trial list’s: every task of the leaf in its trial, the table open with who served each', () => {
	const l = proofLeaf(h, live(), [], { leaf: LEAF, trial: TRIAL });
	assert.ok(l);
	assert.deepEqual(l.detail.rows.map((r) => r.id).sort(), mine.map((t) => t.id).sort());
	const html = proofTasksHTML(l);
	const rows = [...html.matchAll(/<tr data-task="([^"]+)"/g)].map((m) => m[1]).filter((id) => id !== 'all');
	assert.deepEqual(rows.sort(), mine.map((t) => t.id).sort());
	assert.match(html, /<details class="p2m-tasks" data-leaf-tasks="[^"]+" open>/);
	assert.match(html, /<th scope="col">Served by<\/th>/);
	assert.match(html, /data-leaf-plan=/); // its outlines and holes, as on the prove2me page
	// the snapshot predates served_by: a dash for every task, never a guess, and the line says why
	assert.equal([...html.matchAll(/class="served none"/g)].length, rows.length);
	assert.doesNotMatch(html, /class="served" /);
	assert.match(html, /data-th-served="none">No record of which GPU served these tasks/);
	// each hole's place for its Lean statement: there, and empty
	const holes = mine.filter((t) => t.role === 'hole').length;
	assert.ok(holes > 0);
	assert.equal([...html.matchAll(/<span class="sub" data-hole-statement hidden><\/span>/g)].length, holes);
	// the prove2me page's own table is as it was: closed, no column, no statement place
	const plain = taskTableHTML(l.alias, l.detail, 'Every task');
	assert.doesNotMatch(plain, /Served by|data-hole-statement| open>/);
});

test('who served: named by username only, otherwise the kind of GPU; summed over the leaf; escaped', () => {
	const [a, b, c] = mine.filter((t) => t.role !== 'assembly');
	const asm = mine.find((t) => t.role === 'assembly')!;
	const rec = withServed({
		[a.id]: [{ name: "O'Neil", class: 'volunteer', nodes: 1, requests: 20 }, { name: null, class: 'pilot', nodes: 1, requests: 2 }],
		[b.id]: [{ name: "O'Neil", class: 'volunteer', nodes: 2, requests: 5 }],
		[c.id]: [{ name: null, class: 'volunteer', nodes: 1, requests: 4 }],
	});
	const l = proofLeaf(h, rec, [], { leaf: LEAF, trial: TRIAL })!;
	const s = servedSummary(l);
	assert.deepEqual(s.rows, [
		{ name: "O'Neil", class: 'volunteer', nodes: 2, requests: 25 },
		{ name: null, class: 'volunteer', nodes: 1, requests: 4 },
		{ name: null, class: 'pilot', nodes: 1, requests: 2 },
	]);
	assert.equal(s.calls, 31);
	const ran = mine.filter((t) => t.role !== 'assembly' && (t.steps ?? 0) > 0 && ![a.id, b.id, c.id].includes(t.id)).length;
	assert.equal(s.unknown, ran);
	const html = proofTasksHTML(l);
	// over several tasks one GPU and two look alike: the summary counts no GPUs
	assert.match(html, /Served by <strong data-named>O&#39;Neil<\/strong> \(25 calls\), <strong>volunteers’ GPUs<\/strong> \(4\) and <strong>volunteers’ GPUs through BOINC<\/strong> \(2\)/);
	const cell = (id: string) => new RegExp(`<tr data-task="${id}"[^>]*>.*?</tr>`).exec(html)![0];
	assert.match(cell(a.id), /<td data-label="Served by" class="served" data-served="22" title="O&#39;Neil: 20 model calls; a volunteer’s GPU through BOINC: 2 model calls"><span class="srv" data-named>O&#39;Neil <span class="calls">\(20\)<\/span><\/span><span class="srv">a volunteer’s GPU through BOINC <span class="calls">\(2\)<\/span><\/span><\/td>/);
	assert.match(cell(b.id), /<span class="srv" data-named>O&#39;Neil \(2 GPUs\)<\/span><\/td>/);
	assert.match(cell(asm.id), /class="served none" data-served="" title="The assembly runs no model">–/);
	// nothing but the public fields reaches the page
	assert.doesNotMatch(html, /acct-|"nodes"|undefined|null/);
});

test('a proof of a trial the gateway has no list of: the build’s tasks (trial 5), none before (trial 3), none for a reuse', () => {
	const f01 = h.submissions.find((s) => s.trial === 5 && s.verdict === 'accepted')!;
	const built = [builtDetail(p2m, h, 5)!];
	const l = proofLeaf(h, live(), built, { leaf: f01.leaf, trial: 5 });
	assert.ok(l, 'trial 5 from the build');
	assert.ok(l.detail.rows.length > 0 && l.detail.rows.every((r) => !r.served));
	assert.match(proofTasksHTML(l), /No record of which GPU served these tasks/);
	const o04 = h.submissions.find((s) => s.trial === 3 && s.verdict === 'accepted')!;
	assert.equal(proofLeaf(h, live(), [builtDetail(p2m, h, 3)].filter((d) => !!d), { leaf: o04.leaf, trial: 3 }), null);
	assert.equal(proofTasksHTML(null), '');
});

test('the build never names anyone: its records lose served_by, its copy says the GPUs show live', () => {
	const [a] = mine.filter((t) => t.role === 'outline');
	const rec = withServed({ [a.id]: [{ name: 'ada', class: 'volunteer', nodes: 1, requests: 3 }] });
	assert.ok(rec.tasks.tasks.some((t) => t.served_by));
	const built = withoutServed(rec);
	assert.ok(built.tasks.tasks.every((t) => !('served_by' in t)));
	assert.ok(rec.tasks.tasks.some((t) => t.served_by), 'the records it was given are left as they were');
	const html = proofTasksHTML(proofLeaf(h, built, [], { leaf: LEAF, trial: TRIAL }), false);
	assert.doesNotMatch(html, /ada/);
	assert.match(html, /data-th-served="later">Which GPU served each task shows here once the page has read the gateway’s records\./);
	assert.match(html, /class="served none" data-served="" title="Shown once the page has read the gateway’s records">–/);
	// the committed fallback copy holds none either
	assert.ok((snapshot as unknown as { tasks: { tasks: object[] } }).tasks.tasks.every((t) => !('served_by' in t)));
});
