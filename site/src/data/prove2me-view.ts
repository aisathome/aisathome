// The prove2me page's parts that read the gateway's records: the headline numbers, the "Small numbers" text,
// the submissions, the trials table and each trial's own list from trial 6 on, the home page's line and the /science/
// card. One HTML renderer each (every value escaped), for the build (from the build's history) and the browser (with
// the gateway's records, prove2me-records.ts) alike, so the two never say different things.
//
// SPDX-License-Identifier: Apache-2.0
import { FROZEN_THROUGH, day, hhmm, theoremURL } from './prove2me-current';
import { compact, fromStatic, leafDetail, leafOutcomeLabel, leafOutcomes, leafRoleLabel, taskTableHTML, type Outline, type StaticTask } from './prove2me-live';
import { leafPlan, leafPlanHTML } from './prove2me-outlines';
import { attemptOf, provesLeaf } from './prove2me-outcomes';
import type { Kpis, LeavesView, ScienceView, SubRec, TrialDetail, TrialSum } from './prove2me-records';
import { shortSpan, tokTitleOf } from './prove2me-trials';
import { texText } from './tex-text';

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const safe = (u: string | undefined | null) => (u && theoremURL.test(u) ? u : '');
const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);
const link = (url: string | null | undefined, text: string, attrs = '') => (safe(url) ? `<a href="${esc(url)}"${attrs}>${esc(text)}</a>` : `<span${attrs}>${esc(text)}</span>`);

/** A prove2me title as plain text, its TeX written in Unicode (tex-text.ts; the site has no TeX renderer). */
export const plainTitle = (t: string) => texText(t);
/** A trial's dates: "7 November 2026", "9 to 12 November 2026", "since 16 November 2026" while it runs. */
export function span(t: { start: string; end: string | null }) {
	if (!t.end) return `since ${day(t.start)}`;
	if (t.start === t.end) return day(t.start);
	const a = new Date(`${t.start}T00:00:00Z`), b = new Date(`${t.end}T00:00:00Z`);
	if (a.getUTCMonth() === b.getUTCMonth()) return `${a.getUTCDate()} to ${day(t.end)}`;
	return `${day(t.start, false)} to ${day(t.end)}`;
}
/** Plain text (a campaign's text) as HTML paragraphs: blank lines part them, everything escaped, no markup. */
export function plainHTML(s: string): string {
	return s
		.trim()
		.split(/\n\s*\n/)
		.filter((p) => p.trim())
		.map((p) => `<p>${esc(p.trim().replace(/\s*\n\s*/g, ' '))}</p>`)
		.join('');
}
const named = (leaf: string, alias: string) => (alias && alias !== leaf ? `${leaf} (${alias})` : leaf);
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// ---- the headline numbers

/** The KPIs' cells (the dl's children); `--kpis` is their number. */
export function kpisHTML(k: Kpis): { html: string; n: number } {
	const cells = [
		`<div data-kpi="accepted"><dt>${k.proofs === 1 ? 'proof' : 'proofs'} by the model accepted by prove2me, of ${k.submitted} submitted${k.reused ? ` (${k.reused} of them ${k.reused === 1 ? 'a reuse' : 'reuses'})` : ''}</dt><dd>${k.proofs}</dd></div>`,
		`<div data-kpi="open"><dt>open leaves with an accepted proof, of ${k.openTried} tried</dt><dd>${k.openAccepted}</dd></div>`,
		k.reused > 0 ? `<div data-kpi="reused"><dt>${k.reused === 1 ? 'open leaf' : 'open leaves'} closed by reuse of a proof already on prove2me (not counted as proofs)</dt><dd>${k.reused}</dd></div>` : '',
		`<div data-kpi="runs"><dt>runs that passed our own check, of ${k.runs} in trials 1 to ${k.lastTrial}</dt><dd>${k.proved}</dd></div>`,
	].filter(Boolean);
	return { html: cells.join(''), n: cells.length };
}

/** The page where every accepted proof has its line and its cost. */
const THEOREMS = '/science/prove2me/theorems/';

/** The prove2me page's headline (the block's children): the proved theorems page's number in its words, what it is out of,
 * and the way to that page. */
export function headlineStripHTML(v: ScienceView, model: string): string {
	const n = v.kpis.proofs; // the proved theorems page's headline number (prove2me-records.ts Kpis)
	return (
		`<p class="p2m-hl-n" data-p2m-hl-n>${n}</p>` +
		`<div class="p2m-hl-text"><p class="p2m-hl-what">Lean 4 ${n === 1 ? 'proof' : 'proofs'} written by ${esc(model)} and accepted by prove2me, ` +
		`out of ${esc(plural(v.kpis.openTried, 'open leaf', 'open leaves'))} tried.</p>` +
		`<p class="p2m-hl-go"><a href="${THEOREMS}" data-p2m-theorems-link="headline">The proved theorems, and what each cost <span aria-hidden="true">→</span></a></p></div>`
	);
}

