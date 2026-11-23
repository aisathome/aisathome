// "Working on now" on the prove2me page (2026-11-13: the page lagged behind the trial): a trial's tasks turned
// into the leaves the page lists, each with a table of its tasks (2026-11-14: "I also want insights into the
// tasks"). The build renders the committed JSON's list with the same code; in the browser the tasks come from the
// gateway's GET /v1/tasks, read into the QueueTask shape below (prove2me-current.ts). No data here, so the browser's
// bundle stays small.
//
// Tokens (2026-11-17: the computed tokens lead): "tokens computed" are what the GPU processed fresh
// (what the model read without the cache, plus what it wrote); "with cache re-reads" adds what it read again from the
// cache, which is most of an agent's reading. The gateway's explicit fields (tokens_computed, tokens_with_cache) give
// both for a task whose worker reported its cache reads (from 2026-11-14 on); a task finished before that takes its
// counts from the build's data (the task's own records); with neither, its total is unknown and the leaf's total
// with cache re-reads says "at least".
//
// SPDX-License-Identifier: Apache-2.0
import type { CurrentLeaf } from './prove2me';
import { attemptOf, inplaceStanding, provesLeaf, resultLabel as outcomeWords } from './prove2me-outcomes';

/** One task as the leaf view reads it (prove2me-current.ts asQueueTask makes it from GET /v1/tasks). */
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
	/** The try (GET /v1/tasks `try`); absent: read from the id's -aN. */
	attempt?: number;
	outcome?: string;
	steps?: number;
	prompt_tokens?: number;
	/** Present only when the worker reported it: absent is unknown, not zero. */
	cached_tokens?: number;
	completion_tokens?: number;
	model_min?: number;
	/** What closed the task's hole before it ran (GET /v1/tasks `closed_by`: a task id or reuse:<by>). */
	closed_by?: string;
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
	/** What closed the task's hole before it ran (a task id or reuse:<by>), when the task list says so. */
	closed_by?: string | null;
	/** Whether the task ever started (a node took it); absent: not known (the build's data), read from its steps. */
	started?: boolean;
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

