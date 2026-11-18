// "Working on now" without a deploy (owner, 2026-11-15; decision 0019: project content stays out of the gateway): the
// page reads two public documents every minute and builds the list from them.
//
//  - prove2me's own data document, /data/prove2me/current.json (schema "prove2me-current/2", "/1" still read), which
//    the project's tooling publishes on the website host when a trial starts, a submission gets its verdict, or the
//    trial's text changes: the trial (number, scaffold, start, status, its description in Markdown-light, the budget),
//    its leaves (leaf id, alias, role, theorem and its prove2me page; from /2 each outline's holes: in place or a lemma,
//    and what closed one without a task or a verdict reached outside one) and its submissions (from /2 with the theorem
//    a reuse imports);
//  - the gateway's generic task list, GET /v1/tasks?project=prove2me (gateway tasks_public.go), every page of it:
//    every task's state, outcome, try, relations, labels (trial, leaf id, alias, outline, hole) and counts, and for a
//    running task the gateway's own live counters when it has them (`live`: requests, prompt_tokens with the cache
//    reads in them, cached_tokens, completion_tokens, model_s; gateway tasks_live.go).
//
// The task list stands on its own (decision 0021, 2026-11-17): the gateway is the record of every trial from trial 6 on,
// so the table of every task and the trials table take every such trial from it, not only the document's (trial 7's
// tasks vanished from the page when trial 8 started). Trials 1 to 5 stay the build's frozen history.
//
// When either is missing or not what it should be, the page keeps the copy the build rendered. Pure functions, no
// data, so the browser's bundle stays small.
//
// SPDX-License-Identifier: Apache-2.0
import type { QueueTask, QueueView } from './prove2me-live';
import type { TaskTableRow } from './task-table';
import type { HoleInfo, OutlineHoles } from './prove2me-outlines';

export const CURRENT_URL = '/data/prove2me/current.json';
export const TASKS_URL = '/v1/tasks?project=prove2me&limit=500';
/** At most this many pages of GET /v1/tasks (500 tasks each) in one load. */
export const TASK_PAGES = 20;
/** Trials 1 to 5 are frozen history (decision 0021): the build's rows of them stand, though the gateway still has tasks
 * of trials 4 and 5 (their counts there lack the cache reads). From trial 6 on, the gateway is the record. */
export const FROZEN_THROUGH = 5;
/** A trial's label on a task: t<N>, or t<N>p for the outline probes that chose trial N's leaves. */
export const TRIAL_LABEL = /^t([0-9]+)(p?)$/;
/** A trial label the page takes from the gateway: a trial or probe round after the frozen ones (not a test's label). */
export function isLiveTrial(label: string | undefined): boolean {
	const m = TRIAL_LABEL.exec(label ?? '');
	return !!m && Number(m[1]) > FROZEN_THROUGH;
}
export const CURRENT_SCHEMA = 'prove2me-current/2';
/** The schemas the page reads: /1 has no per-outline holes and no reuse details. */
export const CURRENT_SCHEMAS = ['prove2me-current/1', CURRENT_SCHEMA];

export interface CurrentDoc {
	schema: string;
	project: 'prove2me';
	updated_at: string;
	trial: { id: string; number: number; scaffold: string; start: string | null; status: 'preparing' | 'running' | 'finished'; description: string; budget: string };
	leaves: { id: string; alias: string; role: 'calibration' | 'open' | 'comparison'; theorem: string; url: string; outlines?: OutlineHoles[] }[];
	submissions: {
		leaf: string;
		alias: string;
		kind: 'proof' | 'reuse';
		theorem: string;
		url: string;
		trial: number;
		submitted_at: string;
		verdict: string;
		verdict_at: string | null;
		/** A reuse: the proved prove2me theorem the one-line proof uses. */
		reuses?: { theorem: string; url: string; relation?: 'same' | 'part' };
	}[];
}

/** The gateway's live counters of a running task (gateway tasks_live.go): the model calls it relayed for the task so
 * far. Unlike a result's, its prompt_tokens include the cache reads (as a node reports them); cached_tokens are the
 * part read from the cache. Null or absent: the task's reported numbers only. */
export interface LiveCounters {
	requests?: number;
	prompt_tokens?: number;
	cached_tokens?: number;
	completion_tokens?: number;
	model_s?: number;
	updated_at?: string;
}
const liveCounts = ['requests', 'prompt_tokens', 'cached_tokens', 'completion_tokens', 'model_s'] as const;

