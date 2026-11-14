// "Working on now" on the prove2me page, live (owner, 2026-11-13: the page lagged behind the trial):
// the gateway's public view of prove2me's task queue (GET /v1/projects/prove2me/queue; gateway
// queue_public.go: task ids, states, outcomes, steps, token counts, the trial, leaf and theorem names only)
// turned into the leaves the page lists, each with a table of its tasks (owner, 2026-11-14: "I also
// want insights into the tasks"). The build renders the committed JSON's list with the same code; in the
// browser the live view replaces it when the queue answers. No data here, so the browser's bundle stays
// small.
//
// Tokens (owner, 2026-11-14: the live counts were about a tenth of the real ones): "Qwen tokens" are
// everything the model read, cache re-reads included, and wrote, as on the Tau Ceti page and in the
// trial tables; "computed" are what the GPU processed fresh (read without the cache, plus written). The
// queue's prompt_tokens are the computed prompt only; its cached_tokens (workers from 2026-11-14 on)
// complete them. A task finished before that takes its totals from the build's data (the export
// script's per-task counts from the task's own records); with neither, its total is unknown and the
// leaf's total says "at least".
//
// SPDX-License-Identifier: Apache-2.0
import type { CurrentLeaf } from './prove2me';

/** One task of the public view (only the fields the page uses). */
export interface QueueTask {
	id: string;
	state: 'queued' | 'leased' | 'done' | 'failed' | 'cancelled';
	updated_at: string;
	created_at: string;
	since?: string;
	trial?: string;
	leaf?: string;
	group?: string;
	theorem?: string;
	role?: string;
	gen?: number;
	hole?: number;
	outcome?: string;
	steps?: number;
	prompt_tokens?: number;
	/** Present only when the worker reported it: absent is unknown, not zero. */
	cached_tokens?: number;
	completion_tokens?: number;
	model_min?: number;
}

export interface QueueView {
	updated: string;
	tasks: QueueTask[];
}

/** A task of the running trial in the build's data (site_json.py): prompt tokens include the cache reads; null is unknown. */
export interface StaticTask {
	id: string;
	leaf: string;
	role: string;
	gen: number;
	hole: number | null;
	attempt: number;
	state: string;
	outcome: string | null;
	steps: number | null;
	model_minutes: number | null;
	prompt_tokens: number | null;
	cached_tokens: number | null;
	output_tokens: number | null;
}

/** One outline (generation) of a leaf in the build's data: its holes and what the pre-pass of standard tactics closed. */
export interface Outline {
	gen: number;
	holes: number;
	lemmas: number;
	inplace: number;
	prepass: { tried: number; closed: number; seconds: number; by: Record<string, number> };
	prepass_holes: { hole: number; closed_by: string | null; timed_out: boolean }[];
}

/** A task as the page shows it. Tokens: total = read (cache re-reads included) + written; computed = read without the cache + written. */
export interface TaskRow {
	id: string;
	leaf: string;
	role: string;
	gen: number;
	hole: number | null;
	attempt: number;
	state: string;
	outcome: string | null;
	steps: number | null;
	minutes: number | null;
	total: number | null;
	computed: number | null;
	cached: number | null;
	output: number | null;
}

/** A leaf's tasks summed and per outline. */
export interface LeafDetail {
	rows: TaskRow[];
	/** Sum of the known totals plus, for a task whose total is unknown, its computed tokens: a lower bound unless `exact`. */
	total: number;
	computed: number;
	exact: boolean;
	gens: GenView[];
	outlines: Outline[];
}

export interface GenView {
	gen: number;
	skeleton?: TaskRow;
	asm?: TaskRow;
	/** Holes in the outline (known from the build's data), else null. */
	n: number | null;
	inplace: number | null;
	/** Hole tasks (distinct holes) and how many of those holes are proved (by a task, the pre-pass, or the in-place task). */
	holeTasks: number;
	proved: number;
	running: number;
	queued: number;
	cancelled: number;
}

