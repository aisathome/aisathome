// The prove2me page's parts that read the gateway's records (step D6): the headline numbers, the "Small numbers" text,
// the submissions, the trials table and each trial's own list from trial 6 on, the home page's line and the /science/
// card. One HTML renderer each (every value escaped), for the build (from the build's history) and the browser (with
// the gateway's records, prove2me-records.ts) alike, so the two never say different things.
//
// SPDX-License-Identifier: Apache-2.0
import { day, hhmm, theoremURL } from './prove2me-current';
import { compact, leafRoleLabel, taskTableHTML } from './prove2me-live';
import { leafPlan, leafPlanHTML } from './prove2me-outlines';
import type { Kpis, ScienceView, SubRec, TrialDetail, TrialSum } from './prove2me-records';
import { shortSpan, tokTitleOf } from './prove2me-trials';

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const safe = (u: string | undefined | null) => (u && theoremURL.test(u) ? u : '');
const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);
const link = (url: string | null | undefined, text: string, attrs = '') => (safe(url) ? `<a href="${esc(url)}"${attrs}>${esc(text)}</a>` : `<span${attrs}>${esc(text)}</span>`);

/** A prove2me title without its TeX delimiters (the page has no TeX renderer). */
export const plainTitle = (t: string) => t.replace(/\$([^$]*)\$/g, '$1').trim();
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
		`<div data-kpi="accepted"><dt>${k.accepted === 1 ? 'proof' : 'proofs'} accepted by prove2me, of ${k.submitted} submitted</dt><dd>${k.accepted}</dd></div>`,
		`<div data-kpi="open"><dt>open leaves proved, of ${k.openTried} tried</dt><dd>${k.openProved}</dd></div>`,
		k.reused > 0 ? `<div data-kpi="reused"><dt>${k.reused === 1 ? 'open leaf' : 'open leaves'} closed by reuse of a proof already on prove2me</dt><dd>${k.reused}</dd></div>` : '',
		`<div data-kpi="runs"><dt>runs proved, of ${k.runs} in trials 1 to ${k.lastTrial}</dt><dd>${k.proved}</dd></div>`,
	].filter(Boolean);
	return { html: cells.join(''), n: cells.length };
}

/** "Small numbers": what the counts show and what they do not. The two early proofs (O04, F01) are history and named
 * so; every count comes from the view. `f01Tasks`: F01's task count in trial 5 (the build's). */
export function smallNumbersHTML(v: ScienceView, f01Tasks: number | undefined, idOf: (alias: string) => string): string {
	const k = v.kpis;
	const list = andList(k.openProvedLeaves.map((l) => named(l.leaf, l.alias)));
	const reuses = v.submissions.filter((s) => s.kind === 'reuse' && s.verdict === 'accepted');
	const o04 = named(idOf('O04'), 'O04'), f01 = named(idOf('F01'), 'F01');
	return esc(
		`Small numbers. The ${k.openProved} open leaves proved${list ? `, ${list},` : ''} show that the model can do it, not how often: ${o04} (trial 3) was a short ` +
			`proof of a leaf nobody had tried yet, and ${f01} (trial 5) took a new scaffold, two outlines and ${f01Tasks ?? 'many'} tasks after trials 3 and 4 had failed on it.` +
			(reuses.length ? ` ${andList(reuses.map((s) => named(s.leaf, s.alias)))}, closed by reuse, ${reuses.length === 1 ? 'is' : 'are'} not among them.` : '') +
			` On calibration leaves, which others had proved before, ${k.calibrationProved} of ${k.calibrationRuns} runs succeeded. All runs together took ` +
			`${k.hours.toFixed(1)} hours of model time${k.probeHours > 0 ? `, and the outline probes that chose leaves ${k.probeHours.toFixed(1)} more` : ''}.`,
	);
}

// ---- the submissions

const verdictWord: Record<string, string> = { accepted: 'Accepted', pending: 'Pending', rejected: 'Rejected', error: 'Not judged: an error' };