/** One task of GET /v1/tasks (the fields the page uses). */
export interface PublicTask {
	project: string;
	id: string;
	kind: string;
	role: string | null;
	state: 'queued' | 'leased' | 'done' | 'failed' | 'cancelled';
	outcome: string | null;
	try: number | null;
	parent: string | null;
	needs: string[];
	retry_of: string | null;
	replaces: string | null;
	closed_by?: string | null;
	labels: Record<string, string>;
	steps: number | null;
	model_min: number | null;
	prompt_tokens: number | null;
	cached_tokens: number | null;
	completion_tokens: number | null;
	/** The explicit counts (gateway, 2026-11-18): null when the worker did not report its cache reads (trials 4 and 5),
	 * absent from an older gateway. tokens_computed = uncached_prompt_tokens + completion_tokens; tokens_with_cache adds
	 * cached_tokens. */
	uncached_prompt_tokens?: number | null;
	tokens_computed?: number | null;
	tokens_with_cache?: number | null;
	created_at: string;
	started_at: string | null;
	finished_at: string | null;
	updated_at: string;
	live?: LiveCounters | null;
}

export interface TasksView {
	updated: string;
	tasks: PublicTask[];
}

const isStr = (v: unknown): v is string => typeof v === 'string';
const isTime = (v: unknown) => isStr(v) && !Number.isNaN(Date.parse(v));
const count = (v: unknown) => typeof v === 'number' && Number.isFinite(v) && v >= 0;
const opt = (v: unknown) => v === null || v === undefined || count(v);
const optStr = (v: unknown) => v === null || v === undefined || isStr(v);
export const theoremURL = /^https:\/\/prove2\.me\/theorems\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const leafID = /^p2m-[0-9a-f]{6}([0-9a-f]{2})?$/;
const holeBy = ['prepass', 'index', 'ledger', 'counterexample', 'model', 'check'];
const posInt = (v: unknown): v is number => typeof v === 'number' && Number.isInteger(v) && v > 0 && v < 1000;
const isHole = (h: HoleInfo) =>
	h && posInt(h.hole) && ['lemma', 'inplace'].includes(h.kind) && ['open', 'proved', 'false'].includes(h.state) &&
	(h.by === undefined || holeBy.includes(h.by)) && (h.note === undefined || (isStr(h.note) && h.note.length <= 300));
/** A leaf's outlines as the document states them; an outline with a hole that is not what it should be is left out. */
const outlinesOf = (v: unknown): OutlineHoles[] | undefined =>
	Array.isArray(v) ? v.filter((o) => o && posInt(o.outline) && Array.isArray(o.holes) && o.holes.every(isHole)) : undefined;

/** The data document, or null when it is not one (the page then keeps the build's copy). */
export function parseCurrent(d: unknown): CurrentDoc | null {
	const c = d as CurrentDoc;
	if (!c || !CURRENT_SCHEMAS.includes(c.schema) || c.project !== 'prove2me' || !isTime(c.updated_at)) return null;
	const t = c.trial;
	if (!t || !/^t[0-9]+$/.test(t.id ?? '') || t.number !== Number(t.id.slice(1)) || !isStr(t.scaffold) || !optStr(t.start)) return null;
	if (!['preparing', 'running', 'finished'].includes(t.status) || !isStr(t.description) || !isStr(t.budget)) return null;
	if (!Array.isArray(c.leaves) || !Array.isArray(c.submissions)) return null;
	const leaves = c.leaves
		.filter((l) => l && leafID.test(l.id) && isStr(l.alias) && isStr(l.theorem) && ['calibration', 'open', 'comparison'].includes(l.role))
		.map(({ outlines, ...l }) => {
			const os = outlinesOf(outlines);
			return os ? { ...l, outlines: os } : l;
		});
	const subs = c.submissions
		.filter((s) => s && leafID.test(s.leaf) && isStr(s.alias) && ['proof', 'reuse'].includes(s.kind) && isTime(s.submitted_at) && isStr(s.verdict) && (s.verdict_at === null || isTime(s.verdict_at)))
		.map(({ reuses, ...s }) => (reuses && isStr(reuses.theorem) && theoremURL.test(reuses.url ?? '') ? { ...s, reuses } : s));
	return { ...c, leaves, submissions: subs };
}

