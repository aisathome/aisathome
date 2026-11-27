// The proved theorems (/science/prove2me/theorems/): every leaf prove2me accepted a proof of, with what it cost, from
// the same records as the prove2me page (prove2me-records.ts: the build's history for trials 1 to 5, the gateway's
// public campaigns, results and tasks from trial 6 on). Nothing here is typed in: the counts, the denominator and the
// three costs are computed from the records, by the build and again by the browser every minute, with the same
// renderers (every value escaped), so the two never say different things.
//
// A reuse (the gateway's `reuse` record on a result: the accepted proof is a theorem someone else had proved) is
// listed, marked as such, and never counted as a proof by the model or in the costs.
//
// SPDX-License-Identifier: Apache-2.0
import { FROZEN_THROUGH, day, hhmm, theoremURL } from './prove2me-current';
import { compact } from './prove2me-live';
import { allRuns, scienceView, type History, type Records, type RunRec, type SubRec } from './prove2me-records';
import { plainTitle } from './prove2me-view';
import type { TheoremCard } from './og-card';

/** From this trial on, the trials ran only leaves we judged easy or moderate (the page says so beside every count). */
export const SELECTED_FROM = 11;
/** The median per proof counts the proofs whose tasks the gateway holds, from trial 6 on. */
export const MEDIAN_FROM = FROZEN_THROUGH + 1;
export const THEOREMS_PATH = '/science/prove2me/theorems/';
export const theoremPath = (leaf: string) => `${THEOREMS_PATH}${leaf}/`;

/** One accepted submission. */
export interface TheoremRow {
	leaf: string;
	alias: string;
	kind: 'proof' | 'reuse';
	/** The Lean name; `title` prove2me's title (it may be the Lean name again). */
	theorem: string;
	title: string;
	/** The theorem's page on prove2me (its statement). */
	url: string;
	trial: number;
	submitted_at: string;
	verdict_at: string | null;
	/** Model hours of the leaf's own tasks in the trial that proved it; null for a reuse (no run). */
	hours: number | null;
	/** Tokens computed by those tasks; `exact` false when a task's count is missing (a lower bound). */
	tokens: number | null;
	exact: boolean;
	reuses?: SubRec['reuses'];
}
/** GPU-hours over proofs: hours / proofs, null with no proof. */
export interface CostLine {
	hours: number;
	proofs: number;
	perProof: number | null;
	/** The trials it covers. */
	from: number;
	to: number;
}
export interface TheoremsView {
	source: 'build' | 'live';
	updated: string;
	/** Accepted, newest verdict first. */
	theorems: TheoremRow[];
	counts: {
		submitted: number;
		accepted: number;
		rejected: number;
		pending: number;
		error: number;
		/** Accepted proofs by the model; accepted reuses. */
		proofs: number;
		reused: number;
		openTried: number;
		openProved: number;
		/** From SELECTED_FROM on: the open leaves tried, and how many of them prove2me accepted a proof by the model of. */
		selected: { from: number; to: number; tried: number; accepted: number };
	};
	costs: {
		/** The median and 90th percentile of a proof's own model hours (MEDIAN_FROM on); null with no proof. */
		median: { value: number; p90: number; n: number; from: number; to: number } | null;
		/** All-in from SELECTED_FROM on: every model hour of those trials over their accepted proofs. */
		recent: CostLine;
		/** All-in over every trial and outline probe round. */
		overall: CostLine;
	};
	lastTrial: number;
}

const sum = <T>(xs: T[], f: (x: T) => number) => xs.reduce((a, x) => a + f(x), 0);
/** The q-quantile of sorted values (linear between neighbours). */
export function quantile(sorted: number[], q: number): number {
	if (!sorted.length) return NaN;
	const i = (sorted.length - 1) * q;
	const lo = Math.floor(i), hi = Math.ceil(i);
	return sorted[lo] + (sorted[hi] - sorted[lo]) * (i - lo);
}