/** A leaf as the live view sees it: the page's CurrentLeaf plus its tasks. */
export type LiveLeaf = Omit<CurrentLeaf, 'title' | 'url' | 'earlier'> & { tokens: number; proved: boolean; detail: LeafDetail };

const states = ['queued', 'leased', 'done', 'failed', 'cancelled'];
const str = (v: unknown) => typeof v === 'string';
const num = (v: unknown) => v === undefined || (typeof v === 'number' && Number.isFinite(v) && v >= 0);

/** The queue's JSON, or null when it is not what the gateway sends (the page then keeps the build's list). */
export function parseQueue(d: unknown): QueueView | null {
	const v = d as QueueView;
	if (!v || !str(v.updated) || Number.isNaN(Date.parse(v.updated)) || !Array.isArray(v.tasks)) return null;
	const tasks = v.tasks.filter(
		(t) =>
			t && str(t.id) && states.includes(t.state) && str(t.updated_at) &&
			[t.trial, t.leaf, t.group, t.theorem, t.role, t.outcome].every((x) => x === undefined || str(x)) &&
			[t.gen, t.hole, t.steps, t.prompt_tokens, t.cached_tokens, t.completion_tokens, t.model_min].every(num),
	);
	return { updated: v.updated, tasks };
}

const taskState: Record<string, string> = { leased: 'running', queued: 'queued', done: 'finished', failed: 'failed', cancelled: 'cancelled' };

/** The trial the queue is working on: that of the latest open task, else null (nothing runs or waits). */
export function currentTrial(q: QueueView): string | null {
	const open = q.tasks.filter((t) => (t.state === 'leased' || t.state === 'queued') && t.trial);
	open.sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at));
	return open[0]?.trial ?? null;
}

/** A task of the build's data as a row. */
export function fromStatic(t: StaticTask): TaskRow {
	const known = t.prompt_tokens !== null && t.output_tokens !== null;
	return {
		id: t.id, leaf: t.leaf, role: t.role, gen: t.gen, hole: t.hole, attempt: t.attempt, state: t.state, outcome: t.outcome,
		steps: t.steps, minutes: t.model_minutes,
		total: known ? t.prompt_tokens! + t.output_tokens! : null,
		computed: known ? t.prompt_tokens! - (t.cached_tokens ?? 0) + t.output_tokens! : null,
		cached: t.cached_tokens, output: t.output_tokens,
	};
}

/** A task of the live view as a row; its tokens from the build's data when the queue lacks the cache reads. */
export function fromQueue(t: QueueTask, known?: StaticTask): TaskRow {
	const s = known ? fromStatic(known) : undefined;
	const role = t.role ?? (t.leaf && t.id.endsWith(`-${t.leaf}`) ? 'skeleton' : 'other');
	const row: TaskRow = {
		id: t.id, leaf: t.leaf ?? '', role, gen: t.gen ?? 1, hole: t.hole ?? null, attempt: Number(/-a([0-9]+)$/.exec(t.id)?.[1] ?? 1),
		state: taskState[t.state] ?? t.state, outcome: t.state === 'done' ? t.outcome ?? null : null,
		steps: t.steps ?? s?.steps ?? null, minutes: t.model_min ?? s?.minutes ?? null,
		total: null, computed: null, cached: null, output: null,
	};
	const p = t.prompt_tokens ?? 0, o = t.completion_tokens ?? 0;
	if (t.cached_tokens !== undefined) Object.assign(row, { total: p + t.cached_tokens + o, computed: p + o, cached: t.cached_tokens, output: o });
	else if (s && s.total !== null) Object.assign(row, { total: s.total, computed: s.computed, cached: s.cached, output: s.output });
	else if (role === 'asm') Object.assign(row, { total: 0, computed: 0, cached: 0, output: 0 }); // no model
	else if (t.prompt_tokens !== undefined || t.completion_tokens !== undefined) Object.assign(row, { computed: p + o, output: o });
	return row;
}

