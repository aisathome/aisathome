// The home page's prove2me chart: proofs prove2me accepted, counted up over time, with the start of each trial marked.
// Pure: an SVG string and table rows from a ScienceView, so the build draws it (the page works without JavaScript) and
// the browser draws it again at its real width and with the gateway's newer records. The look follows the network
// page's charts (pressure-chart.ts): gridlines at 0, half and the top, the first and last date under the plot, a title
// per point. Real dates only: the line stops at the data's time, nothing is projected.
// SPDX-License-Identifier: Apache-2.0
import type { ScienceView } from './prove2me-records';
import { plainTitle } from './prove2me-view';
import { nice } from './pressure-chart';

export interface Accepted {
	/** When prove2me accepted it (its verdict), or when it was submitted when the verdict has no time. */
	at: string;
	trial: number;
	kind: 'proof' | 'reuse';
	title: string;
	url: string;
}
export interface AcceptedSeries {
	/** From the first trial's start to the data's time. */
	start: string;
	end: string;
	/** Oldest first. */
	accepted: Accepted[];
	/** Each trial's start (outline probes left out); trials that start on the same day share a mark. */
	trials: { number: number; start: string }[];
}

const DAY = 86400000;
const ms = (iso: string) => Date.parse(iso.length === 10 ? `${iso}T00:00:00Z` : iso);
const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
/** "25 Nov" in UTC. */
export const shortDay = (iso: string) => {
	const d = new Date(ms(iso));
	return `${d.getUTCDate()} ${months[d.getUTCMonth()]}`;
};
/** "25 Nov 2026, 07:41 UTC". */
const when = (iso: string) => {
	const d = new Date(ms(iso));
	return `${shortDay(iso)} ${d.getUTCFullYear()}, ${String(d.getUTCHours()).padStart(2, '0')}:${String(d.getUTCMinutes()).padStart(2, '0')} UTC`;
};

/** The chart's data from a view: its accepted submissions and its trials. Null when there is nothing to draw. */
export function acceptedSeries(v: ScienceView): AcceptedSeries | null {
	const accepted = v.submissions
		.filter((s) => s.verdict === 'accepted')
		.map((s) => ({ at: s.verdict_at || s.submitted_at, trial: s.trial, kind: s.kind, title: plainTitle(s.title), url: s.url }))
		.filter((a) => Number.isFinite(ms(a.at)))
		.sort((a, b) => ms(a.at) - ms(b.at));
	const trials = v.trials
		.filter((t) => !t.probe && t.start && Number.isFinite(ms(t.start)))
		.map((t) => ({ number: t.number, start: t.start! }))
		.sort((a, b) => ms(a.start) - ms(b.start) || a.number - b.number);
	if (!accepted.length || !trials.length) return null;
	const start = trials[0].start;
	const last = accepted[accepted.length - 1].at;
	const end = Number.isFinite(ms(v.updated)) && ms(v.updated) > ms(last) ? v.updated : last;
	return { start, end, accepted, trials };
}

/** What the chart says, for its aria-label and caption. */
export function acceptedSummary(s: AcceptedSeries): string {
	const n = s.accepted.length;
	const reused = s.accepted.filter((a) => a.kind === 'reuse').length;
	const first = s.trials[0].number, lastTrial = s.trials[s.trials.length - 1].number;
	return `${n} ${n === 1 ? 'proof' : 'proofs'} accepted by prove2me from ${shortDay(s.accepted[0].at)} to ${shortDay(s.end)}` +
		`${reused ? ` (${reused} of them by reusing a proof already there)` : ''}, over trials ${first} to ${lastTrial}, which started ${shortDay(s.start)}.`;
}

const H = 200, L = 30, R = 30, T = 22, B = 24;

