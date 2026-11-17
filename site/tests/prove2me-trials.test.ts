// The prove2me page's live trials (decision 0021): every trial from trial 6 on comes from the gateway's GET /v1/tasks,
// page after page, not only the data document's current trial; trials 1 to 5 stay the build's.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fetchAllTasks, isLiveTrial, parseCurrent, taskTableRows, type PublicTask, type TasksView } from '../src/data/prove2me-current';
import { liveTrialHTML, liveTrials, liveTrialsNote } from '../src/data/prove2me-trials';
import { mergeRows, type TaskTableRow } from '../src/data/task-table';

let clock = 0;
/** One task of GET /v1/tasks (gateway tasks_public.go). */
function task(id: string, trial: string, leaf: string, role: string, state: PublicTask['state'], more: Partial<PublicTask> & { labels?: Record<string, string> } = {}): PublicTask {
	const { labels, ...rest } = more;
	const at = new Date(Date.UTC(2026, 10, 16, 8, 0, clock++)).toISOString();
	return {
		project: 'prove2me', id, kind: 'p2m-v8', role, state, outcome: null, try: 1, parent: null, needs: [], retry_of: null, replaces: null,
		closed_by: null, labels: { trial, leaf, group: 'open', scaffold: 'v8', outline: '1', ...labels },
		steps: null, model_min: null, prompt_tokens: null, cached_tokens: null, completion_tokens: null,
		created_at: at, started_at: null, finished_at: null, updated_at: at, live: null, ...rest,
	};
}
const done = (outcome: string, n = 1) => ({
	state: 'done' as const, outcome, steps: 10 * n, model_min: 5 * n, prompt_tokens: 1000 * n, cached_tokens: 9000 * n, completion_tokens: 100 * n,
	started_at: '2026-11-16T09:00:00Z', finished_at: '2026-11-16T10:00:00Z',
});

// Trial 7 ended (two leaves: one proved by its assembly, one not), trial 8 runs (the document's), a probe round of trial
// 8, a frozen trial-5 task, and a test's task that is no trial.
const A = 'p2m-aaaaaa', B = 'p2m-bbbbbb', C = 'p2m-cccccc';
const t7 = [
	task(`t7-${A}-s1`, 't7', A, 'outline', 'done', { ...done('outline'), labels: { alias: 'N06', group: 'calibration' } }),
	task(`t7-${A}-s1-h01`, 't7', A, 'hole', 'done', { ...done('proved'), parent: `t7-${A}-s1`, labels: { alias: 'N06', group: 'calibration', hole: '1' } }),
	task(`t7-${A}-s1-asm`, 't7', A, 'assembly', 'done', { ...done('proved'), steps: 0, model_min: 0, prompt_tokens: 0, cached_tokens: 0, completion_tokens: 0, labels: { alias: 'N06', group: 'calibration' } }),
	task(`t7-${B}-s1`, 't7', B, 'outline', 'done', { ...done('compile_error', 2), finished_at: '2026-11-17T01:00:00Z', labels: { alias: 'N07' } }),
	task(`t7-${B}-s2`, 't7', B, 'outline', 'cancelled', { replaces: `t7-${B}-s1`, labels: { alias: 'N07', outline: '2' } }),
];
const t8 = [
	task(`t8-${C}-s1`, 't8', C, 'outline', 'leased', { started_at: '2026-11-17T15:00:00Z', labels: { alias: 'N15', scaffold: 'v9' } }),
	task(`t8-${A}-s1`, 't8', A, 'outline', 'queued', { labels: { alias: 'N06', scaffold: 'v9' } }),
];
const t8p = [task(`t8p-${C}-s1`, 't8p', C, 'outline', 'done', { ...done('outline'), labels: { alias: 'N15', scaffold: 'v8' } })];
const frozen = [task('t5-F01-s2', 't5', 'F01', 'skeleton', 'done', { ...done('outline'), cached_tokens: null, labels: { theorem: 'X.f01' } })];
const other = [task('e2e1-C07', 'e2e1', 'C07', 'outline', 'done', done('proved'))];
const all = [...t8, ...t8p, ...t7, ...frozen, ...other];
const view = (tasks: PublicTask[]): TasksView => ({ updated: '2026-11-17T16:00:00Z', tasks });