/** "Small numbers": what the counts show and what they do not. The two early proofs (O04, F01) are history and named
 * so; every count comes from the view. `f01Tasks`: F01's task count in trial 5 (the build's). */
export function smallNumbersHTML(v: ScienceView, _f01Tasks?: number, _idOf?: (alias: string) => string): string {
	const k = v.kpis;
	const hours = k.hours + (k.probeHours > 0 ? k.probeHours : 0);
	return esc(
		`Small numbers: ${k.openAccepted} open leaves with an accepted proof so far` +
			`${k.openProved > k.openAccepted ? ` (${k.openProved - k.openAccepted} more passed our own check without an accepted proof)` : ''}. ` +
			`That shows the model can do it, not yet how often. ` +
			`On calibration leaves, which others had proved before, ${k.calibrationProved} of ${k.calibrationRuns} runs succeeded. ` +
			`All runs together took ${Math.round(hours)} GPU-hours.`,
	);
}

// ---- the submissions

const verdictWord: Record<string, string> = { accepted: 'Accepted', pending: 'Pending', rejected: 'Rejected', error: 'Not judged: an error' };

/** One submission as a row of the table. */
export function submissionRowHTML(s: SubRec): string {
	const r = s.reuses;
	const reuse = r?.theorem
		? `<span class="sub" data-reuses>${r.relation === 'part' ? 'part of' : r.relation === 'import' ? 'imports' : 'same statement as'} ${link(r.url, r.theorem)}${r.author ? ` by ${esc(r.author)}` : ''}</span>`
		: '';
	const from = s.kind === 'reuse'
		? `<td data-label="From">Closed by reuse<span class="sub">${s.found === 'trial' ? `found during trial ${s.trial}` : `found while preparing trial ${s.trial}`}</span></td>`
		: `<td data-label="From">Trial ${s.trial}</td>`;
	return (
		`<tr data-submission="${esc(s.alias || s.leaf)}" data-leaf-id="${esc(s.leaf)}" data-verdict="${esc(s.verdict)}" data-kind="${esc(s.kind)}">` +
		`<th scope="row">${esc(s.leaf)}${s.alias ? `<span class="sub" data-alias title="The id the trials used">${esc(s.alias)}</span>` : ''}</th>` +
		`<td class="thm">${link(s.url, s.theorem, ' data-theorem')}${s.title && plainTitle(s.title) !== s.theorem ? `<span class="sub">${esc(plainTitle(s.title))}</span>` : ''}${reuse}</td>` +
		from +
		`<td data-label="Submitted"><time datetime="${esc(s.submitted_at)}">${esc(day(s.submitted_at))}, ${esc(hhmm(s.submitted_at))}</time></td>` +
		`<td data-label="Verdict" class="verdict verdict-${esc(s.verdict)}">${esc(verdictWord[s.verdict] ?? s.verdict)}${s.verdict_at ? `<span class="sub">${esc(hhmm(s.verdict_at))}</span>` : ''}</td>` +
		`</tr>`
	);
}

/** The submissions as the table lists them: newest trial first, the newest submission first within a trial. */
export const newestFirst = (subs: SubRec[]) => [...subs].sort((a, b) => b.trial - a.trial || b.submitted_at.localeCompare(a.submitted_at));

/** The table's rows, newest trial first (the table shows about five and scrolls for the rest). */
export const submissionRowsHTML = (v: ScienceView) => newestFirst(v.submissions).map(submissionRowHTML).join('');

// ---- the trials (2026-11-28: one list, newest first; each trial one row that opens to its own list, instead of a
// table of eight columns above a second list of the same trials)

const tokText = (computed: number, exactCount: boolean) => `${exactCount ? '' : '≥ '}${compact(computed)}`;
/** GPU-hours as the theorems page writes them: two decimals under 10, one above. */
const gpuH = (h: number) => (h < 10 ? h.toFixed(2) : h.toFixed(1));

/** The model's proofs from trial `n` that prove2me accepted (reuses left out, as in the headline). */
export const acceptedOf = (v: ScienceView, n: number) => v.submissions.filter((s) => s.trial === n && s.kind !== 'reuse' && s.verdict === 'accepted').length;

/** A trial's dates for its row: "7 Nov", "9–12 Nov", "16 Nov–" while it runs; '' before it starts. */
function rowDates(t: TrialSum): string {
	if (!t.start) return '';
	return `<time datetime="${esc(t.start)}">${esc(shortSpan({ start: t.start, end: t.end ?? t.start }))}${t.end ? '' : '–'}</time>`;
}

/** A cell of a trial's row. `unit` is said after the value (before it with `before`): read aloud everywhere, shown on a
 * phone, where the row wraps and the list's head is hidden; on a wide screen the head names the columns. */
const cell = (key: string, value: string, unit: string, attrs = '', before = false) =>
	`<span class="tr-c tr-${key}" data-cell="${key}"${attrs}>${before ? `<span class="u">${esc(unit)} </span>${value}` : `${value}<span class="u"> ${esc(unit)}</span>`}</span>`;