/** The run that proved a submission's leaf in its trial (the proof's own tasks), if the records have one. */
function runOf(runs: RunRec[], s: SubRec): RunRec | undefined {
	const rs = runs.filter((r) => r.leaf === s.leaf && r.trial === s.trial);
	return rs.find((r) => r.proved) ?? rs[0];
}

/** The page's model: the build's history alone (`live` null) or with the gateway's records. */
export function theoremsView(h: History, live: Records | null): TheoremsView {
	const v = scienceView(h, live);
	const runs = live ? allRuns(h, live) : h.runs;
	const subs = v.submissions;
	const accepted = subs.filter((s) => s.verdict === 'accepted');
	const theorems = accepted
		.map((s): TheoremRow => {
			const run = s.kind === 'proof' ? runOf(runs, s) : undefined;
			return {
				leaf: s.leaf, alias: s.alias, kind: s.kind, theorem: s.theorem, title: s.title, url: theoremURL.test(s.url) ? s.url : '', trial: s.trial,
				submitted_at: s.submitted_at, verdict_at: s.verdict_at, hours: run ? run.model_min / 60 : null, tokens: run ? run.computed : null,
				exact: run?.exact ?? true, ...(s.reuses ? { reuses: s.reuses } : {}),
			};
		})
		.sort((a, b) => (b.verdict_at ?? b.submitted_at).localeCompare(a.verdict_at ?? a.submitted_at) || b.leaf.localeCompare(a.leaf));
	const proofs = theorems.filter((t) => t.kind === 'proof');
	const lastTrial = v.kpis.lastTrial;
	const real = v.trials.filter((t) => !t.probe && t.all.runs > 0);
	const firstTrial = Math.min(...real.map((t) => t.number), lastTrial);

	const own = proofs.filter((t) => t.trial >= MEDIAN_FROM && t.hours !== null).map((t) => t.hours!).sort((a, b) => a - b);
	const ownTrials = proofs.filter((t) => t.trial >= MEDIAN_FROM && t.hours !== null).map((t) => t.trial);
	const line = (from: number): CostLine => {
		const hours = sum(v.trials.filter((t) => t.number >= from), (t) => t.model_min) / 60;
		const n = proofs.filter((t) => t.trial >= from).length;
		return { hours, proofs: n, perProof: n ? hours / n : null, from, to: lastTrial };
	};
	const selectedOpen = new Set(runs.filter((r) => r.trial >= SELECTED_FROM && r.group === 'open').map((r) => r.leaf));
	return {
		source: v.source,
		updated: v.updated,
		theorems,
		counts: {
			submitted: subs.length,
			accepted: accepted.length,
			rejected: subs.filter((s) => s.verdict === 'rejected').length,
			pending: subs.filter((s) => s.verdict === 'pending').length,
			error: subs.filter((s) => s.verdict === 'error').length,
			proofs: proofs.length,
			reused: theorems.length - proofs.length,
			openTried: v.kpis.openTried,
			openProved: v.kpis.openProved,
			selected: { from: SELECTED_FROM, to: lastTrial, tried: selectedOpen.size, accepted: proofs.filter((t) => t.trial >= SELECTED_FROM && selectedOpen.has(t.leaf)).length },
		},
		costs: {
			median: own.length ? { value: quantile(own, 0.5), p90: quantile(own, 0.9), n: own.length, from: Math.min(...ownTrials), to: Math.max(...ownTrials) } : null,
			recent: line(SELECTED_FROM),
			overall: line(firstTrial),
		},
		lastTrial,
	};
}

// ---- the renderers (HTML strings, every value escaped)

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
/** GPU-hours as the page writes them: two decimals under 10, one above. */
export const gpuH = (h: number | null) => (h === null || !Number.isFinite(h) ? '–' : h < 10 ? h.toFixed(2) : h.toFixed(1));
const trials = (a: number, b: number) => (a === b ? `trial ${a}` : `trials ${a} to ${b}`);
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
/** A theorem's name for people: prove2me's title without TeX, else the Lean name. */
export const nameOf = (t: Pick<TheoremRow, 'title' | 'theorem' | 'leaf'>) => plainTitle(t.title || '') || t.theorem || t.leaf;
const relationWord = (r?: string) => (r === 'part' ? 'part of' : r === 'import' ? 'imports' : 'same statement as');