const doc = parseCurrent({
	schema: 'prove2me-current/2', project: 'prove2me', updated_at: '2026-11-17T15:47:29Z',
	trial: { id: 't8', number: 8, scaffold: 'v9', start: '2026-11-17', status: 'running', description: '', budget: '' },
	leaves: [{ id: C, alias: 'N15', role: 'open', theorem: 'Y.n15', url: 'https://prove2.me/theorems/cccccc00-0000-4000-8000-000000000000' }],
	submissions: [],
})!;

test('a trial label is live from trial 6 on, probe rounds too; frozen trials and other labels are not', () => {
	for (const l of ['t6', 't7', 't8', 't6p', 't12p', 't100']) assert.equal(isLiveTrial(l), true, l);
	for (const l of ['t1', 't5', 't5p', 'e2e1', 't7x', '7', '', undefined]) assert.equal(isLiveTrial(l), false, String(l));
});

test('the task table takes every live trial from the gateway, with or without the document', () => {
	for (const d of [doc, null]) {
		const rows = taskTableRows(view(all), d);
		const ids = rows.map((r) => r.id);
		// trial 7 is there although the document names trial 8 (the bug: it vanished when trial 8 started)
		for (const t of [...t7, ...t8, ...t8p]) assert.ok(ids.includes(t.id), `${t.id} missing (doc: ${!!d})`);
		assert.ok(!ids.includes('t5-F01-s2'), 'a frozen trial’s task replaced the build’s');
		assert.ok(!ids.includes('e2e1-C07'), 'a task of no trial');
		assert.equal(new Set(ids).size, ids.length);
		const r7 = rows.find((r) => r.id === `t7-${B}-s1`)!;
		assert.deepEqual(r7.labels, { trial: '7', leaf: B, alias: 'N07', outline: '1' });
		assert.equal(r7.tokens, 2000 + 18000 + 200);
		assert.equal(rows.find((r) => r.id === `t8p-${C}-s1`)!.labels.trial, '8p');
		assert.equal(rows.find((r) => r.id === `t8-${C}-s1`)!.state, 'running');
	}
});

test('a task before v7 names its leaf by alias: the build’s theorem -> id finds it', () => {
	const old = task('t6p-N05', 't6p', 'N05', 'skeleton', 'done', { ...done('outline'), labels: { theorem: 'Z.n05' } });
	const [r] = taskTableRows(view([old]), null, { 'Z.n05': 'p2m-f181df' });
	assert.equal(r.labels.leaf, 'p2m-f181df');
	assert.equal(r.labels.alias, 'N05');
	assert.equal(r.kind, 'outline');
	// unknown: the alias stands in, the leaf id stays empty (the group cell shows the alias)
	const [u] = taskTableRows(view([old]), null);
	assert.equal(u.labels.leaf, '');
	assert.equal(u.labels.alias, 'N05');
});

const row = (id: string, trial: string, tokens: number | null = null): TaskTableRow => ({
	project: 'prove2me', id, kind: 'outline', state: 'done', outcome: 'outline', try: 1, parent: null, needs: [], retry_of: null, replaces: null,
	steps: 1, model_min: 1, tokens, started_at: null, finished_at: null, labels: { trial, leaf: A, alias: 'N06', outline: '1' },
});

test('live rows replace the build’s of the same id in place and join after them, one row per id', () => {
	const built = [row('t5-a', '5', 1), row('t6-a', '6', 1), row('t6-b', '6', 1), row('t7-a', '7', 1)];
	const live = [row('t7-a', '7', 2), row('t6-a', '6', 2), row('t7-b', '7', 2), row('t7-b', '7', 3), row('t8-a', '8', 2)];
	const out = mergeRows(built, live);
	assert.deepEqual(out.map((r) => r.id), ['t5-a', 't6-a', 't6-b', 't7-a', 't7-b', 't8-a']);
	assert.deepEqual(out.map((r) => r.tokens), [1, 2, 1, 2, 2, 2]);
	// without the gateway the build's rows stand
	assert.deepEqual(mergeRows(built, []), built);
	// the whole path: the build without trial 7 (or with it) and the gateway's tasks
	for (const b of [built.filter((r) => r.labels.trial !== '7'), built]) {
		const merged = mergeRows(b, taskTableRows(view(all), doc));
		const ids = merged.map((r) => r.id);
		assert.equal(new Set(ids).size, ids.length);
		for (const t of t7) assert.ok(ids.includes(t.id), t.id);
		assert.equal(merged.find((r) => r.id === 't5-a')!.tokens, 1);
	}
});

