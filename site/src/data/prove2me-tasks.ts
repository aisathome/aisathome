// The prove2me page's task table (2026-11-15; simplified the same day, owner's design): prove2me's options for the
// generic task table (task-table.ts). Rows grouped by trial and leaf (newest trial first), then by outline; each task as
// a label ("Hole 4 (outline 2)", "Probe: outline"), its result in one plain phrase (no raw outcome word), and filters by
// trial (outline probe rounds as "Trial 7 probes"), by leaf, and the "Show" switch. The build's rows come from the
// export (science/prove2me.json "tasks", finished probe rounds included); the live rows of every trial and probe round
// from trial 6 on come from the gateway's GET /v1/tasks (prove2me-current.ts taskTableRows).
//
// SPDX-License-Identifier: Apache-2.0
import type { TaskShow, TaskTableOptions, TaskTableRow } from './task-table';
import { cancelReason, closedByText, inplaceStanding, kindOf, taskRef, tryWord, type Attempt } from './prove2me-outcomes';

export { taskRef };

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** A trial label's place in time: trial N's outline probes ("6p") come just before trial N. */
export const roundNo = (t: string) => (t.endsWith('p') ? Number(t.slice(0, -1)) - 0.5 : Number(t));
export const isProbe = (r: TaskTableRow) => (r.labels.trial ?? '').endsWith('p');
/** "Trial 5", "Trial 7 probes". */
export const roundName = (t: string) => (t.endsWith('p') ? `Trial ${t.slice(0, -1)} probes` : `Trial ${t}`);
const leafOf = (r: TaskTableRow) => r.labels.leaf || r.labels.alias || '';
const outlineOf = (r: TaskTableRow) => Number(r.labels.outline ?? 1);
const same = (a: TaskTableRow, b: TaskTableRow) => a.labels.trial === b.labels.trial && leafOf(a) === leafOf(b);
const kindOrder: Record<string, number> = { run: 0, outline: 0, hole: 1, rest: 2, assembly: 3 };

/** The task in words: "Run", "Outline 2", "Hole 4 (outline 2)", "In place (outline 2)", "Assembly (outline 2)",
 * "Probe: outline", with " · try N" after a first try. */
export function taskLabel(r: TaskTableRow): string {
	const o = r.labels.outline ?? '1';
	const base =
		r.kind === 'run' ? 'Run'
		: r.kind === 'outline' ? (isProbe(r) ? 'Probe: outline' : `Outline ${o}`)
		: r.kind === 'hole' ? `Hole ${r.labels.hole ?? '?'} (outline ${o})`
		: r.kind === 'rest' ? `In place (outline ${o})`
		: r.kind === 'assembly' ? `Assembly (outline ${o})`
		: cap(r.kind);
	return r.try > 1 ? `${base} · try ${r.try}` : base;
}

/** Outlines the trial's data refuted (a hole false), as "<trial label>|<leaf id>|<outline>". */
export type Refuted = Set<string>;
export const refutedKey = (trial: string, leaf: string, outline: number | string) => `${trial}|${leaf}|${outline}`;

/** A row of the table as the outcome rules read it (prove2me-outcomes.ts). */
const attempt = (r: TaskTableRow): Attempt => ({
	id: r.id, kind: kindOf(r.kind), state: r.state, outcome: r.outcome, gen: outlineOf(r), hole: r.labels.hole ? Number(r.labels.hole) : null,
	try: r.try, closed_by: r.closed_by, started: !!r.started_at, steps: r.steps,
});

/** The result in one plain phrase (the outline view's words), and its "Show" set. */
export function taskResult(r: TaskTableRow, rows: TaskTableRow[], refuted?: Refuted): { text: string; show: TaskShow } {
	if (r.state === 'running') return { text: `Running${r.steps ? ` (${r.steps} steps so far)` : ''}`, show: 'running' };
	if (r.state === 'queued') return { text: 'Queued', show: 'running' };
	if (r.state === 'failed') return { text: 'Stopped by a fault', show: 'not_proved' };
	if (r.state === 'cancelled') {
		// "Cancelled: outline abandoned after hole 1 failed" (the task its closed_by names), else "Cancelled (outline replaced)"
		const why = cancelReason(attempt(r), rows.filter((x) => same(x, r)).map(attempt), !!refuted?.has(refutedKey(r.labels.trial, leafOf(r), outlineOf(r))));
		return { text: `Cancelled${why.text ? (why.own ? `: ${why.text}` : ` (${why.text})`) : ''}`, show: 'not_proved' };
	}
	switch (r.outcome) {
		case 'proved':
			return { text: ['hole', 'rest'].includes(r.kind) ? 'Proved' : 'Proved: our full check passed', show: 'proved' };
		case 'outline': {
			const n = Number(r.labels.holes);
			return { text: n > 0 ? `Written, ${n} hole${n === 1 ? '' : 's'}` : 'Outline written', show: 'not_proved' };
		}
		case 'rest_done': {
			// the holes left in place, filled in: proved when the outline's assembly passed our full check
			const asm = rows.filter((x) => same(x, r) && x.kind === 'assembly' && outlineOf(x) === outlineOf(r)).sort((a, b) => a.try - b.try).at(-1);
			return { text: 'Filled in, checked by the assembly', show: inplaceStanding(r, asm)?.state === 'proved' ? 'proved' : 'not_proved' };
		}
		case 'check_error':
			return { text: 'Our check failed', show: 'not_proved' };
		case 'infra':
			return { text: 'Stopped by a fault', show: 'not_proved' };
		case 'disproved':
			return { text: 'Disproved: the hole is false', show: 'not_proved' };
		case 'closed':
			return { text: r.closed_by ? cap(closedByText(r.closed_by).text) : 'Closed', show: 'proved' };
		case 'assembly_failed':
			return { text: 'Assembly did not check', show: 'not_proved' };
		default:
			return { text: cap(tryWord(r.outcome)), show: 'not_proved' };
	}
}