/** The chart as SVG markup, `W` pixels wide (the browser passes its real width, so text is never stretched). */
export function acceptedChartSVG(s: AcceptedSeries, W = 640): string {
	W = Math.max(Math.round(W), 240);
	const t0 = ms(s.start), t1 = Math.max(ms(s.end), t0 + DAY);
	const top = nice(s.accepted.length);
	const x = (iso: string) => L + ((W - L - R) * (ms(iso) - t0)) / (t1 - t0);
	const y = (v: number) => T + (H - T - B) * (1 - v / top);
	const out: string[] = [];
	for (const f of [0, 0.5, 1]) {
		const yy = y(top * f);
		out.push(`<line x1="${L}" x2="${W - R}" y1="${yy}" y2="${yy}" class="${f ? 'grid' : 'base'}"/>`,
			`<text x="${L - 6}" y="${yy + 4}" class="tick" text-anchor="end">${Math.round(top * f)}</text>`);
	}
	out.push(`<text x="${L}" y="${H - 6}" class="tick">${shortDay(s.start)}</text>`,
		`<text x="${W - R}" y="${H - 6}" class="tick" text-anchor="end">${shortDay(s.end)}</text>`);
	// Trial starts: a dashed line each, numbered above the plot; numbers that would overlap are left out (the table has
	// every trial), trials on the same day share one line.
	const marks = new Map<number, number[]>();
	for (const tr of s.trials) {
		const xx = Math.round(x(tr.start));
		marks.set(xx, [...(marks.get(xx) ?? []), tr.number]);
	}
	let free = -Infinity;
	for (const [xx, ns] of marks) {
		out.push(`<line x1="${xx}" x2="${xx}" y1="${T}" y2="${H - B}" class="trial"><title>Trial ${ns.join(' and ')} started ${shortDay(s.trials.find((t) => t.number === ns[0])!.start)}</title></line>`);
		const label = ns.join('·');
		const w = label.length * 6.5;
		if (xx - w / 2 >= free && xx + w / 2 <= W) {
			out.push(`<text x="${xx}" y="${T - 8}" class="tick trial-n" text-anchor="middle">${label}</text>`);
			free = xx + w / 2 + 4;
		}
	}
	// The count as steps: flat until prove2me accepts the next one.
	let n = 0;
	const pts = [`${L},${y(0)}`];
	const dots: string[] = [];
	for (const a of s.accepted) {
		const xx = x(a.at).toFixed(1);
		pts.push(`${xx},${y(n)}`);
		n += 1;
		pts.push(`${xx},${y(n)}`);
		dots.push(`<circle cx="${xx}" cy="${y(n)}" r="3.5" class="${a.kind === 'reuse' ? 'pt reuse' : 'pt'}"><title>${esc(`${n}. ${when(a.at)}, trial ${a.trial}: ${a.title}${a.kind === 'reuse' ? ' (closed by reusing a proof already there)' : ''}`)}</title></circle>`);
	}
	pts.push(`${(W - R).toFixed(1)},${y(n)}`);
	out.push(`<polyline points="${pts.join(' ')}" class="step"/>`, ...dots,
		`<text x="${W - R + 4}" y="${y(n) + 4}" class="tick end-n">${n}</text>`);
	return `<svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="${esc(acceptedSummary(s))}" xmlns="http://www.w3.org/2000/svg">${out.join('')}</svg>`;
}

/** The table under the chart, newest first: when, trial, the lemma (its prove2me page), how, and the count so far. */
export function acceptedRowsHTML(s: AcceptedSeries): string {
	return s.accepted
		.map((a, i) => ({ a, n: i + 1 }))
		.reverse()
		.map(({ a, n }) => `<tr><td>${esc(shortDay(a.at))}</td><td class="num">${a.trial}</td>` +
			`<td class="lemma">${a.url ? `<a href="${esc(a.url)}">${esc(a.title)}</a>` : esc(a.title)}</td>` +
			`<td>${a.kind === 'reuse' ? 'reuse' : 'proof'}</td><td class="num">${n}</td></tr>`)
		.join('');
}

/** In the browser: draws the chart in a figure of components/AcceptedChart.astro again at its real width (the build's
 * copy is scaled) and on every resize; `update` redraws it and its table from a newer view (the gateway's records). */
export function mountAcceptedChart(fig: HTMLElement | null): { update: (v: ScienceView) => void } {
	const box = fig?.querySelector<HTMLElement>('[data-p2m-chart-svg]');
	const rows = fig?.querySelector<HTMLElement>('[data-p2m-chart-rows]');
	let series: AcceptedSeries | null = null;
	try {
		series = JSON.parse(fig?.querySelector('[data-p2m-chart-series]')?.textContent || 'null');
	} catch {
		series = null;
	}
	const draw = () => {
		if (box && series && box.clientWidth > 0) box.innerHTML = acceptedChartSVG(series, box.clientWidth);
	};
	draw();
	let frame = 0;
	window.addEventListener('resize', () => {
		cancelAnimationFrame(frame);
		frame = requestAnimationFrame(draw);
	});
	return {
		update(v) {
			const s = acceptedSeries(v);
			if (!fig || !s || !box || !rows) return;
			series = s;
			draw();
			rows.innerHTML = acceptedRowsHTML(s); // every value escaped
			fig.dataset.source = 'live';
		},
	};
}