const taskState: Record<string, string> = { leased: 'running', queued: 'queued', done: 'finished', failed: 'failed', cancelled: 'cancelled' };

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
		id: t.id, leaf: t.leaf ?? '', role, gen: t.gen ?? 1, hole: t.hole ?? null, attempt: t.attempt ?? Number(/-a([0-9]+)$/.exec(t.id)?.[1] ?? 1),
		state: taskState[t.state] ?? t.state, outcome: t.state === 'done' ? t.outcome ?? null : null,
		steps: t.steps ?? s?.steps ?? null, minutes: t.model_min ?? s?.minutes ?? null,
		total: null, computed: null, cached: null, output: null,
		closed_by: t.closed_by ?? null, started: !!t.since || t.state === 'leased',
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
		// the holes in place count once the assembly passed (prove2me-outcomes.ts inplaceStanding)
		const newest = (role: string) => mine.filter((r) => r.role === role).sort((a, b) => a.attempt - b.attempt).at(-1);
		const inplaceProved = inplaceStanding(newest('rest'), newest('asm'))?.state === 'proved';
		const open = mine.filter((r) => r.role === 'hole' || r.role === 'rest');
		return {
			gen,
			skeleton: mine.find((r) => r.role === 'skeleton'),
			asm: mine.find((r) => r.role === 'asm'),
			n: o?.holes ?? null,
			inplace: o?.inplace ?? null,
			holeTasks: new Set(holes.map((r) => r.hole)).size,
			proved: provedHoles.size + (o?.prepass.closed ?? 0) + (inplaceProved && o ? o.inplace : 0),
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
export const detailProved = (d: LeafDetail) => d.rows.some((r) => provesLeaf(attemptOf(r)));

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

/** A task's state or outcome in words (prove2me-outcomes.ts). */
export const resultLabel = (r: TaskRow): string => outcomeWords(r.state, r.outcome);

/** The hover text of a task's or leaf's tokens. */
export function tokensTitle(total: number | null, computed: number | null, cached: number | null, output: number | null): string {
	if (total === null) return computed === null ? 'Not counted yet' : `${exact(computed)} tokens computed; the cache re-reads of this task are not known`;
	const c = computed ?? 0;
	return `${exact(c)} tokens computed${output !== null ? ` (${exact(output)} written, ${exact(c - output)} read fresh)` : ''}; ${exact(total)} with ${exact(cached ?? 0)} cache re-reads`;
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
export function taskTableHTML(leaf: string, d: LeafDetail, label = 'Tasks'): string {
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
				tokCell('Tokens computed', 'tok', r.computed, tokensTitle(r.total, r.computed, r.cached, r.output), 'data-computed') +
				tokCell('With cache re-reads', 'tok', r.total, tokensTitle(r.total, r.computed, r.cached, r.output), 'data-total') +
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
		`<td data-label="Tokens computed" class="num tok" data-computed="${d.computed}" title="${esc(`${exact(d.computed)} tokens computed`)}">${esc(compact(d.computed))}</td>` +
		`<td data-label="With cache re-reads" class="num tok" data-total="${d.total}" data-exact="${d.exact}" title="${esc(`${exact(d.total)} with cache re-reads${d.exact ? '' : ' at least: the cache re-reads of some tasks are not known'}`)}">${esc(totalText)}</td></tr>`;
	const notes = d.outlines.map((o) => `<p class="outline-note" data-outline-note="${o.gen}">${esc(outlineNote(o))}</p>`).join('');
	return (
		`<details class="p2m-tasks" data-leaf-tasks="${esc(leaf)}"><summary>${esc(label)} (${d.rows.length})</summary>` +
		`<div class="p2m-tasks-wrap"><table><thead><tr><th scope="col">Task</th><th scope="col" class="num">Outline</th><th scope="col" class="num">Try</th>` +
		`<th scope="col">Result</th><th scope="col" class="num">Steps</th><th scope="col" class="num">Model min</th>` +
		`<th scope="col" class="num">Tokens computed</th><th scope="col" class="num">With cache re-reads</th></tr></thead>` +
		`<tbody>${rows}</tbody><tfoot>${foot}</tfoot></table></div>${notes}</details>`
	);
}

/** The leaf's tokens line: "1.6M tokens computed (18.3M with cache re-reads)" ("at least" when some cache re-reads
 * are not known). */
export function leafTokensText(d: LeafDetail, soFar: boolean): string {
	return `${compact(d.computed)} tokens computed${soFar ? ' so far' : ''} (${d.exact ? '' : 'at least '}${compact(d.total)} with cache re-reads)`;
}

// ---- The table of every leaf (2026-11-14): its outcome labels, and the sort the browser applies (filters and sort are
// the browser's; the build's table is in the export's order, by first tried).

export const leafOutcomes = ['accepted', 'reused', 'proved', 'not_proved', 'stopped', 'not_scored'] as const;
export const leafOutcomeLabel: Record<string, string> = {
	accepted: 'Accepted by prove2me',
	reused: 'Closed by reuse',
	proved: 'Proved',
	not_proved: 'Not proved',
	stopped: 'Stopped',
	not_scored: 'Not scored',
};
export const leafRoleLabel: Record<string, string> = { calibration: 'Calibration', open: 'Open', comparison: 'Comparison' };

export type LeafSortKey = 'leaf' | 'theorem' | 'first' | 'outcome' | 'tokens' | 'minutes';
export type LeafSort = { key: LeafSortKey; dir: 'asc' | 'desc' };
/** A row's sort values: text for leaf and theorem, numbers for the rest (null: none, always last). */
export type LeafKeys = { leaf: string; theorem: string; first: number; outcome: number; tokens: number | null; minutes: number | null; order: number };
/** Names and dates go up first, the outcome best first, tokens and minutes most first. */
export const leafFirstDir = (k: LeafSortKey): 'asc' | 'desc' => (k === 'tokens' || k === 'minutes' ? 'desc' : 'asc');
export function compareLeaves(a: LeafKeys, b: LeafKeys, s: LeafSort): number {
	const x = a[s.key], y = b[s.key];
	if (x === null || y === null) return x === y ? a.order - b.order : x === null ? 1 : -1;
	const c = typeof x === 'string' ? x.localeCompare(y as string) : (x as number) - (y as number);
	return (s.dir === 'asc' ? c : -c) || a.order - b.order;
}