const states = ['queued', 'leased', 'done', 'failed', 'cancelled'];

/** GET /v1/tasks, or null when it is not what the gateway sends. */
export function parseTasks(d: unknown): TasksView | null {
	const v = d as TasksView;
	if (!v || !isTime(v.updated) || !Array.isArray(v.tasks)) return null;
	const tasks = v.tasks.filter(
		(t) =>
			t && isStr(t.id) && states.includes(t.state) && isTime(t.created_at) && isTime(t.updated_at) && t.labels && typeof t.labels === 'object' &&
			Object.values(t.labels).every(isStr) && [t.role, t.outcome, t.parent, t.retry_of, t.replaces, t.closed_by, t.started_at, t.finished_at].every(optStr) &&
			[t.try, t.steps, t.model_min, t.prompt_tokens, t.cached_tokens, t.completion_tokens, t.uncached_prompt_tokens, t.tokens_computed, t.tokens_with_cache].every(opt) &&
			(t.live === undefined || t.live === null || (typeof t.live === 'object' && liveCounts.every((k) => opt(t.live![k])))),
	);
	return { updated: v.updated, tasks };
}

/** Every item of a paged list of the gateway (GET /v1/tasks, campaigns, results): page after page by its cursor
 * (`next`, sent back as `after`), at most `maxPages`. `get` fetches a URL's JSON (with its own timeout) and throws when
 * it fails; `parse` reads one page's items, or null when it is not what the gateway sends. Null when a page fails, so
 * the page keeps what it shows; a cursor seen before ends the walk, and an id seen before counts once. */
export async function walkPages<T extends { id: string }, P>(
	get: (url: string) => Promise<unknown>,
	url: string,
	parse: (d: unknown) => (P & { items: T[] }) | null,
	maxPages = TASK_PAGES,
): Promise<{ first: P; items: T[] } | null> {
	let first: P | null = null;
	const items: T[] = [];
	const ids = new Set<string>();
	const cursors = new Set<string>();
	let after: string | null = null;
	for (let i = 0; i < maxPages; i++) {
		let d: unknown;
		try {
			d = await get(after === null ? url : `${url}${url.includes('?') ? '&' : '?'}after=${encodeURIComponent(after)}`);
		} catch {
			return null;
		}
		const page = parse(d);
		if (!page) return null;
		first ??= page;
		for (const t of page.items) {
			if (ids.has(t.id)) continue;
			ids.add(t.id);
			items.push(t);
		}
		const next = (d as { next?: unknown }).next;
		if (typeof next !== 'string' || !next || cursors.has(next)) break;
		cursors.add(next);
		after = next;
	}
	return first ? { first, items } : null;
}

/** Every task of GET /v1/tasks, page after page (walkPages). */
export async function fetchAllTasks(get: (url: string) => Promise<unknown>, url = TASKS_URL, maxPages = TASK_PAGES): Promise<TasksView | null> {
	const r = await walkPages(get, url, (d) => {
		const v = parseTasks(d);
		return v ? { ...v, items: v.tasks } : null;
	}, maxPages);
	return r ? { updated: r.first.updated, tasks: r.items } : null;
}

const und = <T>(v: T | null | undefined): T | undefined => (v === null ? undefined : v);
const int = (v: string | undefined) => (v && /^[0-9]+$/.test(v) ? Number(v) : undefined);
// The roles as the page's leaf view names them (the v5 export's names; a task made before the typed fields says so too).
const internalRole: Record<string, string> = { outline: 'skeleton', assembly: 'asm' };

/** A task's counts: a running task's from the gateway's live counters when it has them, else the numbers its worker
 * reported. Tokens as in a task's result: prompt_tokens computed fresh (the live count less its cache reads; the
 * explicit uncached_prompt_tokens when the gateway sends it), cached_tokens read from the cache, completion_tokens
 * written. */
export function counts(t: PublicTask) {
	const lv = t.state === 'leased' && t.live ? t.live : null;
	if (!lv) {
		const fresh = t.uncached_prompt_tokens ?? t.prompt_tokens;
		return { steps: und(t.steps), model_min: und(t.model_min), prompt_tokens: und(fresh), cached_tokens: und(t.cached_tokens), completion_tokens: und(t.completion_tokens) };
	}
	return {
		steps: lv.requests,
		model_min: lv.model_s === undefined ? undefined : Math.round(lv.model_s / 6) / 10,
		prompt_tokens: lv.prompt_tokens === undefined ? undefined : Math.max(lv.prompt_tokens - (lv.cached_tokens ?? 0), 0),
		cached_tokens: lv.cached_tokens,
		completion_tokens: lv.completion_tokens,
	};
}

