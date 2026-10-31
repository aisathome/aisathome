// The science page's data part: the numbers by type on top (the Scorecard the owner chose,
// 2026-10-31), then a short filterable, sortable list. Every number comes from science-stats.ts;
// rows and "How to read this" are science-render.ts's. The build renders it from the committed data
// (the page works without JavaScript); the browser renders it again from the gateway's when that
// is newer, so both use this one function.
import { compact, dayLabel, shortDay, type Science } from './science-model';
import { esc, howToRead, nowSection, plural, progress, rowsTable, updated } from './science-render';
import { insights, SMALL_SAMPLE, type Insights, type Rate, type TypeStats } from './science-stats';

const SMALL_TITLE = `Fewer than ${SMALL_SAMPLE}: a count, not a rate`;
const pct = (r: Rate) => Math.round((100 * r.x) / r.n);
const rateText = (r: Rate) => (r.n >= SMALL_SAMPLE ? `${r.x} of ${r.n} (${pct(r)}%)` : `${r.x} of ${r.n}`);
function rateCell(r: Rate) {
	if (!r.n) return '<span class="sci-none">none yet</span>';
	return r.n < SMALL_SAMPLE ? `<span class="sci-small" data-small title="${SMALL_TITLE}">${r.x} of ${r.n}†</span>` : rateText(r);
}
const rounds1 = (v: number) => v.toFixed(1);
const minutes = (v: number) => String(Math.round(v));
/** A value per merged PR: nothing merged says so; fewer than the small sample is marked. */
function perMerged(v: number | null, merged: number, fmt: (v: number) => string) {
	if (v === null) return '<span class="sci-none">none merged</span>';
	return merged < SMALL_SAMPLE ? `<span class="sci-small" data-small title="${merged} merged: a small sample">${fmt(v)}†</span>` : fmt(v);
}
const swatch = (slot: number) => `<span class="sci-swatch s${slot}" aria-hidden="true"></span>`;

function kpis(st: Insights) {
	const a = st.all;
	const list = [
		{ key: 'merged', value: String(a.merged), label: `merged, of ${a.opened} pull requests opened` },
		{ key: 'gpu', value: a.minutesPerMerged === null ? null : minutes(a.minutesPerMerged), label: 'GPU minutes per merged PR' },
		{ key: 'tokens', value: a.tokensPerMerged === null ? null : compact(a.tokensPerMerged), label: 'Qwen tokens per merged PR' },
	].filter((k) => k.value !== null);
	return `<dl class="sci-kpis not-content" data-science-kpis>${list.map((k) => `<div data-kpi="${k.key}"><dt>${esc(k.label)}</dt><dd>${esc(k.value)}</dd></div>`).join('')}</dl>`;
}

/** What the scorecard's columns sort on, kept on each row. */
export type ScoreKeys = {
	order: number;
	type: string;
	merged: number;
	opened: number;
	x: number;
	n: number;
	rounds: number | null;
	tokens: number | null;
	gpu: number | null;
};
export type ScoreKey = 'type' | 'merged' | 'first-review' | 'rounds' | 'tokens' | 'gpu';
export type ScoreSort = { key: ScoreKey; dir: 'asc' | 'desc' };
export const scoreFirstDir = (key: ScoreKey): ScoreSort['dir'] => (key === 'type' ? 'asc' : 'desc');

/** The tier of a first-review rate: 0 a real rate, 1 a marked count under the small sample, 2 none yet. */
const rateTier = (k: ScoreKeys) => (!k.n ? 2 : k.n < SMALL_SAMPLE ? 1 : 0);

/**
 * The scorecard's order. Merged sorts on the merged count, then opened. Approved at first review sorts
 * on the rate, but a marked count is no rate: tiers stay in that order in both directions (rates, then
 * marked counts by their count, then none yet). The means sort on their value, a small sample's too;
 * "none merged" stays last. Ties keep the page's order of types.
 */
export function compareScore(a: ScoreKeys, b: ScoreKeys, { key, dir }: ScoreSort) {
	const way = dir === 'asc' ? 1 : -1;
	let c = 0;
	if (key === 'type') c = way * a.type.localeCompare(b.type);
	else if (key === 'merged') c = way * (a.merged - b.merged || a.opened - b.opened);
	else if (key === 'first-review') {
		const tier = rateTier(a) - rateTier(b);
		if (tier) return tier;
		c = rateTier(a) === 0 ? way * (a.x / a.n - b.x / b.n || a.n - b.n) : way * (a.x - b.x || a.n - b.n);
	} else {
		const x = a[key], y = b[key];
		if (x === null || y === null) c = x === y ? 0 : x === null ? 1 : -1;
		else c = way * (x - y);
	}
	return c || a.order - b.order;
}