/** A trial's row (its list's summary): trial, dates, scaffold, leaves tried, accepted proofs, GPU-hours per accepted
 * proof (its model hours over them; – when none was accepted, its hours in the tooltip). */
export function trialSummaryHTML(t: TrialSum, accepted: number): string {
	const h = t.model_min / 60;
	const state = t.status === 'running' ? ' <span class="tr-run">running</span>' : t.status === 'preparing' ? ' <span class="tr-run">being prepared</span>' : '';
	return (
		`<span class="tr-name">Trial ${t.number}${state}</span>` +
		`<span class="tr-c tr-dates" data-cell="dates">${rowDates(t) || '–'}</span>` +
		cell('scaffold', esc(t.scaffold || '–'), 'scaffold', '', true) +
		cell('leaves', String(t.all.runs), t.all.runs === 1 ? 'leaf' : 'leaves') +
		cell('accepted', String(accepted), 'accepted') +
		cell('per-proof', accepted ? esc(gpuH(h / accepted)) : '–', accepted ? 'GPU-h per proof' : 'GPU-h per proof:', ` title="${esc(`${gpuH(h)} GPU-hours${accepted ? ` over ${plural(accepted, 'accepted proof')}` : ', no proof accepted'}`)}"`, !accepted)
	);
}

/** The list's head on a wide screen: the columns' names (the rows say them themselves to a screen reader). */
export const trialsHeadHTML = () =>
	`<span class="tr-name">Trial</span><span class="tr-c">Dates</span><span class="tr-c">Scaffold</span><span class="tr-c">Leaves</span>` +
	`<span class="tr-c">Accepted</span><span class="tr-c tr-per-proof">GPU-h per proof</span>`;

/** The counts a trial's row leaves out (our own check's, by role; model hours; tokens computed), as one line: under a
 * trial's row and in the All line. */
function runCounts(all: { proved: number; runs: number }, cal: { proved: number; runs: number }, open: { proved: number; runs: number }, hours: number, computed: number, withCache: number, exactCount: boolean, hoursNote = '') {
	const c = (key: string, x: { proved: number; runs: number }) => `<span data-cell="${key}">${x.proved} of ${x.runs}</span>`;
	return (
		`Runs that passed our own check: ${c('proved', all)}` +
		(cal.runs ? ` (calibration ${c('calibration', cal)}, open ${c('open', open)})` : ` (open ${c('open', open)})`) +
		` · Model hours: <span data-cell="hours">${esc(hours.toFixed(1))}</span>${esc(hoursNote)}` +
		` · Tokens computed: <span class="tok" data-cell="tokens" data-tokens="${computed}" data-with-cache="${withCache}" title="${esc(tokTitleOf(computed, withCache, exactCount))}">${esc(tokText(computed, exactCount))}</span>`
	);
}

/** Under a trial's row: its counts the row leaves out. */
export const trialNumbersHTML = (t: TrialSum) =>
	`<p class="caps" data-trial-numbers>${runCounts(t.all, t.calibration, t.open, t.model_min / 60, t.computed, t.withCache, t.exact)}</p>`;

/** A row with no list under it: an outline probe round (its model time counts in the totals), or a trial whose list the
 * page does not have. */
export function trialPlainRowHTML(t: TrialSum, accepted = 0): string {
	if (!t.probe)
		return `<div class="p2m-trow" id="trial-${t.number}" data-p2m-trial="${t.number}" data-trial="${esc(t.label)}" data-source="${t.source}">${trialSummaryHTML(t, accepted)}</div>`;
	return (
		`<div class="p2m-trow p2m-tprobe" data-trial="${esc(t.label)}" data-probes data-source="${t.source}">` +
		`<span class="tr-name">Trial ${t.number} probes</span>` +
		`<span class="tr-c tr-dates" data-cell="dates">${rowDates(t) || '–'}</span>` +
		`<span class="tr-c tr-wide">Outline probes to choose its leaves: <span data-cell="hours">${esc(gpuH(t.model_min / 60))}</span> GPU-h, ` +
		`<span class="tok" data-cell="tokens" data-tokens="${t.computed}" data-with-cache="${t.withCache}" title="${esc(tokTitleOf(t.computed, t.withCache, t.exact))}">${esc(tokText(t.computed, t.exact))}</span> tokens computed</span>` +
		`</div>`
	);
}

/** The order of the list: newest trial first, a trial's outline probes under it (they ran before it). */
export const trialsNewestFirst = (v: ScienceView) => [...v.trials].reverse();

