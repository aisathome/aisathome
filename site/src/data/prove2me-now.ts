// "Working on now" on /science/prove2me/: what runs or waits now, a block each (since
// 2026-11-23, a trial or a bench): every trial that has not finished (prove2me-records.ts nowDocs), then every bench with
// open tasks (prove2me-status.ts isBench; no campaign, its tasks name it), shown as a trial is.
//
// A block names the trial or bench, its start and scaffold, then its leaves and their tasks; no description of the
// trial or its budget (2026-11-23: state what is being worked on). One HTML renderer for the build (from the
// records the build fetched) and the browser (live, every minute), so the two never say different things. Every value
// is escaped.
//
// SPDX-License-Identifier: Apache-2.0
import { day, submissionText, theoremURL, trialQueue, type CurrentDoc, type TasksView } from './prove2me-current';
import { leafTokensText, liveLeaves, taskTableHTML, type LiveLeaf } from './prove2me-live';
import { leafPlan, leafPlanHTML } from './prove2me-outlines';
import { nowDocs, type Records } from './prove2me-records';
import { benchName, isBench } from './prove2me-status';

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);
const roleLabel: Record<string, string> = { calibration: 'Calibration', open: 'Open', comparison: 'Comparison' };

/** The bench documents: one per bench with tasks running or waiting, its leaves in the order its tasks were
 * made, each leaf's theorem and page from the campaign that listed it. */
export function benchDocs(live: Records): CurrentDoc[] {
	const byBench = new Map<string, Records['tasks']['tasks']>();
	for (const t of [...live.tasks.tasks].sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id)))
		if (isBench(t.labels.trial)) byBench.set(t.labels.trial, [...(byBench.get(t.labels.trial) ?? []), t]);
	const items = new Map(live.campaigns.flatMap((c) => c.items.map((i) => [i.id, i] as const)));
	return [...byBench]
		.filter(([, ts]) => ts.some((t) => t.state === 'queued' || t.state === 'leased'))
		.sort(([a], [b]) => a.localeCompare(b))
		.map(([id, ts]) => {
			const starts = ts.map((t) => t.started_at).filter((s): s is string => !!s).sort();
			const leaves = [...new Map(ts.filter((t) => t.labels.leaf).map((t) => [t.labels.leaf, t])).values()].map((t) => {
				const i = items.get(t.labels.leaf);
				return {
					id: t.labels.leaf, alias: t.labels.alias || t.labels.leaf, role: (t.labels.group === 'calibration' ? 'calibration' : 'open') as 'open',
					theorem: i?.labels.theorem ?? '', url: i?.url ?? '',
				};
			});
			return {
				schema: 'bench',
				project: 'prove2me',
				updated_at: live.updated,
				trial: { id, number: 0, scaffold: ts.find((t) => t.labels.scaffold)?.labels.scaffold ?? '', start: starts[0]?.slice(0, 10) ?? null, status: 'running', description: '', budget: '' },
				leaves,
				submissions: [],
			} as CurrentDoc;
		});
}

/** Every block of "Working on now": the trials (nowDocs), then the open benches. */
export const nowAll = (doc: CurrentDoc | null, live: Records | null) => [...nowDocs(doc, live), ...(live ? benchDocs(live) : [])];

export const isBenchDoc = (d: CurrentDoc) => d.schema === 'bench';

/** A block's head line. */
export function nowHead(d: CurrentDoc): string {
	const t = d.trial;
	if (isBenchDoc(d)) return `${benchName(t.id)}${t.start ? `, since ${day(t.start)}` : ''} · scaffold ${t.scaffold}`;
	return t.status === 'preparing'
		? `Trial ${t.number} is being prepared · scaffold ${t.scaffold}`
		: t.status === 'finished'
			? `Trial ${t.number} has ended · scaffold ${t.scaffold}`
			: `Trial ${t.number}${t.start ? `, since ${day(t.start)}` : ''} · scaffold ${t.scaffold}`;
}

export interface NowCtx {
	/** The build's theorem pages by theorem (a leaf the document lists without one). */
	theorems?: Record<string, string>;
	/** The trials before `n` that ran a leaf (every trial for a bench: `n` is Infinity). */
	earlier?: (leaf: string, n: number) => number[];
	/** The build's reuses: theorem -> the proved theorem its one-line proof imports. */
	reuses?: Record<string, string>;
}

