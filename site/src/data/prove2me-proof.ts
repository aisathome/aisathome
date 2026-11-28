// One proved theorem's tasks (/science/prove2me/theorems/<leaf>/, "The tasks"): the leaf's own tasks in the trial that
// proved it, each outline, a task per hole, the in-place task and the assembly, with its result, steps, model minutes and
// tokens, and who served its model calls. Nothing new is computed here: the leaf comes from the prove2me page's trial
// lists (prove2me-records.ts trialDetails: the gateway's campaign of the trial, or the build's data of a trial the
// gateway has no campaign of) and is drawn with their renderers (prove2me-outlines.ts leafPlanHTML, prove2me-live.ts
// taskTableHTML), so the two pages never say different things. The build draws it, the browser again every minute.
//
// Who served a task (`served_by` of GET /v1/tasks, gateway tasks_served.go): a GPU is named only by its owner's public
// username, the same opt-in as the leaderboard; any other by the kind of GPU it is. A task the gateway has no record of
// (a trial before it kept one, the build's data of trials 1 to 5) shows a dash, never a guess. Names are shown only live:
// the build's HTML never holds them, so a username cleared on the gateway is gone with its next list.
//
// SPDX-License-Identifier: Apache-2.0
import type { ServedRow } from './prove2me-current';
import { taskTableHTML } from './prove2me-live';
import { leafPlan, leafPlanHTML } from './prove2me-outlines';
import { trialDetails, type DetailLeaf, type History, type Records, type TrialDetail } from './prove2me-records';

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

/** The leaf's own list in the trial that proved it: the trial's list as the prove2me page has it (the build's `built`
 * lists, the gateway's campaign over them when `live` has it), its row of this leaf; null without one. */
export function proofLeaf(h: History, live: Records | null, built: TrialDetail[], t: { leaf: string; trial: number }): DetailLeaf | null {
	const d = trialDetails(built, live, h).find((x) => x.sum.number === t.trial);
	return d?.leaves.find((l) => l.id === t.leaf && l.detail.rows.length > 0) ?? null;
}

/** Who served the leaf's model calls, over all its tasks: a row per name and kind of GPU, most calls first, and how
 * many tasks that ran the model have no record. `nodes` is the most of any one task (see summaryLabel). */
export function servedSummary(l: DetailLeaf): { rows: ServedRow[]; calls: number; unknown: number } {
	const by = new Map<string, ServedRow>();
	let unknown = 0;
	for (const r of l.detail.rows) {
		if (!r.served?.length) {
			if (r.role !== 'asm' && (r.steps ?? 0) > 0) unknown++;
			continue;
		}
		for (const s of r.served) {
			const k = `${s.name ?? ''}\u0000${s.class}`;
			const had = by.get(k);
			// a GPU serves many tasks: the most nodes of any one task, not their sum
			by.set(k, had ? { ...had, nodes: Math.max(had.nodes, s.nodes), requests: had.requests + s.requests } : { ...s });
		}
	}
	const rows = [...by.values()].sort((a, b) => b.requests - a.requests || (a.name ?? '￿').localeCompare(b.name ?? '￿') || a.class.localeCompare(b.class));
	return { rows, calls: rows.reduce((a, r) => a + r.requests, 0), unknown };
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/** A row of the leaf's summary in words: its name, else the kind of GPU, without a count of GPUs (a task's rows count its
 * own nodes; over several tasks the same GPU and two different ones look alike). */
export function summaryLabel(r: ServedRow): string {
	return r.name ?? { volunteer: 'volunteers’ GPUs', pilot: 'volunteers’ GPUs through BOINC', trusted: 'GPUs of the maintainer' }[r.class];
}

/** "Who served it" in one line, as HTML: "Served by ada (41 calls) and volunteers’ GPUs through BOINC (2)."; what is not
 * recorded said so. The build's copy (`live` false) has no names: they show once the browser read the gateway's list. */
export function servedSummaryHTML(l: DetailLeaf, live = true): string {
	if (!live) return `<p class="th-served" data-th-served="later">Which GPU served each task shows here once the page has read the gateway’s records.</p>`;
	const s = servedSummary(l);
	const unknown = s.unknown ? `${plural(s.unknown, 'task')} that ran the model ${s.unknown === 1 ? 'has' : 'have'} no record of the GPU` : '';
	if (!s.rows.length)
		return `<p class="th-served" data-th-served="none">${s.unknown ? 'No record of which GPU served these tasks.' : 'No model calls recorded.'}</p>`;
	const who = s.rows.map((r, i) => `<strong${r.name ? ' data-named' : ''}>${esc(summaryLabel(r))}</strong> (${i === 0 ? plural(r.requests, 'call') : r.requests})`);
	const list = who.length > 1 ? `${who.slice(0, -1).join(', ')} and ${who.at(-1)}` : who[0];
	return `<p class="th-served" data-th-served="${s.calls}">Served by ${list}${unknown ? `; ${esc(unknown)}` : ''}.</p>`;
}

/** The leaf's tasks as HTML (the block's children): who served them, its outlines with every hole, and the table of
 * every task with who served each, open. Empty without a list (a trial before per-task records). The build passes
 * `live` false: its records carry no served_by (prove2me-build.ts withoutServed). */
export function proofTasksHTML(l: DetailLeaf | null, live = true): string {
	if (!l) return '';
	return (
		servedSummaryHTML(l, live) +
		leafPlanHTML(l.alias, leafPlan(l.detail, [], l.reuse), true) +
		taskTableHTML(l.alias, l.detail, 'Every task', { open: true, served: live || 'later', statements: true })
	);
}