function scoreRow(r: TypeStats, order: number) {
	const keyed = order >= 0;
	const keys: ScoreKeys = { order, type: r.type, merged: r.merged, opened: r.opened, x: r.firstReview.x, n: r.firstReview.n, rounds: r.roundsPerMerged, tokens: r.tokensPerMerged, gpu: r.minutesPerMerged };
	const name = keyed ? `${swatch(r.slot)}<button type="button" data-filter="type" data-value="${esc(r.type)}" title="Show its rows below">${esc(r.type)}</button>` : esc(r.type);
	return `<tr data-insight-row="${esc(r.type)}"${keyed ? ` data-score="${esc(JSON.stringify(keys))}"` : ''}><th scope="row">${name}</th>
<td data-cell="merged">${r.merged} of ${r.opened}</td>
<td data-cell="first-review">${rateCell(r.firstReview)}</td>
<td data-cell="rounds">${perMerged(r.roundsPerMerged, r.merged, rounds1)}</td>
<td data-cell="tokens">${perMerged(r.tokensPerMerged, r.merged, compact)}</td>
<td data-cell="gpu">${perMerged(r.minutesPerMerged, r.merged, minutes)}</td></tr>`;
}

const scoreHeads: [ScoreKey, string][] = [
	['type', 'Type'],
	['merged', 'Merged, of opened'],
	['first-review', 'Approved at first review'],
	['rounds', 'Rounds per merged PR'],
	['tokens', 'Tokens per merged PR'],
	['gpu', 'GPU minutes per merged PR'],
];

/** The numbers of every type in one table. All types sits in the foot, so no sort moves it. */
function scorecard(st: Insights) {
	return `<div class="sci-wrap not-content"><table class="sci-score" data-science-scorecard>
<thead><tr>${scoreHeads.map(([key, label]) => `<th scope="col" data-score-col="${key}"><button type="button" data-score-sort="${key}">${label}<span class="sort-mark" aria-hidden="true"></span></button></th>`).join('')}</tr></thead>
<tbody>${st.rows.map((r, i) => scoreRow(r, i)).join('')}</tbody>
<tfoot>${scoreRow(st.all, -1)}</tfoot></table></div>`;
}

/** What the numbers cover, with the marks they carry. */
function notes(st: Insights, s: Science) {
	const spans = st.weeks;
	const from = s.prs.length ? shortDay(new Date(Math.min(...s.prs.map((p) => +p.opened)))) : '';
	const parts = [
		`${spans === 1 ? 'One week' : `${spans} weeks`} of data, from ${from}, so most of this is a small sample.`,
		`† Fewer than ${SMALL_SAMPLE}: read it as a count, not a rate.`,
		'“Per merged PR” divides all the work of a type by what got merged: failed attempts, closed pull requests, tasks without a PR and internal reviews count.',
		'Rounds are the fix and internal rounds of merged pull requests.',
	];
	if (st.reviewsLeftOut > 0) parts.push(`${plural(st.reviewsLeftOut, 'internal review')} left out of the GPU minutes: ${st.reviewsLeftOut === 1 ? 'its' : 'their'} time includes a stall.`);
	if (st.attemptsWithoutGpuTime > 0) parts.push(`The time of ${plural(st.attemptsWithoutGpuTime, 'early attempt')} was not recorded.`);
	return `<p class="sci-notes" data-science-notes>${parts.map(esc).join(' ')}</p>`;
}