/** The list's All line: every trial and probe round together, as the theorems page's all-in cost counts them. */
export function trialsFootHTML(v: ScienceView): string {
	const k = v.kpis;
	const trials = v.trials.filter((t) => !t.probe && t.all.runs > 0);
	const hours = k.hours + k.probeHours;
	const proofs = k.proofs;
	const open = trials.reduce((a, t) => ({ proved: a.proved + t.open.proved, runs: a.runs + t.open.runs }), { proved: 0, runs: 0 });
	const exactCount = v.trials.every((t) => t.exact);
	return (
		`<span class="tr-name">All</span>` +
		cell('trials', String(trials.length), trials.length === 1 ? 'trial' : 'trials') +
		`<span class="tr-c tr-scaffold"></span>` +
		cell('leaves', String(k.runs), k.runs === 1 ? 'leaf' : 'leaves') +
		cell('accepted', String(proofs), 'accepted') +
		cell('per-proof', proofs ? esc(gpuH(hours / proofs)) : '–', 'GPU-h per proof', ` title="${esc(`${gpuH(hours)} GPU-hours, outline probes included${proofs ? `, over ${plural(proofs, 'accepted proof')}` : ''}`)}"`) +
		`<span class="tr-more" data-trials-numbers>${runCounts({ proved: k.proved, runs: k.runs }, { proved: k.calibrationProved, runs: k.calibrationRuns }, open, hours, k.computed, k.withCache, exactCount, ', outline probes included')}</span>`
	);
}

// ---- a trial's own list (trial 6 on)

/** The trial's facts in one sentence, made from its records. */
export function trialFacts(d: TrialDetail): string {
	const t = d.sum;
	// the runs: a leaf closed by reuse is none (it is named after them)
	const ran = d.leaves.filter((l) => !l.reuse);
	const roles = (['calibration', 'open', 'comparison'] as const)
		.map((r) => [r, ran.filter((l) => l.role === r || (r === 'open' && !['calibration', 'comparison'].includes(l.role))).length] as const)
		.filter(([, n]) => n > 0)
		.map(([r, n]) => `${n} ${r}`);
	const when = !t.start
		? 'is being prepared'
		: t.status === 'running'
			? `has run since ${day(t.start)}`
			: t.end && t.end !== t.start
				? `ran from ${span({ start: t.start, end: t.end })}`
				: `ran on ${day(t.start)}`;
	const leaves = ran.length ? `: ${plural(ran.length, 'leaf', 'leaves')}${roles.length ? ` (${roles.join(', ')})` : ''}` : '';
	const proved = ran.length ? `, ${t.status === 'running' ? 'so far ' : ''}${t.all.proved} of ${t.all.runs} ${t.all.runs === 1 ? 'run' : 'runs'} proved` : '';
	const acc = d.accepted ? `; prove2me accepted ${plural(d.accepted, 'proof')} from it` : '';
	const reuse = d.reused ? `${acc ? ' and' : ';'} ${plural(d.reused, 'leaf', 'leaves')} closed by reuse` : '';
	return `Trial ${t.number} ${when} on scaffold ${t.scaffold || '?'}${leaves}${proved}${acc}${reuse}.`;
}

/** A trial's list (opened, 2026-11-28 drawn only then: lazyHTML): a row per leaf and, under them, each leaf's outlines
 * and tasks. */
export function trialListHTML(d: TrialDetail): string {
	const rows = d.leaves
		.map((l) => {
			// as the build's earlier trials: the outcome first (the outline that proved it), the leaf's line under it
			const res = l.reuse ? 'reused' : l.proved ? 'proved' : /^(Running|Queued)/.test(l.summary) ? 'running' : 'not_proved';
			const outlines = new Set(l.detail.rows.map((r) => r.gen)).size;
			const won = l.detail.rows.find((r) => provesLeaf(attemptOf(r)));
			const word = { reused: 'Closed by reuse', proved: won && outlines > 1 ? `Proved with outline ${won.gen}` : 'Proved', running: 'Running', not_proved: 'Not proved' }[res];
			const mins = l.detail.rows.reduce((a, r) => a + (r.minutes ?? 0), 0);
			return (
				`<tr data-leaf="${esc(l.alias)}" data-leaf-id="${esc(l.id)}" data-outcome="${res}">` +
				`<th scope="row">${esc(l.id)}${l.alias && l.alias !== l.id ? `<span class="sub" data-alias title="The id the trials used">${esc(l.alias)}</span>` : ''}</th>` +
				`<td class="thm">${link(l.url, l.theorem)}${l.title ? `<span class="sub">${esc(plainTitle(l.title))}</span>` : ''}</td>` +
				`<td data-label="Kind">${esc(leafRoleLabel[l.role] ?? 'Open')}${l.earlier.length ? `<span class="sub">tried before in trial${l.earlier.length > 1 ? 's' : ''} ${esc(andList(l.earlier.map(String)))}</span>` : ''}</td>` +
				`<td data-label="Result" class="result ${res === 'proved' ? 'proved' : res === 'not_proved' ? 'failed' : ''}" data-cell="result">${esc(word)}<span class="sub" data-leaf-line>${esc(l.summary)}</span></td>` +
				`<td data-label="Tasks" class="num">${l.detail.rows.length}<span class="sub">${outlines === 1 ? '1 outline' : `${outlines} outlines`}</span></td>` +
				`<td data-label="Model min" class="num">${mins.toFixed(1)}</td>` +
				`<td data-label="Tokens computed" class="num tok" data-tokens="${l.detail.computed}" data-with-cache="${l.detail.total}" title="${esc(tokTitleOf(l.detail.computed, l.detail.total, l.detail.exact))}">${esc(compact(l.detail.computed))}</td>` +
				`</tr>`
			);
		})
		.join('');
	const plans = d.leaves
		.filter((l) => l.detail.rows.length)
		.map(
			(l) =>
				`<div class="p2m-trial-leaf" data-trial-leaf="${esc(l.alias)}"><p class="p2m-tleaf">${esc(named(l.id, l.alias))}</p>` +
				leafPlanHTML(l.alias, leafPlan(l.detail, [], l.reuse), true) +
				taskTableHTML(l.alias, l.detail, `${named(l.id, l.alias)}: every task`) +
				`</div>`,
		)
		.join('');
	return (
		(rows
			? `<div class="p2m-wrap not-content"><table class="p2m-table p2m-leaves"><thead><tr><th scope="col">Leaf</th><th scope="col">Theorem</th><th scope="col">Kind</th><th scope="col">Result</th><th scope="col" class="num">Tasks</th><th scope="col" class="num">Model min</th><th scope="col" class="num">Tokens computed</th></tr></thead><tbody>${rows}</tbody></table></div>`
			: '') +
		(plans
			? `<div class="p2m-trial-tasks not-content" data-p2m-trial-tasks="${d.sum.number}"><p class="caps">Each leaf’s outlines, newest first, with what became of every hole; under each, every task (outline, hole, try and assembly) with its counts, as in “Working on now”.</p>${plans}</div>`
			: '')
	);
}

