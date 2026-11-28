// The proved theorems pages in the browser (pages/science/prove2me/theorems/): the gateway's public records read on load
// and every minute while the tab is visible, the page's parts drawn again with the build's renderers
// (prove2me-theorems.ts). When the gateway does not answer, the page keeps what it shows.
// SPDX-License-Identifier: Apache-2.0
import { mountAcceptedChart } from '../data/prove2me-chart';
import { fetchRecords, getJSON, type History, type Records } from '../data/prove2me-records';
import { proofLeaf, proofTasksHTML } from '../data/prove2me-proof';
import {
	costsHTML, ctaHTML, headlineHTML, listCountText, selectionText, theoremFactsHTML, theoremKpisHTML, theoremRowsHTML, theoremsView, trialBarRowsHTML,
	trialBarsSVG, updatedText, type TrialBar, type TheoremsView,
} from '../data/prove2me-theorems';

/** What the build left in the page for the script: the history (trials 1 to 5), the leaves with a page, the model's name,
 * this page's leaf and, for a proof, the trial that proved it (its tasks). */
type Known = { history: History; pages: string[]; model?: string; leaf?: string; trial?: number };

/** A proof page's tasks from the records (prove2me-proof.ts), the reader's open or closed table kept; the build's copy
 * stays when the records have no list of the trial (trials 1 to 5 are the build's). */
function drawTasks(live: Records, known: Known) {
	const box = document.querySelector<HTMLElement>('[data-th-tasks]');
	if (!box || !known.leaf || !known.trial) return;
	const l = proofLeaf(known.history, live, [], { leaf: known.leaf, trial: known.trial });
	if (!l) return;
	const open = box.querySelector<HTMLDetailsElement>('details[data-leaf-tasks]')?.open;
	box.innerHTML = proofTasksHTML(l); // every value escaped (prove2me-live.ts, prove2me-outlines.ts)
	box.dataset.source = 'live';
	const t = box.querySelector<HTMLDetailsElement>('details[data-leaf-tasks]');
	if (t && open !== undefined) t.open = open;
}

/** The per-trial chart, drawn at its real width; `update` with a newer view. */
function mountBars(bars: TrialBar[]) {
	const fig = document.querySelector<HTMLElement>('[data-th-bars]');
	const box = fig?.querySelector<HTMLElement>('[data-th-bars-svg]');
	let data = bars;
	const draw = () => {
		if (box && data.length && box.clientWidth > 0) box.innerHTML = trialBarsSVG(data, box.clientWidth);
	};
	draw();
	let frame = 0;
	window.addEventListener('resize', () => {
		cancelAnimationFrame(frame);
		frame = requestAnimationFrame(draw);
	});
	return (next: TrialBar[]) => {
		if (!fig) return;
		data = next;
		fig.hidden = !next.length;
		fig.dataset.source = 'live';
		draw();
		const rows = fig.querySelector<HTMLElement>('[data-th-bars-rows]');
		if (rows) rows.innerHTML = trialBarRowsHTML(next);
	};
}

function draw(v: TheoremsView, known: Known) {
	const set = (sel: string, html: string) => {
		const e = document.querySelector<HTMLElement>(sel);
		if (e) {
			e.innerHTML = html; // every value escaped (prove2me-theorems.ts)
			e.dataset.source = 'live';
		}
	};
	const k = theoremKpisHTML(v);
	document.querySelector<HTMLElement>('[data-th-kpis]')?.style.setProperty('--kpis', String(k.n));
	set('[data-th-kpis]', k.html);
	if (known.model) set('[data-th-headline]', headlineHTML(v, known.model));
	const sel = document.querySelector<HTMLElement>('[data-th-selection]');
	if (sel) sel.textContent = selectionText(v);
	set('[data-th-costs]', costsHTML(v));
	set('[data-th-rows]', theoremRowsHTML(v, new Set(known.pages)));
	document.querySelectorAll<HTMLElement>('[data-th-count]').forEach((e) => (e.textContent = listCountText(v)));
	if (known.leaf) {
		const t = v.theorems.find((x) => x.leaf === known.leaf);
		if (t) {
			set('[data-th-facts]', theoremFactsHTML(t));
			set('[data-th-cta]', ctaHTML(t));
		}
	}
	const up = document.querySelector<HTMLElement>('[data-th-updated]');
	if (up) {
		up.textContent = updatedText(v);
		up.dataset.source = 'live';
	}
}

/** Start the live refresh (a page without the build's data or without fetch keeps the build's copy). */
export function liveTheorems() {
	let known: Known | null = null;
	try {
		known = JSON.parse(document.querySelector('[data-th-known]')?.textContent || 'null') as Known | null;
	} catch {}
	if (!known || !window.fetch) return;
	const k = known;
	const chart = mountAcceptedChart(document.querySelector<HTMLElement>('[data-p2m-chart]'));
	let bars: ((b: TrialBar[]) => void) | null = null;
	// both charts from the view the numbers come from: its series and its bars (one computation, theoremsView)
	const charts = (v: TheoremsView) => {
		if (!document.querySelector('[data-p2m-chart], [data-th-bars]')) return;
		chart.show(v.series);
		bars ??= mountBars(v.perTrial);
		bars(v.perTrial);
	};
	const load = async () => {
		const live = await fetchRecords(getJSON).catch(() => null);
		if (!live) return;
		try {
			const v = theoremsView(k.history, live);
			draw(v, k);
			charts(v);
			drawTasks(live, k);
		} catch {
			// keep what the page shows
		}
	};
	// the build's charts at their real width before the first read
	const fig = document.querySelector<HTMLElement>('[data-th-bars]');
	if (fig) {
		try {
			const rows = [...fig.querySelectorAll<HTMLTableRowElement>('[data-th-bars-rows] tr')].map((r) => [...r.cells].map((c) => Number(c.textContent)));
			bars = mountBars(rows.map(([trial, tried, accepted]) => ({ trial, tried, accepted })));
		} catch {}
	}
	load();
	setInterval(() => document.visibilityState === 'visible' && load(), 60000);
}