// Merged PRs over time: a cumulative step line from the first PR to the snapshot date. The axis ends
// on a round number at or past the total, and the last point says the total, so the line can't look
// shorter than the headline.
function chart(s: Science) {
	const day = 86400000;
	const merges = s.prs.flatMap((p) => (p.merged ? [p] : [])).sort((a, b) => +a.merged! - +b.merged!);
	if (!merges.length) return '';
	const W = 640, H = 220, L = 40, R = 28, T = 16, B = 34;
	const start = Math.floor(Math.min(...s.prs.map((p) => +p.opened)) / day) * day;
	const last = Math.max(+merges[merges.length - 1].merged!, s.asOf ? +s.asOf + day : 0);
	const end = Math.max(Math.ceil(last / day) * day, start + day);
	const step = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000].find((n) => merges.length / n <= 5) ?? 2000;
	const top = Math.max(Math.ceil(merges.length / step) * step, 2);
	const x = (t: number) => L + ((t - start) / (end - start)) * (W - L - R);
	const y = (v: number) => T + (1 - v / top) * (H - T - B);
	let d = `M${x(start).toFixed(1)},${y(0).toFixed(1)}`;
	const points = merges.map((p, i) => {
		const px = x(+p.merged!);
		d += ` H${px.toFixed(1)} V${y(i + 1).toFixed(1)}`;
		return { x: px, y: y(i + 1), label: `#${p.number} merged ${dayLabel(p.merged!)} (${i + 1} in total)` };
	});
	d += ` H${x(end).toFixed(1)}`;
	const yTicks = Array.from({ length: Math.floor(top / step) + 1 }, (_, i) => ({ v: i * step, y: y(i * step) }));
	const days = Math.round((end - start) / day);
	const every = Math.max(1, Math.ceil(days / 6));
	const count = Math.floor(days / every) + 1;
	const xTicks = Array.from({ length: count }, (_, i) => {
		const t = start + i * every * day;
		return { x: x(t), label: shortDay(new Date(t)), anchor: i === 0 ? 'start' : i === count - 1 ? 'end' : 'middle', minor: i % 2 === 1 };
	});
	const end0 = points[points.length - 1];
	const desc = `${plural(merges.length, 'pull request')} merged into Tau Ceti: ${merges.map((p) => `#${p.number} on ${dayLabel(p.merged!)}`).join(', ')}.`;
	return `<figure class="sci-chart not-content" data-science-chart>
<figcaption>Merged PRs over time (cumulative)</figcaption>
<svg viewBox="0 0 ${W} ${H}" role="img" aria-labelledby="sci-chart-desc"><desc id="sci-chart-desc">${esc(desc)}</desc>
<g class="grid">${yTicks.map((t) => `<line x1="${L}" x2="${W - R}" y1="${t.y}" y2="${t.y}"></line><text x="${L - 8}" y="${t.y + 4}" text-anchor="end" data-y-tick="${t.v}">${t.v}</text>`).join('')}${xTicks
		.map((t) => `<text x="${t.x}" y="${H - 10}" text-anchor="${t.anchor}"${t.minor ? ' class="minor"' : ''}>${esc(t.label)}</text>`)
		.join('')}</g>
<path class="line" d="${d}"></path>
${points.map((p) => `<g class="pt" tabindex="0" data-merge-point data-tip="${esc(p.label)}" aria-label="${esc(p.label)}"><circle class="hit" cx="${p.x}" cy="${p.y}" r="14"></circle><circle class="dot" cx="${p.x}" cy="${p.y}" r="5"></circle></g>`).join('')}
<text class="total" x="${W - R + 4}" y="${(end0.y + 4).toFixed(1)}" data-merge-total>${merges.length}</text>
</svg></figure>`;
}

function insightsSection(st: Insights, s: Science) {
	return `<section data-science-insights><h2 id="insights">Results by type</h2>${kpis(st)}${scorecard(st)}${notes(st, s)}${chart(s)}</section>`;
}

/** The list, closed: chips for one type and one state; the page script applies them. */
function list(s: Science, st: Insights) {
	const chip = (filter: string, value: string, label: string) => `<button type="button" data-filter="${filter}" data-value="${esc(value)}" aria-pressed="${value === ''}">${esc(label)}</button>`;
	const states: [string, string][] = [['merged', 'Merged'], ['open', 'Open'], ['closed', 'Closed'], ['no-pr', 'No PR']];
	return `<h2 id="every-pr">Pull requests</h2>
<details class="sci-list not-content" data-science-list><summary>All ${plural(s.prs.length, 'pull request')} and ${plural(s.tasks.length, 'task')} without one</summary>
<div class="sci-filters" role="group" aria-label="Filter the list" data-science-filters>${chip('type', '', 'All types')}${st.rows.map((r) => chip('type', r.type, r.type)).join('')}<span class="sep"></span>${chip('state', '', 'Any state')}${states.map(([v, l]) => chip('state', v, l)).join('')}</div>
<p class="sci-notes" data-list-count></p>
${rowsTable(s)}</details>`;
}

/** Everything after the introduction. */
export function renderScience(s: Science, at: Date) {
	const st = insights(s);
	return [
		updated(s),
		s.prs.length ? insightsSection(st, s) : '',
		nowSection(s.now, at),
		list(s, st),
		progress(s),
		howToRead(s),
	]
		.filter(Boolean)
		.join('\n');
}