/** The document's leaf of a task: by its leaf id label, else by its alias (a task made before v7 names the alias "leaf"). */
const docLeaf = (doc: CurrentDoc, lab: Record<string, string>) => doc.leaves.find((l) => l.id === lab.leaf || l.alias === (lab.alias ?? lab.leaf));

/** The document's leaf of a task of the document's own trial (another trial's alias may name another leaf). */
const ownLeaf = (doc: CurrentDoc | null, lab: Record<string, string>) => (doc && lab.trial === doc.trial.id ? docLeaf(doc, lab) : undefined);

/** A task of GET /v1/tasks in the shape the leaf view takes (prove2me-live.ts liveLeaves). */
export function asQueueTask(t: PublicTask, doc: CurrentDoc): QueueTask {
	const lab = t.labels;
	const leaf = ownLeaf(doc, lab);
	return {
		id: t.id,
		state: t.state,
		created_at: t.created_at,
		updated_at: t.updated_at,
		since: und(t.started_at),
		trial: lab.trial,
		leaf: lab.alias ?? leaf?.alias ?? lab.leaf,
		group: leaf?.role ?? lab.group,
		theorem: leaf?.theorem ?? lab.theorem,
		role: t.role ? internalRole[t.role] ?? t.role : undefined,
		gen: int(lab.outline),
		hole: int(lab.hole),
		attempt: und(t.try),
		outcome: und(t.outcome),
		closed_by: und(t.closed_by),
		...counts(t),
	};
}

/** The current trial's tasks as the leaf view's queue. */
export function trialQueue(v: TasksView, doc: CurrentDoc): QueueView {
	return { updated: v.updated, tasks: v.tasks.filter((t) => t.labels.trial === doc.trial.id).map((t) => asQueueTask(t, doc)) };
}

const kindOf: Record<string, string> = { outline: 'outline', skeleton: 'outline', hole: 'hole', rest: 'rest', assembly: 'assembly', asm: 'assembly' };

/** An outline probe round's trial label: t<N>p (trial N's leaves chosen by one outline task each). */
export const PROBE_TRIAL = /^t([0-9]+)p$/;

/** The tasks of every trial and outline probe round after the frozen ones (isLiveTrial) as rows of the task table, in
 * every state: a live row replaces the build's of the same id (task-table.ts mergeRows). The document, when there is
 * one, adds its own trial's leaves and outlines. `leafIds`: theorem -> leaf id (a task made before v7 names its leaf by
 * alias); `holes`: a written outline's hole count by task id, from the build (the document's own outlines win). */
export function taskTableRows(v: TasksView, doc: CurrentDoc | null, leafIds: Record<string, string> = {}, holes: Record<string, string> = {}): TaskTableRow[] {
	return v.tasks
		.filter((t) => isLiveTrial(t.labels.trial))
		.sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id)) // as they were made
		.map((t) => {
			const c = counts(t);
			const lab = t.labels;
			const leaf = ownLeaf(doc, lab);
			// tokens computed (what the GPU did); with the cache re-reads only for the hover
			const known = c.cached_tokens !== undefined;
			const tokens = known ? (c.prompt_tokens ?? 0) + (c.completion_tokens ?? 0) : null;
			const withCache = known ? (c.prompt_tokens ?? 0) + c.cached_tokens! + (c.completion_tokens ?? 0) : null;
			const kind = kindOf[t.role ?? ''] ?? t.role ?? 'task';
			const gen = int(lab.outline) ?? 1;
			const docHoles = leaf?.outlines?.find((o) => o.outline === gen)?.holes.length;
			const n = kind === 'outline' && t.state === 'done' && t.outcome === 'outline' ? (docHoles ? String(docHoles) : holes[t.id]) : undefined;
			return {
				project: 'prove2me',
				id: t.id,
				kind,
				state: t.state === 'leased' ? 'running' : t.state,
				outcome: t.state === 'done' ? t.outcome : null, // as the export: an outcome only for a task that ended by itself
				try: t.try ?? 1,
				parent: t.parent,
				needs: t.needs ?? [],
				retry_of: t.retry_of,
				replaces: t.replaces,
				closed_by: t.closed_by ?? null,
				steps: c.steps ?? null,
				model_min: c.model_min ?? null,
				tokens,
				tokens_with_cache: withCache,
				started_at: t.started_at,
				finished_at: t.finished_at,
				labels: {
					trial: lab.trial.slice(1),
					leaf: leaf?.id ?? (lab.leaf && leafID.test(lab.leaf) ? lab.leaf : (lab.theorem && leafIds[lab.theorem]) || ''),
					alias: lab.alias ?? leaf?.alias ?? (lab.leaf && !leafID.test(lab.leaf) ? lab.leaf : ''),
					...(lab.outline ? { outline: lab.outline } : {}),
					...(lab.hole ? { hole: lab.hole } : {}),
					...(n ? { holes: n } : {}),
				},
			};
		});
}