const roleOrder: Record<string, number> = { skeleton: 0, hole: 1, rest: 2, asm: 3 };
const byPlace = (a: TaskRow, b: TaskRow) =>
	a.gen - b.gen || (roleOrder[a.role] ?? 4) - (roleOrder[b.role] ?? 4) || (a.hole ?? 0) - (b.hole ?? 0) || a.attempt - b.attempt || a.id.localeCompare(b.id);

/** A leaf's tasks, every outline counted: totals, and per outline its holes proved, open and cancelled. */
export function leafDetail(rows: TaskRow[], outlines: Outline[] = []): LeafDetail {
	const rs = [...rows].sort(byPlace);
	const gens = [...new Set(rs.map((r) => r.gen))].sort((a, b) => a - b).map((gen): GenView => {
		const mine = rs.filter((r) => r.gen === gen);
		const o = outlines.find((x) => x.gen === gen);
		const holes = mine.filter((r) => r.role === 'hole' && r.hole);
		const provedHoles = new Set(holes.filter((r) => r.state === 'finished' && r.outcome === 'proved').map((r) => r.hole));
		const rest = mine.find((r) => r.role === 'rest' && r.state === 'finished' && r.outcome === 'proved');
		const open = mine.filter((r) => r.role === 'hole' || r.role === 'rest');
		return {
			gen,
			skeleton: mine.find((r) => r.role === 'skeleton'),
			asm: mine.find((r) => r.role === 'asm'),
			n: o?.holes ?? null,
			inplace: o?.inplace ?? null,
			holeTasks: new Set(holes.map((r) => r.hole)).size,
			proved: provedHoles.size + (o?.prepass.closed ?? 0) + (rest && o ? o.inplace : 0),
			running: open.filter((r) => r.state === 'running').length,
			queued: open.filter((r) => r.state === 'queued').length,
			cancelled: open.filter((r) => r.state === 'cancelled').length,
		};
	});
	return {
		rows: rs,
		total: rs.reduce((a, r) => a + (r.total ?? r.computed ?? 0), 0),
		computed: rs.reduce((a, r) => a + (r.computed ?? 0), 0),
		exact: rs.every((r) => r.total !== null || (r.computed === null && r.state !== 'finished' && r.state !== 'failed')),
		gens,
		outlines,
	};
}

/** Where a leaf stands, from its tasks: each outline, then its holes ("k of N holes proved") and what is open. */
export function detailProgress(d: LeafDetail): string {
	const many = d.gens.length > 1;
	const outline: Record<string, string> = { running: 'writing the outline', queued: 'outline queued', failed: 'outline stopped by a fault', cancelled: 'outline cancelled' };
	const parts = d.gens.map((g) => {
		const name = many ? `outline ${g.gen}` : 'outline';
		const sk = g.skeleton;
		if (!sk) return '';
		if (sk.state !== 'finished') return many ? `${outline[sk.state]?.replace('outline', name) ?? name}` : outline[sk.state] ?? '';
		if (sk.outcome === 'proved') return `${name}: proved without holes`;
		if (sk.outcome !== 'outline') return `${name}: ${resultLabel(sk).toLowerCase()}`;
		const holes =
			g.n !== null ? `${g.proved} of ${plural(g.n, 'hole')} proved` : g.holeTasks ? `${plural(g.holeTasks, 'hole task')}: ${g.proved} proved` : '';
		const open = [g.running ? `${g.running} running` : '', g.queued ? `${g.queued} queued` : '', g.cancelled ? `${g.cancelled} cancelled` : '']
			.filter(Boolean)
			.join(', ');
		const asm = g.asm ? { running: 'assembling', queued: 'assembly queued', failed: 'assembly stopped by a fault', cancelled: 'assembly cancelled' }[g.asm.state] ?? (g.asm.outcome === 'proved' ? 'assembled' : 'assembly failed') : '';
		const body = [holes + (open ? `, ${open}` : ''), g.asm && g.asm.state !== 'cancelled' ? asm : ''].filter(Boolean).join('; ');
		return many ? `${name}: ${body || 'done'}` : `outline done${body ? `; ${body}` : ''}`;
	});
	return parts.filter(Boolean).join('; ');
}