function leafHTML(doc: CurrentDoc, l: CurrentDoc['leaves'][number] | undefined, live: LiveLeaf | undefined, ctx: NowCtx, taken: Set<string>): string {
	const bench = isBenchDoc(doc);
	const alias = l?.alias ?? live!.leaf;
	const id = (x: string) => (taken.has(x) ? '' : (taken.add(x), ` id="${esc(x)}"`));
	const state = live?.state ?? 'planned';
	const leafID = l?.id ?? alias;
	const theorem = l?.theorem || live?.theorem || '';
	const url = l?.url || ctx.theorems?.[theorem] || '';
	const role = l?.role ?? (live?.group === 'calibration' ? 'calibration' : 'open');
	const earlier = l?.id ? (ctx.earlier?.(l.id, bench ? Infinity : doc.trial.number) ?? []) : [];
	const meta = [`<span>${esc(roleLabel[role] ?? 'Open')} leaf</span>`];
	if (earlier.length) meta.push(`<span>tried before in trial${earlier.length > 1 ? 's' : ''} ${esc(andList(earlier.map(String)))}</span>`);
	const d = live?.detail;
	if (d && (d.total > 0 || d.computed > 0))
		meta.push(`<span data-tokens="${d.total}" data-computed="${d.computed}" data-exact="${d.exact}">${esc(leafTokensText(d, state === 'running' || state === 'queued'))}</span>`);
	for (const s of doc.submissions.filter((s) => s.leaf === l?.id))
		meta.push(`<span data-submission="${esc(s.verdict)}">${esc(`${submissionText(s)} (${day(s.verdict_at ?? s.submitted_at, false)})`)}</span>`);
	// closed by reuse: the theorem the accepted proof imports (the document's, else the build's)
	const reuseSub = doc.submissions.find((s) => s.leaf === l?.id && s.kind === 'reuse');
	const reuse = reuseSub ? reuseSub.reuses?.theorem ?? ctx.reuses?.[theorem] ?? 'a theorem already proved on prove2me' : undefined;
	const plan = live ? leafPlan(live.detail, l?.outlines ?? [], reuse) : null;
	const words = state === 'planned' || !plan ? 'Not started yet' : plan.summary;
	return (
		`<li${id(`leaf-${leafID}`)} data-current-leaf="${esc(alias)}"${l ? ` data-leaf-id="${esc(l.id)}"` : ''} data-state="${esc(state)}">` +
		`<span class="leaf">${esc(leafID)}</span>` +
		(l ? `<span class="alias"${id(`leaf-${alias}`)} title="The id the trials used" data-alias>${esc(alias)}</span>` : '') +
		(theorem ? ` ${theoremURL.test(url) ? `<a class="thm-name" href="${esc(url)}">${esc(theorem)}</a>` : `<span class="thm-name">${esc(theorem)}</span>`}` : '') +
		`<span class="meta">${meta.join('')}</span>` +
		`<span class="state state-${esc(state)}" data-progress="${esc(plan?.summary ?? '')}" data-leaf-summary>${esc(words)}</span>` +
		(live && plan ? leafPlanHTML(alias, plan) + taskTableHTML(alias, live.detail, 'Every task') : '') +
		`</li>`
	);
}

function blockHTML(doc: CurrentDoc, tasks: TasksView, ctx: NowCtx, taken: Set<string>): string {
	const t = doc.trial;
	const live = liveLeaves(trialQueue(tasks, doc), t.id, doc.leaves.map((l) => l.alias));
	const items = [
		...doc.leaves.map((l) => leafHTML(doc, l, live.find((x) => x.leaf === l.alias), ctx, taken)),
		...live.filter((x) => !doc.leaves.some((l) => l.alias === x.leaf)).map((x) => leafHTML(doc, undefined, x, ctx, taken)),
	];
	// the trial's submissions of leaves it does not list (a leaf closed by reuse while the trial was prepared)
	const others = doc.submissions.filter((s) => !doc.leaves.some((l) => l.id === s.leaf));
	const subs = others.map((s) => `${s.leaf} (${s.alias}), ${submissionText(s)} on ${day(s.verdict_at ?? s.submitted_at, false)}`).join('; ');
	return (
		`<div class="now-trial" data-now-trial="${esc(t.id)}" data-status="${esc(t.status)}">` +
		`<p class="head" data-now-head>${esc(nowHead(doc))}</p>` +
		`<ul data-now-leaves>${items.join('')}</ul>` +
		`<p class="meta" data-now-subs${others.length ? '' : ' hidden'}>${others.length ? esc(`Also from this trial: ${subs}.`) : ''}</p>` +
		`</div>`
	);
}

/** Every block in `docs`' order. A leaf's #leaf-<id> and #leaf-<alias> anchors go to its first row (the oldest trial's;
 * the benches come last). */
export function nowBlocksHTML(docs: CurrentDoc[], tasks: TasksView, ctx: NowCtx = {}): string {
	const taken = new Set<string>();
	return docs.map((d) => blockHTML(d, tasks, ctx, taken)).join('');
}