/** The document's outlines with a false hole, as the task table's refuted keys ("<trial number>|<leaf id>|<outline>"). */
export function refutedOutlines(doc: CurrentDoc): Set<string> {
	const out = new Set<string>();
	for (const l of doc.leaves) for (const o of l.outlines ?? []) if (o.holes.some((h) => h.state === 'false')) out.add(`${doc.trial.id.slice(1)}|${l.id}|${o.outline}`);
	return out;
}

/** The outline probes choosing the next trial's leaves, while any is queued or running and the document's trial is not
 * running: round t<N+1>p after trial N ended, t<N>p while trial N is prepared. Null otherwise. */
export function probeRound(v: TasksView, doc: CurrentDoc): { trial: string; forTrial: number; tasks: PublicTask[] } | null {
	if (doc.trial.status === 'running') return null;
	const n = doc.trial.status === 'preparing' ? doc.trial.number : doc.trial.number + 1;
	const tasks = v.tasks.filter((t) => t.labels.trial === `t${n}p`);
	if (!tasks.some((t) => t.state === 'queued' || t.state === 'leased')) return null;
	return { trial: `t${n}p`, forTrial: n, tasks: [...tasks].sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id)) };
}

// ---- Markdown-light (the trial's description): paragraphs, `code`, **strong**, *em*, [text](https://...). Everything
// is escaped first, so the result is safe as HTML in the build and in the browser.

const esc = (v: string) => v.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

function inline(t: string): string {
	return t
		.split(/(`[^`\n]+`)/)
		.map((part, i) => {
			if (i % 2) return `<code>${esc(part.slice(1, -1))}</code>`;
			return esc(part)
				.replace(/\[([^\]]+)\]\((https:\/\/[^)\s*]+)\)/g, '<a href="$2">$1</a>')
				.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
				.replace(/\*([^*]+)\*/g, '<em>$1</em>');
		})
		.join('');
}

/** Markdown-light text as HTML paragraphs. */
export function mdLightHTML(s: string): string {
	return s
		.trim()
		.split(/\n\s*\n/)
		.filter((p) => p.trim())
		.map((p) => `<p>${inline(p.trim().replace(/\s*\n\s*/g, ' '))}</p>`)
		.join('');
}

// ---- Words

const months = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
/** "2026-11-12" -> "12 November 2026"; with `year` false, "12 November". */
export function day(iso: string, year = true) {
	const d = new Date(iso.length === 10 ? `${iso}T00:00:00Z` : iso);
	return `${d.getUTCDate()} ${months[d.getUTCMonth()]}${year ? ` ${d.getUTCFullYear()}` : ''}`;
}
/** "2026-11-12T06:51:23Z" -> "06:51 UTC". */
export const hhmm = (iso: string) => `${iso.slice(11, 16)} UTC`;

export const roleLabel: Record<string, string> = { calibration: 'Calibration', open: 'Open', comparison: 'Comparison' };

/** A submission in words: "accepted by prove2me", "closed by reuse, accepted by prove2me", "waiting for prove2me". */
export function submissionText(s: CurrentDoc['submissions'][number]): string {
	const verdict = s.verdict === 'accepted' ? 'accepted by prove2me' : s.verdict === 'pending' ? 'waiting for prove2me’s verdict' : `prove2me: ${s.verdict.replace(/_/g, ' ')}`;
	return s.kind === 'reuse' ? `closed by reuse, ${verdict}` : `submitted, ${verdict}`;
}