/** The leaf's state from its tasks: running, queued, cancelled (its newest outline's tasks were), else as its newest outline ended. */
export function detailState(d: LeafDetail): string {
	const has = (s: string) => d.rows.some((r) => r.state === s);
	if (has('running')) return 'running';
	if (has('queued')) return 'queued';
	const g = d.gens[d.gens.length - 1];
	if (g && (g.cancelled || g.skeleton?.state === 'cancelled')) return 'cancelled';
	return g?.skeleton?.state ?? 'finished';
}

/** Proved: an outline without holes, or an assembly, that our full check passed. */
export const detailProved = (d: LeafDetail) =>
	d.rows.some((r) => r.state === 'finished' && r.outcome === 'proved' && (r.role === 'asm' || r.role === 'skeleton'));

/**
 * The leaves of a trial, as the export script's leaf_view does it: a leaf runs while any of its tasks
 * runs, waits while any is queued, is cancelled when its newest outline's tasks were, else as its outline
 * ended. Every outline (generation) counts. `order` lists leaves first in the build's order; `known` is
 * the build's data of the same trial (tasks with their totals, each leaf's outlines).
 */
export function liveLeaves(
	q: QueueView,
	trial: string,
	order: string[] = [],
	known: { tasks?: StaticTask[]; outlines?: Record<string, Outline[]> } = {},
): LiveLeaf[] {
	const mine = q.tasks.filter((t) => t.trial === trial && t.leaf);
	const statics = new Map((known.tasks ?? []).map((t) => [t.id, t]));
	const rows = mine.map((t) => fromQueue(t, statics.get(t.id)));
	// a task of the build's data that the queue no longer lists (it keeps at most 200 finished ones) still counts
	const live = new Set(rows.map((r) => r.id));
	for (const t of known.tasks ?? []) if (!live.has(t.id) && mine.some((m) => m.leaf === t.leaf)) rows.push(fromStatic(t));
	const leaves = [...new Set(rows.map((r) => r.leaf))];
	const pos = (l: string) => (order.includes(l) ? order.indexOf(l) : order.length);
	const first = (l: string) => Math.min(...mine.filter((t) => t.leaf === l).map((t) => Date.parse(t.created_at) || 0));
	leaves.sort((a, b) => pos(a) - pos(b) || first(a) - first(b));
	return leaves.map((leaf) => {
		const ts = mine.filter((t) => t.leaf === leaf);
		const detail = leafDetail(rows.filter((r) => r.leaf === leaf), known.outlines?.[leaf] ?? []);
		const g = detail.gens[detail.gens.length - 1];
		const view: LiveLeaf = {
			leaf,
			group: (ts.find((t) => t.group)?.group ?? 'open') as LiveLeaf['group'],
			theorem: ts.find((t) => t.theorem)?.theorem ?? '',
			state: detailState(detail),
			tokens: detail.total,
			proved: detailProved(detail),
			detail,
		};
		if (g?.skeleton) view.outline = g.skeleton.state;
		return view;
	});
}

/** Where a v5 leaf stands: its outline, then its hole tasks (the build's list without per-task data). */
export function leafProgress(l: Pick<CurrentLeaf, 'outline' | 'holes' | 'hole_tasks'>) {
	const outline: Record<string, string> = { running: 'writing the outline', queued: 'outline queued', finished: 'outline done', failed: 'outline stopped by a fault' };
	const parts = [outline[l.outline ?? ''] ?? ''];
	if (l.holes) parts.push(`${l.holes.proved} of ${plural(l.holes.n, 'hole')} proved`);
	else if (l.hole_tasks) {
		const h = l.hole_tasks;
		const open = [h.running ? `${h.running} running` : '', h.queued ? `${h.queued} queued` : ''].filter(Boolean).join(', ');
		parts.push(`${plural(h.n, 'hole task')}: ${h.proved} proved${open ? `, ${open}` : ''}`);
	}
	return parts.filter(Boolean).join('; ');
}

