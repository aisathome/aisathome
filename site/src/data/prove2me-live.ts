// "Working on now" on the prove2me page, live (owner, 2026-11-13: the page lagged behind the trial):
// the gateway's public view of prove2me's task queue (GET /v1/projects/prove2me/queue; gateway
// queue_public.go: task ids, states, outcomes, token counts, the trial, leaf and theorem names only)
// turned into the leaves the page lists. The build renders the committed JSON's list; in the browser
// this replaces it when the queue answers. No data here, so the browser's bundle stays small.
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
	prompt_tokens?: number;
	completion_tokens?: number;
}

export interface QueueView {
	updated: string;
	tasks: QueueTask[];
}

/** A leaf as the live view sees it: the page's CurrentLeaf plus its tokens so far. */
export type LiveLeaf = Omit<CurrentLeaf, 'title' | 'url' | 'earlier'> & { tokens: number; proved: boolean };

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
			[t.gen, t.hole, t.prompt_tokens, t.completion_tokens].every(num),
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

/**
 * The leaves of a trial, as the export script's leaf_view does it: a leaf runs while any of its tasks
 * runs, waits while any is queued, else it is finished (or as its outline ended). Its outline is the
 * newest generation's skeleton task; its hole tasks are that generation's, one per hole (a hole proved
 * by any of its tries counts once). `order` lists leaves first in the build's order.
 */
export function liveLeaves(q: QueueView, trial: string, order: string[] = []): LiveLeaf[] {
	const mine = q.tasks.filter((t) => t.trial === trial && t.leaf);
	const leaves = [...new Set(mine.map((t) => t.leaf!))];
	const pos = (l: string) => (order.includes(l) ? order.indexOf(l) : order.length);
	const first = (l: string) => Math.min(...mine.filter((t) => t.leaf === l).map((t) => Date.parse(t.created_at) || 0));
	leaves.sort((a, b) => pos(a) - pos(b) || first(a) - first(b));
	return leaves.map((leaf) => {
		const ts = mine.filter((t) => t.leaf === leaf);
		const has = (s: string) => ts.some((t) => t.state === s);
		const skels = ts.filter((t) => t.role === 'skeleton' || (!t.role && t.id === `${trial}-${leaf}`));
		const gen = Math.max(1, ...skels.map((t) => t.gen ?? 1));
		const skel = skels.find((t) => (t.gen ?? 1) === gen) ?? skels[0];
		const holes = ts.filter((t) => t.role === 'hole' && (t.gen ?? 1) === gen && t.hole);
		const proved =
			ts.some((t) => t.state === 'done' && t.outcome === 'proved' && (t.role === 'asm' || t.role === 'skeleton' || !t.role)) ||
			false;
		const anyTheorem = ts.find((t) => t.theorem)?.theorem ?? '';
		const view: LiveLeaf = {
			leaf,
			group: (ts.find((t) => t.group)?.group ?? 'open') as LiveLeaf['group'],
			theorem: anyTheorem,
			state: has('leased') ? 'running' : has('queued') ? 'queued' : taskState[skel?.state ?? 'done'] ?? 'finished',
			tokens: ts.reduce((a, t) => a + (t.prompt_tokens ?? 0) + (t.completion_tokens ?? 0), 0),
			proved,
		};
		if (skel) view.outline = taskState[skel.state] ?? 'finished';
		if (holes.length) {
			const by = new Map<number, QueueTask[]>();
			holes.forEach((t) => by.set(t.hole!, [...(by.get(t.hole!) ?? []), t]));
			const tries = [...by.values()];
			view.hole_tasks = {
				n: by.size,
				proved: tries.filter((h) => h.some((t) => t.state === 'done' && t.outcome === 'proved')).length,
				running: holes.filter((t) => t.state === 'leased').length,
				queued: holes.filter((t) => t.state === 'queued').length,
			};
		}
		return view;
	});
}

/** Where a v5 leaf stands: its outline, then its hole tasks. */
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