test('every page of GET /v1/tasks, by its cursor, up to a bound', async () => {
	const pages = [all.slice(0, 3), all.slice(3, 6), all.slice(6)];
	const urls: string[] = [];
	const get = async (url: string) => {
		urls.push(url);
		const m = /&after=([^&]+)$/.exec(url);
		const i = m ? Number(decodeURIComponent(m[1]).split('~')[0]) : 0;
		return { updated: `2026-11-17T16:00:0${i}Z`, projects: ['prove2me'], tasks: pages[i], next: i + 1 < pages.length ? `${i + 1}~prove2me~${pages[i].at(-1)!.id}` : null };
	};
	const v = await fetchAllTasks(get, '/v1/tasks?project=prove2me&limit=3');
	assert.ok(v);
	assert.deepEqual(v.tasks.map((t) => t.id), all.map((t) => t.id));
	assert.equal(v.updated, '2026-11-17T16:00:00Z');
	assert.equal(urls.length, 3);
	assert.equal(urls[1], `/v1/tasks?project=prove2me&limit=3&after=${encodeURIComponent(`1~prove2me~${pages[0][2].id}`)}`);
	// the bound: two pages at most
	urls.length = 0;
	const two = await fetchAllTasks(get, '/v1/tasks?project=prove2me&limit=3', 2);
	assert.equal(urls.length, 2);
	assert.equal(two!.tasks.length, 6);
	// a page that fails, or is not a page: nothing (the page keeps what it shows)
	const failing = async (url: string) => (url.includes('after=') ? Promise.reject(new Error('timeout')) : get(url));
	assert.equal(await fetchAllTasks(failing), null);
	assert.equal(await fetchAllTasks(async () => ({ tasks: {} })), null);
	// a cursor that comes back ends the walk; a task seen twice counts once
	let n = 0;
	const loop = async () => ({ updated: '2026-11-17T16:00:00Z', tasks: [all[n++ % 2]], next: 'same' });
	const v2 = await fetchAllTasks(loop, '/v1/tasks?project=prove2me', 20);
	assert.equal(n, 2);
	assert.equal(v2!.tasks.length, 2);
});

test('the trials table gets an ended trial the build lacks, counted from its tasks', () => {
	const rows = liveTrials(view(all), ['1', '2', '3', '4', '5', '6', '6p', '7p']);
	// trial 8 still runs; the probe round of trial 8 has ended; trial 5 is frozen
	assert.deepEqual(rows.map((r) => r.trial), ['7', '8p']);
	const [r7, p8] = rows;
	assert.deepEqual([r7.all, r7.calibration, r7.open], [{ proved: 1, runs: 2 }, { proved: 1, runs: 1 }, { proved: 0, runs: 1 }]);
	assert.equal(r7.start, '2026-11-16');
	assert.equal(r7.end, '2026-11-17');
	assert.equal(r7.scaffold, 'v8');
	assert.equal(r7.model_min, 5 + 5 + 0 + 10);
	assert.deepEqual(r7.tok, { prompt_tokens: 10000 + 10000 + 0 + 20000, cached_tokens: 9000 + 9000 + 18000, output_tokens: 100 + 100 + 200 });
	assert.equal(r7.exact, true);
	assert.equal(p8.probe, true);
	assert.equal(p8.number, 8);
	const html = liveTrialHTML(r7);
	assert.match(html, /^<tr data-trial="7" data-source="live">/);
	assert.match(html, /data-cell="proved">1 of 2</);
	assert.match(html, /data-cell="calibration">1 of 1</);
	assert.match(html, /data-tokens="40400"/);
	assert.match(html, /16–17 Nov/);
	assert.match(liveTrialHTML(p8), /data-probes/);
	assert.match(liveTrialHTML(p8), /data-cell="proved">–</);
	assert.equal(liveTrialsNote(rows), 'Live from the gateway’s task records: trial 7 and trial 8’s outline probes. The All row and the counts at the top of the page do not include them yet.');
	// in the build: no live row; a task without its cache count: "at least"
	assert.deepEqual(liveTrials(view(all), ['7', '8p']).map((r) => r.trial), []);
	const partial = liveTrials(view([...t7.slice(0, 3), task(`t7-${B}-s1`, 't7', B, 'outline', 'done', { ...done('outline'), cached_tokens: null })]), []);
	assert.equal(partial[0].exact, false);
	assert.match(liveTrialHTML(partial[0]), /title="At least: /);
	assert.equal(liveTrialsNote([]), '');
});