/** " (1 rejected, 1 not judged)": the submissions not accepted, by verdict. */
const notAccepted = (c: TheoremsView['counts']) => {
	const parts = [c.rejected ? `${c.rejected} rejected` : '', c.error ? `${c.error} not judged` : '', c.pending ? `${c.pending} waiting` : ''].filter(Boolean);
	return parts.length ? ` (${parts.join(', ')})` : '';
};

/** The headline numbers (the dl's children); `--kpis` is their number. */
export function theoremKpisHTML(v: TheoremsView): { html: string; n: number } {
	const c = v.counts;
	const cells = [
		`<div data-kpi="proofs"><dt>${c.proofs === 1 ? 'proof' : 'proofs'} by the model accepted by prove2me</dt><dd>${c.proofs}</dd></div>`,
		`<div data-kpi="submitted"><dt>submissions accepted, of ${c.submitted}${notAccepted(c)}</dt><dd>${c.accepted}</dd></div>`,
		`<div data-kpi="open"><dt>open leaves proved, of ${c.openTried} tried</dt><dd>${c.openProved}</dd></div>`,
		c.reused ? `<div data-kpi="reused"><dt>${c.reused === 1 ? 'leaf' : 'leaves'} closed by reuse (not counted as proofs)</dt><dd>${c.reused}</dd></div>` : '',
	].filter(Boolean);
	return { html: cells.join(''), n: cells.length };
}

/** What the counts mean: the denominator and the selection from trial 11 on, in words. */
export function selectionText(v: TheoremsView): string {
	const s = v.counts.selected;
	const tail = s.tried
		? ` In ${trials(s.from, s.to)}, prove2me accepted proofs by the model of ${s.accepted} of the ${plural(s.tried, 'open leaf', 'open leaves')} tried.`
		: '';
	return (
		`From trial ${s.from} on, the trials ran only leaves we judged easy or moderate, so the faster count and the lower cost since then reflect that choice, not only the model.` +
		tail +
		` Every attempt counts, the failed ones too: the prove2me page lists every leaf and every run.`
	);
}

/** The three costs (the dl's children). */
export function costsHTML(v: TheoremsView): string {
	const m = v.costs.median, r = v.costs.recent, o = v.costs.overall;
	const cell = (key: string, value: string, label: string, sub: string) =>
		`<div data-cost="${key}"><dt>${esc(label)}<span class="sub">${esc(sub)}</span></dt><dd>${esc(value)}</dd></div>`;
	return [
		cell(
			'median', m ? gpuH(m.value) : '–', 'GPU-hours per proof, median',
			m ? `Each accepted proof’s own tasks in the trial that proved it; ${plural(m.n, 'proof')}, ${trials(m.from, m.to)}; 90% took at most ${gpuH(m.p90)}.` : 'No accepted proof yet.',
		),
		cell(
			'recent', gpuH(r.perProof), `GPU-hours per proof, all-in, ${trials(r.from, r.to)}`,
			`Every task of those trials, failed leaves and calibration leaves included: ${gpuH(r.hours)} GPU-hours over ${plural(r.proofs, 'accepted proof')}.`,
		),
		cell(
			'overall', gpuH(o.perProof), `GPU-hours per proof, all-in, ${trials(o.from, o.to)}`,
			`Every trial and outline probe since the first: ${gpuH(o.hours)} GPU-hours over ${plural(o.proofs, 'accepted proof')}.`,
		),
	].join('');
}

const pageLink = (t: TheoremRow, pages: ReadonlySet<string>, text: string) =>
	pages.has(t.leaf) ? `<a href="${esc(theoremPath(t.leaf))}">${esc(text)}</a>` : esc(text);