/** What the page knows of a leaf for its group cell: its theorem and prove2me page. */
export type LeafInfo = Record<string, { theorem: string; url: string }>;
const theoremURL = /^https:\/\/prove2\.me\/theorems\/[0-9a-f-]{36}$/;

/** The table's order: newest trial first; in a trial its leaves as they first come; in a leaf by outline, then the
 * outline, its holes, the in-place task and the assembly, each try after the one before. */
export function sortTasks(rows: TaskTableRow[]): TaskTableRow[] {
	const first = new Map<string, number>();
	rows.forEach((r, i) => {
		const k = `${r.labels.trial}|${leafOf(r)}`;
		if (!first.has(k)) first.set(k, i);
	});
	const key = (r: TaskTableRow) => [-roundNo(r.labels.trial ?? '0'), first.get(`${r.labels.trial}|${leafOf(r)}`)!, outlineOf(r), kindOrder[r.kind] ?? 9, Number(r.labels.hole ?? 0), r.try];
	return rows
		.map((r) => ({ r, k: key(r) }))
		.sort((a, b) => {
			for (let i = 0; i < a.k.length; i++) if (a.k[i] !== b.k[i]) return a.k[i] - b.k[i];
			return 0;
		})
		.map((x) => x.r);
}

/** prove2me's options. `leaves`: theorem and page per leaf id; `refuted`: outlines the trial's data says are refuted. */
export function p2mTaskOptions(leaves: LeafInfo, refuted?: () => Refuted): TaskTableOptions {
	let rowsNow: TaskTableRow[] = [];
	return {
		name: 'p2m',
		sort: (rows) => {
			rowsNow = rows;
			return sortTasks(rows);
		},
		group: (r) => {
			const id = r.labels.leaf || '';
			const info = id ? leaves[id] : undefined;
			const alias = r.labels.alias && r.labels.alias !== id ? r.labels.alias : '';
			const thm = info
				? theoremURL.test(info.url)
					? `<a class="tt-thm" href="${esc(info.url)}" title="${esc(info.theorem)}">${esc(info.theorem)}</a>`
					: `<span class="tt-thm" title="${esc(info.theorem)}">${esc(info.theorem)}</span>`
				: '';
			return {
				key: `${r.labels.trial}|${leafOf(r)}`,
				html: `<span class="tt-leaf"${alias ? ` title="${esc(`${alias} in ${roundName(r.labels.trial ?? '').toLowerCase()}`)}"` : ''} data-leaf>${esc(id || alias)}</span>${thm}<span class="tt-round" data-round>${esc(roundName(r.labels.trial ?? ''))}</span>`,
			};
		},
		label: (r, byId, link) => {
			const notes: string[] = [];
			if (r.retry_of) {
				const prev = byId.get(r.retry_of)?.try ?? taskRef(r.retry_of)?.attempt ?? r.try - 1;
				notes.push(`<span data-rel="retry_of">retry of ${link(r.retry_of, `try ${prev}`)}</span>`);
			}
			if (r.replaces) {
				const prev = byId.get(r.replaces)?.labels.outline ?? String(taskRef(r.replaces)?.gen ?? '');
				notes.push(`<span data-rel="replaces">replaces ${link(r.replaces, `outline ${prev}`)}</span>`);
			}
			return { text: taskLabel(r), noteHTML: notes.join('') || undefined };
		},
		result: (r, byId) => taskResult(r, rowsNow.length ? rowsNow : [...byId.values()], refuted?.()),
		selects: [
			{
				key: 'trial',
				title: 'Trial',
				all: 'All trials',
				values: (rows) => [...new Set(rows.map((r) => r.labels.trial).filter(Boolean))].sort((a, b) => roundNo(b) - roundNo(a)).map((t) => [t, roundName(t)]),
				// the newest trial (not its probes)
				initial: (rows) => {
					const n = Math.max(0, ...rows.filter((r) => !isProbe(r)).map((r) => Number(r.labels.trial) || 0));
					return n ? String(n) : '';
				},
			},
			{
				key: 'leaf',
				title: 'Leaf',
				all: 'All leaves',
				values: (rows) => [...new Set(rows.map((r) => r.labels.leaf).filter(Boolean))].sort().map((l) => [l, l]),
			},
		],
	};
}