/** A trial's row and what stays under it closed: its facts and text, its budget; its list (trialListHTML) only with
 * `list`, else the place the browser draws it in when the row opens. */
export function trialDetailHTML(d: TrialDetail, list = true, open = false): string {
	const t = d.sum;
	return (
		`<details${open ? ' open' : ''} class="p2m-trial" id="trial-${t.number}" data-p2m-trial="${t.number}" data-trial="${esc(t.label)}" data-source="${t.source}"${t.status !== 'finished' ? ` data-status="${t.status}"` : ''}>` +
		`<summary class="p2m-trow">${trialSummaryHTML(t, d.accepted)}</summary>` +
		`<p data-trial-facts>${esc(trialFacts(d))}</p>` +
		trialNumbersHTML(t) +
		(d.text.trim() ? `<div data-trial-text>${plainHTML(d.text)}</div>` : '') +
		(d.budget ? `<p class="caps">Budget per leaf: ${esc(d.budget)}.</p>` : '') +
		(list ? trialListHTML(d) : trialLazyHTML(t.number)) +
		`</details>`
	);
}

// ---- the parts the browser draws when they are opened (2026-11-28: the page was 2.4 MB of HTML, 1.9 MB of it in
// sections that start closed). Without JavaScript each such place links the same part on the record page
// (/science/prove2me/record/), which the build draws in full with the same renderers.

export const RECORD_PATH = '/science/prove2me/record/';
/** The build's data for those parts (pages/science/prove2me/record.json.ts). */
export const RECORD_JSON = '/science/prove2me/record.json';

/** The place of a part drawn when opened: `what` and a link to it on the record page (`anchor`). */
export function lazyHTML(kind: 'trial' | 'now' | 'leaves' | 'tasks', key: string, anchor: string, what: string): string {
	return (
		`<div class="p2m-lazy" data-lazy="${kind}" data-lazy-key="${esc(key)}">` +
		`<p class="p2m-lazy-note">${esc(what)}: <a href="${RECORD_PATH}#${esc(anchor)}" data-p2m-record-link="${kind}">on the record page</a>.</p></div>`
	);
}
export const trialLazyHTML = (n: number) => lazyHTML('trial', String(n), `trial-${n}`, `Trial ${n}’s leaves, with each leaf’s outlines and tasks`);

/** A frozen trial's leaf (trials 1 to 5, from the build's history; prove2me-page.ts frozenTrials), as its list shows it. */
export interface FrozenLeaf {
	leaf: string;
	/** The leaf's id (p2m-...), when the build knows it. */
	id: string | null;
	theorem: string;
	url: string;
	title: string;
	group: string;
	earlier: number[];
	outcome: string;
	scored: boolean;
	/** The result in words (prove2me.ts outcomeLabel) and, for a run not scored, why. */
	result: string;
	why: string;
	rounds: number;
	minutes: number;
	computed: number;
	withCache: number;
	tokTitle: string;
	/** From trial 5 (v5): the run's tasks and outlines; null before. */
	tasks: StaticTask[] | null;
	outlines: Outline[];
	/** A leaf closed by reuse: the theorem it imports. */
	reuse?: string;
}
export interface FrozenTrial {
	trial: number;
	/** The trial in the history's words, and its budget per leaf. */
	summary: string;
	caps: string;
	v5: boolean;
	leaves: FrozenLeaf[];
}
const groupLabel: Record<string, string> = { calibration: 'Calibration', open: 'Open' };

