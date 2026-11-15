// The prove2me page's task table (2026-11-15): prove2me's options for the generic task table (task-table.ts). The build's
// rows come from the export (science/prove2me.json "tasks", relations included); the current trial's live rows come from
// the gateway's GET /v1/tasks with the relations the tasks were made with (prove2me-current.ts taskTableRows). A task
// of trials 1 to 5 that a relation names without a row is named from its id (taskRef).
//
// SPDX-License-Identifier: Apache-2.0
import type { TaskTableOptions, TaskTableRow } from './task-table';

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

export const taskKinds: [string, string][] = [
	['outline', 'Outline'],
	['hole', 'Hole'],
	['rest', 'In place'],
	['assembly', 'Assembly'],
	['run', 'Run'],
];
const outcomeText: Record<string, string> = {
	proved: 'Proved',
	outline: 'Outline written',
	refused: 'A sorry left',
	compile_error: 'Did not compile',
	no_solution: 'No solution file',
	'no-progress': 'Stopped, no progress',
	infra: 'A fault on our side',
	assembly_failed: 'Assembly did not check',
	no_manifest: 'No outline record',
	cancelled: 'Stopped by us',
};
export const taskOutcomeLabel = (o: string | null) => (o ? outcomeText[o] ?? o : '–');

// <trial>-<leaf>[-s<gen>][-h<hole>|-rest|-asm][-a<try>] (p2m_holes.py names them so); a run of trials 1 to 4 is t<N>-<leaf>.
const TASK_ID = /^(t[0-9]+)-([A-Za-z0-9]+)(?:-s([0-9]+))?(?:-(?:h([0-9]+)|(rest|asm)))?(?:-a([0-9]+))?$/;
const KIND: Record<string, string> = { rest: 'rest', asm: 'assembly' };
export function taskRef(id: string) {
	const m = TASK_ID.exec(id);
	if (!m) return null;
	return { trial: m[1], leaf: m[2], gen: Number(m[3] ?? 1), hole: m[4] ? Number(m[4]) : null, kind: m[4] ? 'hole' : m[5] ? KIND[m[5]] : 'outline', attempt: Number(m[6] ?? 1) };
}

/** A task as a relation names it: "outline 2", "hole 3", "assembly", with " try N" after a first try. */
export function describeTask(id: string, byId: Map<string, TaskTableRow>): string {
	const r = byId.get(id);
	const kind = r?.kind ?? taskRef(id)?.kind ?? 'task';
	const outline = r?.labels.outline ?? String(taskRef(id)?.gen ?? '');
	const hole = r?.labels.hole ?? String(taskRef(id)?.hole ?? '');
	const tr = r?.try ?? taskRef(id)?.attempt ?? 1;
	const base = kind === 'outline' ? `outline ${outline}` : kind === 'hole' ? `hole ${hole}` : kind === 'rest' ? 'in-place task' : kind === 'run' ? id : kind;
	return tr > 1 ? `${base} try ${tr}` : base;
}

/** prove2me's options: the leaf (its p2m- id, the trials' id on hover), outline and hole columns; filters by kind,
 * state, outcome and trial (the values the build's rows have, the queue's states, and the trials up to the current). */
export function p2mTaskOptions(rows: TaskTableRow[], currentTrial: number | null): TaskTableOptions {
	const trials = [...new Set([...rows.map((r) => Number(r.labels.trial)), ...(currentTrial ? [currentTrial] : [])])].sort((a, b) => a - b);
	const outcomes = [...new Set(rows.map((r) => r.outcome).filter((o): o is string => !!o))];
	return {
		name: 'p2m',
		labelCols: [
			{
				key: 'leaf',
				title: 'Leaf',
				html: (r) =>
					r.labels.leaf
						? `<span class="tt-leaf" title="${esc(`${r.labels.alias} in trial ${r.labels.trial}`)}">${esc(r.labels.leaf)}</span><span class="tt-sub" data-alias>${esc(r.labels.alias)}</span>`
						: `<span class="tt-leaf">${esc(r.labels.alias)}</span>`,
			},
			{ key: 'outline', title: 'Outline' },
			{ key: 'hole', title: 'Hole' },
		],
		filters: [
			{ key: 'kind', all: 'All kinds', values: taskKinds.filter(([k]) => rows.some((r) => r.kind === k)) },
			{ key: 'state', all: 'Any state', values: [['queued', 'Queued'], ['running', 'Running'], ['done', 'Done'], ['failed', 'Failed'], ['cancelled', 'Cancelled']] },
			{ key: 'outcome', all: 'Any outcome', values: outcomes.map((o) => [o, taskOutcomeLabel(o)]) },
			{ key: 'label:trial', all: 'All trials', values: trials.map((t) => [String(t), `Trial ${t}`]) },
		],
		kindLabel: Object.fromEntries(taskKinds),
		outcomeLabel: (o) => taskOutcomeLabel(o),
		describe: describeTask,
	};
}
