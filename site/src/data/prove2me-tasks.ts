// The prove2me page's task table (2026-11-15): prove2me's options for the generic task table (task-table.ts) and its
// live rows, the queued and running tasks of the gateway's public queue (GET /v1/projects/prove2me/queue). The build's
// rows come from the export (science/prove2me.json "tasks", relations included); a live row's relations are derived
// here from the task ids the same way (site_json.py task_relations): a hole or rest task is from its outline (the
// outline's newest try), a try -aN is a retry of the try before, an outline -sN replaces outline N-1, an assembly
// needs its outline's holes and rest. Outline probes (t<N>p-...) and anything not a trial's task stay out.
//
// SPDX-License-Identifier: Apache-2.0
import type { QueueTask, QueueView } from './prove2me-live';
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

type Ref = { id: string; trial: string; leaf: string; kind: string; gen: number; hole: number | null; attempt: number };

/** The relations of `ids` among `refs` (the trial's tasks so far): the same rules as the export's. */
export function relations(refs: Ref[], id: string): Pick<TaskTableRow, 'parent' | 'needs' | 'retry_of' | 'replaces'> {
	const r = refs.find((x) => x.id === id)!;
	const mine = refs.filter((x) => x.leaf === r.leaf && x.trial === r.trial);
	const newest = (p: (x: Ref) => boolean) => mine.filter(p).sort((a, b) => b.attempt - a.attempt)[0]?.id ?? null;
	const outline = (g: number) => newest((x) => x.kind === 'outline' && x.gen === g);
	const ids = new Set(refs.map((x) => x.id));
	const base = r.id.replace(/-a[0-9]+$/, '');
	const prev = r.attempt === 2 ? base : `${base}-a${r.attempt - 1}`;
	const holes = [...new Set(mine.filter((x) => x.kind === 'hole' && x.gen === r.gen).map((x) => x.hole!))].sort((a, b) => a - b);
	const rest = newest((x) => x.kind === 'rest' && x.gen === r.gen);
	return {
		parent: ['hole', 'rest', 'assembly'].includes(r.kind) ? outline(r.gen) : null,
		retry_of: r.attempt > 1 && ids.has(prev) ? prev : null,
		replaces: r.kind === 'outline' && r.gen > 1 && r.attempt === 1 ? outline(r.gen - 1) : null,
		needs: r.kind === 'assembly' ? [...holes.map((h) => newest((x) => x.kind === 'hole' && x.gen === r.gen && x.hole === h)!), ...(rest ? [rest] : [])] : [],
	};
}

/** The queue's queued and running tasks of a trial (no probe, nothing else) as rows, their relations among the build's
 * rows and each other; `leafIds` maps a theorem to its p2m- id (the build's leaf table). */
export function liveTaskRows(q: QueueView, built: TaskTableRow[], leafIds: Record<string, string>): TaskTableRow[] {
	const open = q.tasks.filter((t) => (t.state === 'queued' || t.state === 'leased') && !/^t[0-9]+p-/.test(t.id) && taskRef(t.id)); // a trial's task ids only
	const ref = (r: TaskTableRow): Ref => ({ id: r.id, trial: `t${r.labels.trial}`, leaf: r.labels.alias, kind: r.kind, gen: Number(r.labels.outline ?? 1), hole: r.labels.hole ? Number(r.labels.hole) : null, attempt: r.try });
	const live = open.map((t: QueueTask): TaskTableRow => {
		const x = taskRef(t.id)!;
		const kind = t.role ? ({ skeleton: 'outline', hole: 'hole', rest: 'rest', asm: 'assembly' } as Record<string, string>)[t.role] ?? x.kind : x.kind;
		const gen = t.gen ?? x.gen, hole = t.hole ?? x.hole;
		const tok = t.cached_tokens !== undefined ? (t.prompt_tokens ?? 0) + t.cached_tokens + (t.completion_tokens ?? 0) : null;
		return {
			project: 'prove2me', id: t.id, kind, state: t.state === 'leased' ? 'running' : 'queued', outcome: null, try: x.attempt,
			parent: null, needs: [], retry_of: null, replaces: null, steps: t.steps ?? null, model_min: t.model_min ?? null, tokens: tok,
			started_at: t.state === 'leased' ? t.since ?? t.updated_at : null, finished_at: null,
			labels: {
				trial: (t.trial ?? x.trial).replace(/^t/, ''), leaf: (t.theorem && leafIds[t.theorem]) || '', alias: t.leaf ?? x.leaf,
				...(kind === 'run' ? {} : { outline: String(gen) }), ...(hole ? { hole: String(hole) } : {}),
			},
		};
	});
	const ids = new Set(live.map((r) => r.id));
	const refs = [...built.filter((r) => !ids.has(r.id)), ...live].filter((r) => r.kind !== 'run').map(ref);
	return live.map((r) => ({ ...r, ...relations(refs, r.id) }));
}