/** A frozen trial's row and what stays under it closed (its words, counts and budget); its list only with `list`. */
export function frozenTrialHTML(f: FrozenTrial, t: TrialSum, accepted: number, list = true, open = false): string {
	return (
		`<details${open ? ' open' : ''} class="p2m-trial" id="trial-${f.trial}" data-p2m-trial="${f.trial}" data-trial="${esc(t.label)}" data-source="build">` +
		`<summary class="p2m-trow">${trialSummaryHTML(t, accepted)}</summary>` +
		`<p>${esc(f.summary)}</p>` +
		trialNumbersHTML(t) +
		`<p class="caps">Budget per leaf: ${esc(f.caps)}.</p>` +
		(list ? frozenListHTML(f) : trialLazyHTML(f.trial)) +
		`</details>`
	);
}

/** Every trial's row, newest first (trialsNewestFirst): trials 1 to 5 from the history (`frozen`), the later ones from
 * their records (`details`), a trial with neither and a probe round as a plain row. Their lists drawn with `list`
 * (the record page; the page draws a list when it opens), and open with `open`. */
export function trialRowsHTML(v: ScienceView, frozen: FrozenTrial[], details: TrialDetail[], list: boolean, open = false): string {
	return trialsNewestFirst(v)
		.map((t) => {
			if (t.probe) return trialPlainRowHTML(t);
			const built = details.find((d) => d.sum.number === t.number);
			if (built) return trialDetailHTML(built, list, open);
			const f = t.number <= FROZEN_THROUGH ? frozen.find((x) => x.trial === t.number) : undefined;
			return f ? frozenTrialHTML(f, t, acceptedOf(v, t.number), list, open) : trialPlainRowHTML(t, acceptedOf(v, t.number));
		})
		.join('');
}

/** A frozen trial's list: a row per leaf and, from trial 5, each leaf's outlines and tasks. */
export function frozenListHTML(f: FrozenTrial): string {
	const rows = f.leaves
		.map((r) => {
			const e = r.earlier;
			return (
				`<tr data-leaf="${esc(r.leaf)}" data-outcome="${esc(r.outcome)}" data-scored="${r.scored}">` +
				`<th scope="row">${esc(r.id ?? r.leaf)}${r.id ? `<span class="sub" data-alias title="The id the trials used">${esc(r.leaf)}</span>` : ''}</th>` +
				`<td class="thm">${link(r.url, r.theorem)}<span class="sub">${esc(plainTitle(r.title))}</span></td>` +
				`<td data-label="Kind">${esc(groupLabel[r.group] ?? r.group)}${e.length ? `<span class="sub">tried before in trial${e.length > 1 ? 's' : ''} ${esc(e.join(' and '))}</span>` : ''}</td>` +
				`<td data-label="Result" class="result ${r.outcome === 'proved' ? 'proved' : r.scored ? 'failed' : 'unscored'}">${esc(r.result)}${!r.scored && r.why ? `<span class="sub">${esc(r.why)}</span>` : ''}</td>` +
				(r.tasks
					? `<td data-label="Tasks" class="num">${r.tasks.length}<span class="sub">${r.rounds === 1 ? '1 outline' : `${r.rounds} outlines`}</span></td>`
					: `<td data-label="Rounds" class="num">${r.rounds}</td>`) +
				`<td data-label="Model min" class="num">${r.minutes.toFixed(1)}</td>` +
				`<td data-label="Tokens computed" class="num tok" data-tokens="${r.computed}" data-with-cache="${r.withCache}" title="${esc(r.tokTitle)}">${esc(compact(r.computed))}</td>` +
				`</tr>`
			);
		})
		.join('');
	const plans = f.v5
		? f.leaves
				.filter((r) => r.tasks?.length)
				.map((r) => {
					const d = leafDetail(r.tasks!.map(fromStatic), r.outlines);
					const name = named(r.id ?? r.leaf, r.leaf);
					return (
						`<div class="p2m-trial-leaf" data-trial-leaf="${esc(r.leaf)}"><p class="p2m-tleaf">${esc(name)}</p>` +
						leafPlanHTML(r.leaf, leafPlan(d, [], r.reuse), true) +
						taskTableHTML(r.leaf, d, `${name}: every task`) +
						`</div>`
					);
				})
				.join('')
		: '';
	return (
		`<div class="p2m-wrap not-content"><table class="p2m-table p2m-leaves"><thead><tr><th scope="col">Leaf</th><th scope="col">Theorem</th><th scope="col">Kind</th><th scope="col">Result</th>` +
		`<th scope="col" class="num">${f.v5 ? 'Tasks' : 'Rounds'}</th><th scope="col" class="num">Model min</th><th scope="col" class="num">Tokens computed</th></tr></thead><tbody>${rows}</tbody></table></div>` +
		(f.v5
			? `<div class="p2m-trial-tasks not-content" data-p2m-trial-tasks="${f.trial}"><p class="caps">Each leaf’s outlines, newest first, with what became of every hole; under each, every task (outline, hole, try and assembly) with its counts, as in “Working on now”.</p>${plans}</div>`
			: '')
	);
}

