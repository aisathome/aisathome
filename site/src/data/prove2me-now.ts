// "Working on now" on /science/prove2me/: what runs or waits now, a block each (owner, 2026-11-23: "Working on now can
// be the trial or the bench"): every replay benchmark with open tasks (prove2me-status.ts isBench; no campaign, its
// tasks name it), then every trial that has not finished (prove2me-records.ts nowDocs). A benchmark's block says it is a
// replay on its scaffold and not a trial; a trial whose tasks wait behind a benchmark says so.
//
// One HTML renderer for the build (from the records the build fetched) and the browser (live, every minute), so the
// two never say different things. Every value is escaped; a campaign's text is plain text, the data document's
// Markdown-light (prove2me-current.ts mdLightHTML).
//
// SPDX-License-Identifier: Apache-2.0
import { day, mdLightHTML, submissionText, theoremURL, trialQueue, type CurrentDoc, type TasksView } from './prove2me-current';
import { leafTokensText, liveLeaves, taskTableHTML, type LiveLeaf } from './prove2me-live';
import { leafPlan, leafPlanHTML } from './prove2me-outlines';
import { nowDocs, type Records } from './prove2me-records';
import { isBench, type StatusView } from './prove2me-status';

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);
const plainHTML = (s: string) =>
	s
		.trim()
		.split(/\n\s*\n/)
		.filter((p) => p.trim())
		.map((p) => `<p>${esc(p.trim().replace(/\s*\n\s*/g, ' '))}</p>`)
		.join('');
const roleLabel: Record<string, string> = { calibration: 'Calibration', open: 'Open', comparison: 'Comparison' };

/** The benchmark documents: one per benchmark with tasks running or waiting, its leaves in the order its tasks were
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

/** Every block of "Working on now": the open benchmarks, then the trials (nowDocs). */
export const nowAll = (doc: CurrentDoc | null, live: Records | null) => [...(live ? benchDocs(live) : []), ...nowDocs(doc, live)];

export const isBenchDoc = (d: CurrentDoc) => d.schema === 'bench';

/** A block's head line. */
export function nowHead(d: CurrentDoc): string {
	const t = d.trial;
	if (isBenchDoc(d)) return `Benchmark ${t.id}${t.start ? `, since ${day(t.start)}` : ''} · a replay on scaffold ${t.scaffold || '?'}, not a trial`;
	return t.status === 'preparing'
		? `Trial ${t.number} is being prepared · scaffold ${t.scaffold}`
		: t.status === 'finished'
			? `Trial ${t.number} has ended · scaffold ${t.scaffold}`
			: `Trial ${t.number}${t.start ? `, since ${day(t.start)}` : ''} · scaffold ${t.scaffold}`;
}

/** A benchmark's own text: what it is and where it stands in the queue. */
export function benchText(d: CurrentDoc, status: StatusView | null): string {
	const b = status?.benches.find((x) => x.label === d.trial.id);
	const ahead = (status?.trials ?? []).filter((t) => b?.ahead.includes(t.label)).map((t) => (t.probe ? `the outline probes for trial ${t.number}` : `trial ${t.number}`));
	return (
		`It runs tasks recorded in earlier trials again under scaffold ${d.trial.scaffold || '?'}, so that scaffold versions can be compared on the same tasks. ` +
		`It is not a trial: it submits nothing, and its runs count in neither the trials nor the leaves on this page.` +
		(ahead.length ? ` Its tasks run before ${andList(ahead)}’s in the queue.` : '')
	);
}

export interface NowCtx {
	/** The build's theorem pages by theorem (a leaf the document lists without one). */
	theorems?: Record<string, string>;
	/** The trials before `n` that ran a leaf (every trial for a benchmark: `n` is Infinity). */
	earlier?: (leaf: string, n: number) => number[];
	/** The build's reuses: theorem -> the proved theorem its one-line proof imports. */
	reuses?: Record<string, string>;
	/** The queue now: which benchmark runs ahead of which trial. */
	status?: StatusView | null;
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
	if (earlier.length) meta.push(`<span>${bench ? 'tried in' : 'tried before in'} trial${earlier.length > 1 ? 's' : ''} ${esc(andList(earlier.map(String)))}</span>`);
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
	const bench = isBenchDoc(doc);
	const live = liveLeaves(trialQueue(tasks, doc), t.id, doc.leaves.map((l) => l.alias));
	const items = [
		...doc.leaves.map((l) => leafHTML(doc, l, live.find((x) => x.leaf === l.alias), ctx, taken)),
		...live.filter((x) => !doc.leaves.some((l) => l.alias === x.leaf)).map((x) => leafHTML(doc, undefined, x, ctx, taken)),
	];
	const behind = ctx.status?.trials.find((x) => x.label === t.id)?.behind ?? [];
	const about = bench ? plainHTML(benchText(doc, ctx.status ?? null)) : doc.schema === 'campaign' ? plainHTML(t.description) : mdLightHTML(t.description);
	// the trial's submissions of leaves it does not list (a leaf closed by reuse while the trial was prepared)
	const others = doc.submissions.filter((s) => !doc.leaves.some((l) => l.id === s.leaf));
	const subs = others.map((s) => `${s.leaf} (${s.alias}), ${submissionText(s)} on ${day(s.verdict_at ?? s.submitted_at, false)}`).join('; ');
	return (
		`<div class="now-trial" data-now-trial="${esc(t.id)}" data-status="${esc(t.status)}"${bench ? ' data-bench' : ''}>` +
		`<p class="head" data-now-head>${esc(nowHead(doc))}</p>` +
		(behind.length
			? `<p class="meta" data-now-behind>${esc(`Waiting: its tasks wait behind ${behind.length > 1 ? 'benchmarks' : 'benchmark'} ${andList(behind)}, which the queue runs first.`)}</p>`
			: '') +
		`<div class="about" data-now-about${about ? '' : ' hidden'}>${about}</div>` +
		`<p class="caps" data-now-budget${t.budget ? '' : ' hidden'}>${t.budget ? esc(`Budget: ${t.budget}.`) : ''}</p>` +
		`<ul data-now-leaves>${items.join('')}</ul>` +
		`<p class="meta" data-now-subs${others.length ? '' : ' hidden'}>${others.length ? esc(`Also from this trial: ${subs}.`) : ''}</p>` +
		`</div>`
	);
}

/** Every block in `docs`' order. A leaf's #leaf-<id> and #leaf-<alias> anchors go to its first row in a trial's block
 * (an account page's link names a task's leaf), a benchmark's rows of the same leaf get none. */
export function nowBlocksHTML(docs: CurrentDoc[], tasks: TasksView, ctx: NowCtx = {}): string {
	const taken = new Set<string>();
	const html = new Map<CurrentDoc, string>();
	for (const d of [...docs.filter((d) => !isBenchDoc(d)), ...docs.filter(isBenchDoc)]) html.set(d, blockHTML(d, tasks, ctx, taken));
	return docs.map((d) => html.get(d)!).join('');
}