export const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

export const stateLabel: Record<string, string> = {
	running: 'Running',
	queued: 'Queued',
	finished: 'Finished',
	failed: 'Stopped by a fault',
	cancelled: 'Cancelled',
};

/** 16968593 -> "17.0M", 405114 -> "405k" (the Tau Ceti page's format). */
export const compact = (v: number) => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v)));

// ---- the per-leaf task table (one renderer for the build and the browser: an HTML string, every value escaped)

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const exact = (v: number) => new Intl.NumberFormat('en').format(v);

/** A task's role: Outline, Hole N, In place (the holes left in the outline's file, one task), Assembly. */
export function roleLabel(r: TaskRow): string {
	return { skeleton: 'Outline', hole: `Hole ${r.hole ?? '?'}`, rest: 'In place', asm: 'Assembly' }[r.role] ?? 'Task';
}

/** A task's state or outcome in words. */
export function resultLabel(r: TaskRow): string {
	if (r.state !== 'finished') return stateLabel[r.state] ?? r.state;
	const o: Record<string, string> = {
		proved: 'Proved',
		outline: 'Outline written',
		refused: 'Not proved: a sorry left',
		compile_error: 'Not proved: did not compile',
		no_solution: 'Not proved: no solution file',
		'no-progress': 'Not proved: stopped, no progress',
		infra: 'Not proved: a fault on our side',
		assembly_failed: 'Assembly did not check',
		no_manifest: 'Not assembled: no outline record',
	};
	return o[r.outcome ?? ''] ?? (r.outcome ? `Ended: ${r.outcome}` : 'Finished');
}

/** The hover text of a task's or leaf's tokens. */
export function tokensTitle(total: number | null, computed: number | null, cached: number | null, output: number | null): string {
	if (total === null) return computed === null ? 'Not counted yet' : `${exact(computed)} computed; the cache reads of this task are not known`;
	return `${exact(total)} Qwen tokens: ${output !== null ? `${exact(output)} written, ` : ''}${exact(total - (output ?? 0))} read, of them ${exact(cached ?? 0)} from the cache; ${exact(computed ?? 0)} computed`;
}

const prepassText = (o: Outline | undefined, r: TaskRow) => {
	if (o && r.role === 'rest' && r.attempt === 1) return `the ${plural(o.inplace, 'hole')} left in place, in one task`;
	if (!o || r.role !== 'hole' || r.attempt !== 1) return '';
	const h = o.prepass_holes.find((x) => x.hole === r.hole);
	if (!h) return '';
	return h.closed_by ? `pre-pass closed it (${h.closed_by})` : h.timed_out ? 'pre-pass timed out' : 'pre-pass did not close it';
};

/** An outline in one line: its holes, how they were split, and the pre-pass. */
export function outlineNote(o: Outline): string {
	const lemmas = o.lemmas === 1 ? '1 extracted as a lemma' : `${o.lemmas} extracted as lemmas`;
	const split = o.inplace ? `${lemmas}, ${o.inplace} left in place` : o.lemmas === o.holes ? (o.holes === 1 ? 'extracted as a lemma' : 'all extracted as lemmas') : lemmas;
	const pp = o.prepass.tried ? `; the pre-pass of standard tactics closed ${o.prepass.closed} of ${o.prepass.tried} (${o.prepass.seconds} s)` : '';
	return `Outline ${o.gen}: ${plural(o.holes, 'hole')}, ${split}${pp}.`;
}

const tokCell = (label: string, cls: string, v: number | null, title: string, attr: string) =>
	`<td data-label="${label}" class="num ${cls}" ${attr}="${v ?? ''}" title="${esc(title)}">${v === null ? '–' : esc(compact(v))}</td>`;