/** One submission as a row of the table. */
export function submissionRowHTML(s: SubRec): string {
	const r = s.reuses;
	const reuse = r?.theorem
		? `<span class="sub" data-reuses>${r.relation === 'part' ? 'part of' : 'same statement as'} ${link(r.url, r.theorem)}${r.author ? ` by ${esc(r.author)}` : ''}</span>`
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

// ---- the trials table

const tokCell = (computed: number, withCache: number, exactCount: boolean, attrs = '') =>
	`<td data-label="Tokens computed" class="num tok" data-tokens="${computed}" data-with-cache="${withCache}"${attrs} title="${esc(tokTitleOf(computed, withCache, exactCount))}">${exactCount ? '' : '≥ '}${esc(compact(computed))}</td>`;

/** A trial (or probe round) as a row of the trials table. A trial links its own list; a running one says so. */
export function trialRowHTML(t: TrialSum): string {
	const cnt = (c: { proved: number; runs: number }, dashIfNone = false) => (t.probe || (dashIfNone && !c.runs) ? '–' : `${c.proved} of ${c.runs}`);
	const name = t.probe
		? `<span>Trial ${t.number} probes</span>`
		: `<a href="#trial-${t.number}">Trial ${t.number}</a>${t.status === 'running' ? '<span class="sub">running</span>' : ''}`;
	const start = t.start ?? '';
	const dates = start ? `<time datetime="${esc(start)}">${esc(shortSpan({ start, end: t.end ?? start }))}${t.end ? '' : '–'}</time>` : '–';
	return (
		`<tr data-trial="${esc(t.label)}" data-source="${t.source}"${t.probe ? ' data-probes' : ''}${t.status !== 'finished' ? ` data-status="${t.status}"` : ''}>` +
		`<th scope="row">${name}</th>` +
		`<td data-label="Dates" class="nowrap">${dates}</td>` +
		`<td data-label="Scaffold">${esc(t.scaffold || '–')}</td>` +
		`<td data-label="Proved" class="num" data-cell="proved">${cnt(t.all)}</td>` +
		`<td data-label="Calibration" class="num" data-cell="calibration">${cnt(t.calibration, true)}</td>` +
		`<td data-label="Open" class="num" data-cell="open">${cnt(t.open)}</td>` +
		`<td data-label="Model hours" class="num" data-cell="hours">${(t.model_min / 60).toFixed(1)}</td>` +
		tokCell(t.computed, t.withCache, t.exact) +
		`</tr>`
	);
}

/** The trials table's All row. */
export function trialsFootHTML(v: ScienceView): string {
	const k = v.kpis;
	const open = v.trials.filter((t) => !t.probe).reduce((a, t) => ({ proved: a.proved + t.open.proved, runs: a.runs + t.open.runs }), { proved: 0, runs: 0 });
	const exactCount = v.trials.every((t) => t.exact);
	return (
		`<tr data-trial="all"><th scope="row">All</th><td></td><td></td>` +
		`<td data-label="Proved" class="num" data-cell="proved">${k.proved} of ${k.runs}</td>` +
		`<td data-label="Calibration" class="num" data-cell="calibration">${k.calibrationProved} of ${k.calibrationRuns}</td>` +
		`<td data-label="Open" class="num" data-cell="open">${open.proved} of ${open.runs}</td>` +
		`<td data-label="Model hours" class="num" data-cell="hours">${(k.hours + k.probeHours).toFixed(1)}</td>` +
		tokCell(k.computed, k.withCache, exactCount) +
		`</tr>`
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

/** A trial's list: its facts and text, its budget, a row per leaf and, under them, each leaf's outlines and tasks. */
export function trialDetailHTML(d: TrialDetail): string {
	const t = d.sum;
	const head = `Trial ${t.number} · ${t.start ? span({ start: t.start, end: t.status === 'running' ? null : t.end ?? t.start }) : 'being prepared'} · ${t.all.proved} of ${t.all.runs} proved`;
	const rows = d.leaves
		.map((l) => {
			const res = l.reuse ? 'closed' : l.proved ? 'proved' : /^(Running|Queued)/.test(l.summary) ? 'running' : 'failed';
			const outlines = new Set(l.detail.rows.map((r) => r.gen)).size;
			const mins = l.detail.rows.reduce((a, r) => a + (r.minutes ?? 0), 0);
			return (
				`<tr data-leaf="${esc(l.alias)}" data-leaf-id="${esc(l.id)}" data-outcome="${res}">` +
				`<th scope="row">${esc(l.id)}${l.alias && l.alias !== l.id ? `<span class="sub" data-alias title="The id the trials used">${esc(l.alias)}</span>` : ''}</th>` +
				`<td class="thm">${link(l.url, l.theorem)}${l.title ? `<span class="sub">${esc(plainTitle(l.title))}</span>` : ''}</td>` +
				`<td data-label="Kind">${esc(leafRoleLabel[l.role] ?? 'Open')}${l.earlier.length ? `<span class="sub">tried before in trial${l.earlier.length > 1 ? 's' : ''} ${esc(andList(l.earlier.map(String)))}</span>` : ''}</td>` +
				`<td data-label="Result" class="result ${res === 'proved' ? 'proved' : res === 'failed' ? 'failed' : ''}" data-cell="result">${esc(l.summary)}</td>` +
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
		`<details class="p2m-trial" id="trial-${t.number}" data-p2m-trial="${t.number}" data-source="${t.source}"><summary>${esc(head)}</summary>` +
		`<p data-trial-facts>${esc(trialFacts(d))}</p>` +
		(d.text.trim() ? `<div data-trial-text>${plainHTML(d.text)}</div>` : '') +
		(d.budget ? `<p class="caps">Budget per leaf: ${esc(d.budget)}.</p>` : '') +
		(rows
			? `<div class="p2m-wrap not-content"><table class="p2m-table p2m-leaves"><thead><tr><th scope="col">Leaf</th><th scope="col">Theorem</th><th scope="col">Kind</th><th scope="col">Result</th><th scope="col" class="num">Tasks</th><th scope="col" class="num">Model min</th><th scope="col" class="num">Tokens computed</th></tr></thead><tbody>${rows}</tbody></table></div>`
			: '') +
		(plans
			? `<div class="p2m-trial-tasks not-content" data-p2m-trial-tasks="${t.number}"><p class="caps">Each leaf’s outlines, newest first, with what became of every hole; under each, every task (outline, hole, try and assembly) with its counts, as in “Working on now”.</p>${plans}</div>`
			: '') +
		`</details>`
	);
}

// ---- the home page's line and the /science/ card

/** The home page's prove2me line (after its count): "proofs by M accepted on prove2me so far (...). Trial 8 is running." */
export function homeLine(v: ScienceView, model: string): string {
	const k = v.kpis;
	const cur = v.current ? ` Trial ${v.current.number} ${v.current.status === 'preparing' ? 'is being prepared' : 'is running'}.` : '';
	return (
		`${k.accepted === 1 ? 'proof' : 'proofs'} by ${model} accepted on prove2me so far (${k.openProved} of ${k.openTried} open leaves tried` +
		`${k.reused ? `; ${k.reused} more closed by reusing a proof already there` : ''}), where theorems from research papers are stated in Lean 4 and every proof is checked by machine.${cur}`
	);
}

/** The /science/ card's numbers. */
export function cardNumbers(v: ScienceView): { key: string; value: string; label: string }[] {
	const k = v.kpis;
	const c = v.current;
	return [
		{ key: 'accepted', value: String(k.accepted), label: `${k.accepted === 1 ? 'proof' : 'proofs'} accepted by prove2me, of ${k.submitted} submitted` },
		{ key: 'open', value: `${k.openProved} of ${k.openTried}`, label: `open leaves proved${k.reused ? `; ${k.reused} more closed by reuse` : ''}` },
		...(c ? [{ key: 'trial', value: `Trial ${c.number}`, label: c.status === 'preparing' ? 'being prepared' : c.start ? `running since ${day(c.start)}` : 'running' }] : []),
	];
}

/** The card's numbers as HTML (the dl's children). */
export const cardNumbersHTML = (v: ScienceView) =>
	cardNumbers(v).map((n) => `<div data-number="${esc(n.key)}"><dt>${esc(n.label)}</dt><dd>${esc(n.value)}</dd></div>`).join('');
