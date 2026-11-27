// The proved theorems pages in the browser (pages/science/prove2me/theorems/): the gateway's public records read on load
// and every minute while the tab is visible, the page's parts drawn again with the build's renderers
// (prove2me-theorems.ts). When the gateway does not answer, the page keeps what it shows.
// SPDX-License-Identifier: Apache-2.0
import { fetchRecords, getJSON, type History } from '../data/prove2me-records';
import { costsHTML, selectionText, theoremFactsHTML, theoremKpisHTML, theoremRowsHTML, theoremsView, updatedText, type TheoremsView } from '../data/prove2me-theorems';

/** What the build left in the page for the script: the history (trials 1 to 5), the leaves with a page, this page's leaf. */
type Known = { history: History; pages: string[]; leaf?: string };

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
	const sel = document.querySelector<HTMLElement>('[data-th-selection]');
	if (sel) sel.textContent = selectionText(v);
	set('[data-th-costs]', costsHTML(v));
	set('[data-th-rows]', theoremRowsHTML(v, new Set(known.pages)));
	document.querySelectorAll<HTMLElement>('[data-th-count]').forEach((e) => (e.textContent = String(v.theorems.length)));
	if (known.leaf) {
		const t = v.theorems.find((x) => x.leaf === known.leaf);
		if (t) set('[data-th-facts]', theoremFactsHTML(t));
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
	const load = async () => {
		const live = await fetchRecords(getJSON).catch(() => null);
		if (!live) return;
		try {
			draw(theoremsView(k.history, live), k);
		} catch {
			// keep what the page shows
		}
	};
	load();
	setInterval(() => document.visibilityState === 'visible' && load(), 60000);
}