/** What a reuse reused, as HTML: "imports <a>Name</a> by author". */
const reuseWhat = (t: TheoremRow) => {
	const r = t.reuses;
	return r?.theorem
		? `${relationWord(r.relation)} ${r.url && theoremURL.test(r.url) ? `<a href="${esc(r.url)}">${esc(r.theorem)}</a>` : esc(r.theorem)}${r.author ? ` by ${esc(r.author)}` : ''}`
		: 'a theorem already proved on prove2me';
};
const reuseLine = (t: TheoremRow) => (t.kind === 'reuse' ? `<span class="sub" data-reuses>Closed by reuse: ${reuseWhat(t)}. Not counted as a proof by the model.</span>` : '');
/** prove2me's page of the theorem: its statement, until this page has its own. */
const statementLink = (t: TheoremRow) => (t.url ? `<a href="${esc(t.url)}" data-statement>statement on prove2me →</a>` : '–');

/** The table's rows, newest accepted first. `pages`: the leaves with a page of their own (built ones). */
export function theoremRowsHTML(v: TheoremsView, pages: ReadonlySet<string>): string {
	return v.theorems
		.map((t) => {
			const at = t.verdict_at ?? t.submitted_at;
			return (
				`<tr data-theorem-leaf="${esc(t.leaf)}" data-kind="${t.kind}">` +
				`<th scope="row">${pageLink(t, pages, t.leaf)}${t.alias ? `<span class="sub" title="The id the trials used">${esc(t.alias)}</span>` : ''}</th>` +
				`<td class="thm"><span class="name">${pageLink(t, pages, nameOf(t))}</span>${t.theorem && t.theorem !== nameOf(t) ? `<code class="lean">${esc(t.theorem)}</code>` : ''}${reuseLine(t)}` +
				`<span class="sub">${statementLink(t)}</span></td>` +
				`<td data-label="Accepted" class="when"><time datetime="${esc(at)}">${esc(day(at))}</time><span class="sub"><a href="/science/prove2me/#trial-${t.trial}">Trial ${t.trial}</a></span></td>` +
				`<td data-label="GPU-hours" class="num" data-cell="hours">${t.kind === 'reuse' ? 'reuse' : gpuH(t.hours)}` +
				`<span class="sub" data-cell="tokens" title="Tokens computed">${t.tokens === null ? '' : `${t.exact ? '' : '≥ '}${esc(compact(t.tokens))} tok`}</span></td>` +
				`</tr>`
			);
		})
		.join('');
}

/** Where the page's numbers are from: the browser's live read, the build's read of the gateway, or the committed copy the
 * build fell back on. */
export function updatedText(v: TheoremsView, from: 'live' | 'build' | 'saved' = 'live'): string {
	const at = `${day(v.updated)}, ${hhmm(v.updated)}`;
	if (from === 'live') return `Live from the gateway’s records, updated ${at}. The page reads them again every minute.`;
	return from === 'saved'
		? `From a saved copy of the gateway’s records of ${at}: the site’s build could not reach the gateway. The page reads the records live when it can.`
		: `From the gateway’s records at the site’s build, ${at}. The page reads them again every minute.`;
}

// ---- one theorem's page

/** The facts of one accepted leaf (the dl's children). */
export function theoremFactsHTML(t: TheoremRow): string {
	const at = t.verdict_at ?? t.submitted_at;
	const row = (key: string, label: string, value: string) => `<div data-fact="${key}"><dt>${esc(label)}</dt><dd>${value}</dd></div>`;
	return [
		row('accepted', 'Accepted by prove2me', `<time datetime="${esc(at)}">${esc(day(at))}, ${esc(hhmm(at))}</time>`),
		row('kind', 'Proof', t.kind === 'reuse' ? 'Closed by reuse of a theorem already proved on prove2me' : 'Written by the model, checked by Lean, accepted by prove2me'),
		row('trial', 'Trial', `<a href="/science/prove2me/#trial-${t.trial}">Trial ${t.trial}</a>`),
		row('hours', 'GPU-hours', t.kind === 'reuse' ? '– (no run: closed by reuse)' : esc(gpuH(t.hours))),
		row('tokens', 'Tokens computed', t.tokens === null ? '–' : `${t.exact ? '' : '≥ '}${esc(compact(t.tokens))}`),
		row('leaf', 'Leaf', `<code>${esc(t.leaf)}</code>${t.alias ? ` (${esc(t.alias)} in the trials)` : ''}`),
	].join('');
}