/** The leaf's tasks as an expandable table (a details element), with a totals row and a note per outline. */
export function taskTableHTML(leaf: string, d: LeafDetail): string {
	if (!d.rows.length) return '';
	const outl = (g: number) => d.outlines.find((o) => o.gen === g);
	const rows = d.rows
		.map((r) => {
			const pp = prepassText(outl(r.gen), r);
			const cls = r.state === 'finished' && r.outcome === 'proved' ? 'proved' : r.state === 'cancelled' ? 'cancelled' : r.state === 'running' ? 'running' : '';
			return (
				`<tr data-task="${esc(r.id)}" data-role="${esc(r.role)}" data-gen="${r.gen}" data-state="${esc(r.state)}"${r.outcome ? ` data-outcome="${esc(r.outcome)}"` : ''}>` +
				`<th scope="row">${esc(roleLabel(r))}${pp ? `<span class="sub" data-prepass>${esc(pp)}</span>` : ''}</th>` +
				`<td data-label="Outline" class="num">${r.gen}</td>` +
				`<td data-label="Try" class="num">${r.attempt}</td>` +
				`<td data-label="Result" class="result ${cls}">${esc(resultLabel(r))}</td>` +
				`<td data-label="Steps" class="num">${r.steps ?? '–'}</td>` +
				`<td data-label="Model min" class="num">${r.minutes === null ? '–' : r.minutes.toFixed(1)}</td>` +
				tokCell('Qwen tokens', 'tok', r.total, tokensTitle(r.total, r.computed, r.cached, r.output), 'data-total') +
				tokCell('Computed', 'tok', r.computed, tokensTitle(r.total, r.computed, r.cached, r.output), 'data-computed') +
				`</tr>`
			);
		})
		.join('');
	const mins = d.rows.reduce((a, r) => a + (r.minutes ?? 0), 0);
	const steps = d.rows.reduce((a, r) => a + (r.steps ?? 0), 0);
	const totalText = `${d.exact ? '' : 'at least '}${compact(d.total)}`;
	const foot =
		`<tr data-task="all"><th scope="row">All ${d.rows.length} tasks</th><td></td><td></td><td></td>` +
		`<td data-label="Steps" class="num">${steps}</td><td data-label="Model min" class="num">${mins.toFixed(1)}</td>` +
		`<td data-label="Qwen tokens" class="num tok" data-total="${d.total}" data-exact="${d.exact}" title="${esc(`${exact(d.total)} Qwen tokens${d.exact ? '' : ' at least: the cache reads of some tasks are not known'}`)}">${esc(totalText)}</td>` +
		`<td data-label="Computed" class="num tok" data-computed="${d.computed}" title="${esc(`${exact(d.computed)} computed`)}">${esc(compact(d.computed))}</td></tr>`;
	const notes = d.outlines.map((o) => `<p class="outline-note" data-outline-note="${o.gen}">${esc(outlineNote(o))}</p>`).join('');
	return (
		`<details class="p2m-tasks" data-leaf-tasks="${esc(leaf)}"><summary>Tasks (${d.rows.length})</summary>` +
		`<div class="p2m-tasks-wrap"><table><thead><tr><th scope="col">Task</th><th scope="col" class="num">Outline</th><th scope="col" class="num">Try</th>` +
		`<th scope="col">Result</th><th scope="col" class="num">Steps</th><th scope="col" class="num">Model min</th>` +
		`<th scope="col" class="num">Qwen tokens</th><th scope="col" class="num">Computed</th></tr></thead>` +
		`<tbody>${rows}</tbody><tfoot>${foot}</tfoot></table></div>${notes}</details>`
	);
}

/** The leaf's tokens line: "18.3M Qwen tokens, 1.6M computed" (with "at least" when some cache reads are not known). */
export function leafTokensText(d: LeafDetail, soFar: boolean): string {
	return `${d.exact ? '' : 'at least '}${compact(d.total)} Qwen tokens${soFar ? ' so far' : ''}, ${compact(d.computed)} computed`;
}