// ---- every leaf: the scorecard by role, the filters and the list (prove2me-records.ts leavesView)

const numCell = (label: string, cell: string, v: string | number) => `<td data-label="${label}" class="num" data-cell="${cell}">${esc(v)}</td>`;
const scoreTok = (computed: number, withCache: number, exactCount: boolean) =>
	`<td data-label="Tokens computed" class="num tok" data-cell="tokens" data-tokens="${computed}" title="${esc(tokTitleOf(computed, withCache, exactCount))}">${exactCount ? '' : '≥ '}${esc(compact(computed))}</td>`;

/** The scorecard: a row per role and the outline probes (the tbody), the All row (the tfoot). */
export function leafScoreHTML(v: LeavesView): { body: string; foot: string } {
	const roles = v.roles.map(
		(r) =>
			`<tr data-role="${r.role}"><th scope="row">${esc(leafRoleLabel[r.role])}</th>${numCell('Leaves', 'leaves', r.leaves)}${numCell('Runs', 'runs', r.runs)}` +
			`${numCell('Runs proved', 'proved', r.proved)}${numCell('Proofs accepted', 'accepted', r.accepted)}${numCell('Closed by reuse', 'reused', r.reused)}` +
			`${numCell('Model hours', 'hours', r.hours.toFixed(1))}${scoreTok(r.computed, r.withCache, r.exact)}</tr>`,
	);
	const p = v.probes;
	const probes = p
		? `<tr data-role="probes"><th scope="row">Outline probes</th>${numCell('Leaves', 'leaves', p.leaves)}${['Runs:runs', 'Runs proved:proved', 'Proofs accepted:accepted', 'Closed by reuse:reused'].map((x) => numCell(x.split(':')[0], x.split(':')[1], '–')).join('')}` +
			`${numCell('Model hours', 'hours', p.hours.toFixed(1))}${scoreTok(p.computed, p.withCache, p.exact)}</tr>`
		: '';
	const a = v.all;
	const foot =
		`<tr data-role="all"><th scope="row">All</th>${numCell('Leaves', 'leaves', a.leaves)}${numCell('Runs', 'runs', a.runs)}${numCell('Runs proved', 'proved', a.proved)}` +
		`${numCell('Proofs accepted', 'accepted', a.accepted)}${numCell('Closed by reuse', 'reused', a.reused)}${numCell('Model hours', 'hours', a.hours.toFixed(1))}` +
		`${scoreTok(a.computed, a.withCache, a.exact)}</tr>`;
	return { body: roles.join('') + probes, foot };
}

/** The list's filters: one per role there is and one per outcome a leaf has. */
export function leafFiltersHTML(v: LeavesView): string {
	const btn = (key: string, value: string, label: string) => `<button type="button" data-filter="${key}" data-value="${esc(value)}" aria-pressed="${value ? 'false' : 'true'}">${esc(label)}</button>`;
	return (
		btn('role', '', 'All roles') +
		v.roles.map((r) => btn('role', r.role, leafRoleLabel[r.role])).join('') +
		'<span class="sep"></span>' +
		btn('outcome', '', 'Any outcome') +
		leafOutcomes.filter((o) => v.leaves.some((l) => l.outcome === o)).map((o) => btn('outcome', o, leafOutcomeLabel[o])).join('')
	);
}