/** On a reused leaf's own page: what the accepted proof reused ("imports <a>Name</a> by author"). */
export const theoremReuseHTML = (t: TheoremRow) => reuseWhat(t);

/** One theorem's share card. */
export function cardOf(t: TheoremRow): TheoremCard {
	const at = t.verdict_at ?? t.submitted_at;
	return {
		kicker: t.kind === 'reuse' ? `Closed by reuse · ${day(at)}` : `Accepted by prove2me · ${day(at)}`,
		title: nameOf(t),
		mono: t.theorem !== nameOf(t) ? t.theorem : t.leaf,
		facts: t.kind === 'reuse'
			? [{ label: 'Trial', value: String(t.trial) }, { label: 'Model run', value: 'none' }, { label: 'Leaf', value: t.leaf }]
			: [
					{ label: 'GPU-hours', value: gpuH(t.hours) },
					{ label: 'Trial', value: String(t.trial) },
					{ label: 'Tokens computed', value: t.tokens === null ? '–' : compact(t.tokens) },
				],
	};
}

/** The list's share card. */
export function listCardOf(v: TheoremsView): TheoremCard {
	return {
		kicker: `Proved theorems · as of ${day(v.updated)}`,
		title: `${v.counts.proofs} Lean proofs by an open model, accepted by prove2me`,
		mono: `${v.counts.accepted} of ${v.counts.submitted} submissions accepted`,
		facts: [
			{ label: 'GPU-h per proof, median', value: gpuH(v.costs.median?.value ?? null) },
			{ label: `All-in, trials ${v.costs.recent.from}+`, value: gpuH(v.costs.recent.perProof) },
			{ label: 'All-in, every trial', value: gpuH(v.costs.overall.perProof) },
		],
	};
}

// ---- the JSON export

/** The page's data as JSON: the counts, the costs with what they cover, and every accepted leaf. */
export function theoremsJSON(v: TheoremsView, site: string, model: string) {
	return {
		about:
			`Leaves of prove2me (theorems from research papers, stated in Lean 4) that ${model}, an open model on the AIs@home network, proved, and that prove2me accepted. ` +
			`Computed from the gateway's public records (${site}/v1/projects/prove2me/results, /campaigns and /v1/tasks?project=prove2me) at the site's build; the page reads them live.`,
		updated: v.updated,
		source: v.source === 'live' ? 'gateway' : 'build',
		definitions: {
			gpu_hours: 'The time the model spent generating, summed over a run’s tasks (model minutes / 60). Lean checks and runner time are not counted; tasks cancelled before they finished are missing from the counts.',
			median: `The median of each accepted proof’s own model hours (the leaf’s tasks in the trial that proved it), trial ${MEDIAN_FROM} on.`,
			all_in: 'Every model hour of the trials in range (failed leaves, calibration leaves and outline probes included) over the proofs by the model accepted from them.',
			selection: `From trial ${SELECTED_FROM} on, the trials ran only leaves judged easy or moderate.`,
			reuse: 'A leaf closed by reuse: the accepted proof is a theorem someone else had already proved on prove2me. Never counted as a proof by the model or in the costs.',
		},
		counts: v.counts,
		costs: v.costs,
		theorems: v.theorems.map((t) => ({
			leaf: t.leaf, alias: t.alias || null, kind: t.kind, theorem: t.theorem, title: t.title, prove2me_url: t.url || null, page: `${site}${theoremPath(t.leaf)}`,
			trial: t.trial, submitted_at: t.submitted_at, accepted_at: t.verdict_at, gpu_hours: t.hours === null ? null : Math.round(t.hours * 1000) / 1000,
			tokens_computed: t.tokens, tokens_exact: t.exact, ...(t.reuses ? { reuses: t.reuses } : {}),
		})),
	};
}