/** The list's rows, in the order the leaves were first tried (data-order); the browser filters and sorts them. */
export function leafRowsHTML(v: LeavesView): string {
	return v.leaves
		.map((l, i) => {
			const cls = ['result', ['accepted', 'proved'].includes(l.outcome) ? 'proved' : '', ['not_scored', 'stopped'].includes(l.outcome) ? 'unscored' : ''].filter(Boolean).join(' ');
			const first = l.first ? `<time datetime="${esc(l.first)}">${esc(day(l.first))}</time>` : '–';
			const tok =
				l.computed === null
					? `<td data-label="Tokens computed" class="num" data-cell="tokens" title="No run: closed by reuse">–</td>`
					: `<td data-label="Tokens computed" class="num tok" data-cell="tokens" data-tokens="${l.computed}" title="${esc(tokTitleOf(l.computed, l.withCache ?? 0, l.exact))}">${l.exact ? '' : '≥ '}${esc(compact(l.computed))}</td>`;
			return (
				`<tr data-leaf-id="${esc(l.id)}" data-roles="${esc(l.roles.join(' '))}" data-outcome="${esc(l.outcome)}" data-order="${i}"` +
				`${l.first ? ` data-sort-first="${Date.parse(`${l.first}T00:00:00Z`)}"` : ''} data-sort-outcome="${leafOutcomes.indexOf(l.outcome)}"` +
				`${l.computed !== null ? ` data-sort-tokens="${l.computed}"` : ''}${l.minutes !== null ? ` data-sort-minutes="${l.minutes}"` : ''}>` +
				`<th scope="row">${esc(l.id)}<span class="sub" data-alias title="The id the trials used">${esc(l.aliases.join(', '))}</span></th>` +
				`<td class="thm">${link(l.url, l.theorem, `${l.title ? ` title="${esc(plainTitle(l.title))}"` : ''} data-sort-theorem`)}</td>` +
				`<td data-label="Role">${esc(l.roles.map((r) => leafRoleLabel[r]).join(', '))}</td>` +
				`<td data-label="Trials" data-cell="trials">${l.trials.length ? esc(l.trials.join(', ')) : '–'}</td>` +
				`<td data-label="Outcome" data-cell="outcome" class="${cls}">${esc(leafOutcomeLabel[l.outcome])}${l.runs > 1 ? `<span class="sub">${l.provedRuns} of ${l.runs} runs proved</span>` : ''}</td>` +
				`<td data-label="First tried" class="nowrap">${first}</td>` +
				`<td data-label="Model min" class="num" data-cell="minutes">${l.minutes === null ? '–' : l.minutes.toFixed(1)}</td>` +
				tok +
				`</tr>`
			);
		})
		.join('');
}

const leafCols: { key: string; label: string; num?: boolean }[] = [
	{ key: 'leaf', label: 'Leaf' },
	{ key: 'theorem', label: 'Theorem' },
	{ key: '', label: 'Role' },
	{ key: '', label: 'Trials' },
	{ key: 'outcome', label: 'Outcome' },
	{ key: 'first', label: 'First tried' },
	{ key: 'minutes', label: 'Model min', num: true },
	{ key: 'tokens', label: 'Tokens computed', num: true },
];

/** The list of every leaf as it opens: its filters, "Showing N of M" and the table (the browser filters and sorts it). */
export function leafListHTML(v: LeavesView): string {
	const head = leafCols
		.map((c) =>
			c.key
				? `<th scope="col"${c.num ? ' class="num"' : ''} data-sort-col="${c.key}"><button type="button" data-leaf-sort="${c.key}">${esc(c.label)}<span class="sort-mark" aria-hidden="true"></span></button></th>`
				: `<th scope="col" class="nosort">${esc(c.label)}</th>`,
		)
		.join('');
	return (
		`<div class="p2m-filters not-content" role="group" aria-label="Filter the leaves" data-p2m-filters>${leafFiltersHTML(v)}</div>` +
		`<p class="caps" data-p2m-list-count>Showing ${v.leaves.length} of ${v.leaves.length}</p>` +
		`<div class="p2m-wrap not-content"><table class="p2m-table p2m-lt" data-p2m-leaf-table><thead><tr>${head}</tr></thead><tbody>${leafRowsHTML(v)}</tbody></table></div>`
	);
}

// ---- the home page's line and the /science/ card

/** The home page's prove2me line, after its count: what the count is, and nothing else (2026-11-28: the line had grown to
 * four claims; what prove2me is, the ways in and Tau Ceti are their own short lines on the page). */
export function homeLine(v: ScienceView, model: string): string {
	const k = v.kpis;
	return `Lean ${k.proofs === 1 ? 'proof' : 'proofs'} by ${model} accepted by prove2me so far, from ${plural(k.openTried, 'open leaf', 'open leaves')} tried.`;
}

/** The /science/ card's status line: active, and the trial running or being prepared. */
export function cardStatus(v: ScienceView): string {
	const c = v.current;
	if (!c) return 'Active';
	return `Active: trial ${c.number} ${c.status === 'preparing' ? 'being prepared' : c.start ? `running since ${day(c.start)}` : 'running'}`;
}

/** The /science/ card's numbers, in the Tau Ceti card's style: the headline number (the model's proofs prove2me
 * accepted), and the open leaves with one of the open leaves tried. */
export function cardNumbers(v: ScienceView): { key: string; value: string; label: string }[] {
	const k = v.kpis;
	return [
		{ key: 'accepted', value: String(k.proofs), label: `Lean ${k.proofs === 1 ? 'proof' : 'proofs'} by the model accepted by prove2me` },
		{ key: 'open', value: `${k.openAccepted} of ${k.openTried}`, label: 'open leaves tried, with a proof accepted' },
	];
}

/** The card's numbers as HTML (the dl's children). */
export const cardNumbersHTML = (v: ScienceView) =>
	cardNumbers(v).map((n) => `<div data-number="${esc(n.key)}"><dt>${esc(n.label)}</dt><dd>${esc(n.value)}</dd></div>`).join('');
