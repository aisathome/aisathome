// Small inline-SVG charts for the network page's "Pressure" section, drawn like the page's daily charts (the same
// geometry, classes and colours): gridlines at 0, half and the top, the first and last bucket under the plot, a hover
// target per bucket with a title. A point with no value leaves a gap; a line never joins across one.
// SPDX-License-Identifier: Apache-2.0

const NS = 'http://www.w3.org/2000/svg';
const H = 200, L = 52, R = 8, T = 10, B = 24;

export interface Point { label: string; tip: string; values: Record<string, number | null> }

function el(name: string, attrs: Record<string, string | number>, parent?: Element): SVGElement {
	const e = document.createElementNS(NS, name);
	for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
	if (parent) parent.append(e);
	return e;
}

/** A round top for the axis, just above the highest value. */
export function nice(max: number): number {
	if (!(max > 0)) return 1;
	const p = 10 ** Math.floor(Math.log10(max));
	return [1, 1.5, 2, 3, 4, 5, 6, 8, 10].map((m) => m * p).find((v) => v >= max * (1 - 1e-9)) ?? max;
}

function frame(svg: SVGSVGElement, pts: Point[], top: number, fmt: (v: number) => string, label: string): number {
	svg.replaceChildren();
	const W = Math.max(Math.round(svg.getBoundingClientRect().width), 240); // drawn at its size: text is never stretched
	svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
	svg.setAttribute('aria-label', label);
	for (const f of [0, 0.5, 1]) {
		const y = T + (H - T - B) * (1 - f);
		el('line', { x1: L, x2: W - R, y1: y, y2: y, class: f ? 'grid' : 'base' }, svg);
		el('text', { x: L - 6, y: y + 4, class: 'tick', 'text-anchor': 'end' }, svg).textContent = fmt(top * f);
	}
	el('text', { x: L, y: H - 6, class: 'tick' }, svg).textContent = pts[0].label;
	el('text', { x: W - R, y: H - 6, class: 'tick', 'text-anchor': 'end' }, svg).textContent = pts[pts.length - 1].label;
	return W;
}

const yOf = (v: number, top: number) => T + (H - T - B) * (1 - Math.min(v, top) / top);

function hit(svg: SVGSVGElement, x: number, w: number, tip: string): SVGElement {
	const g = el('g', { class: 'day' }, svg);
	el('rect', { x, y: T, width: w, height: H - T - B, class: 'hit' }, g);
	el('title', {}, g).textContent = tip;
	return g;
}

/** One bar per point (series `key`), from the baseline. `top` fixes the axis (100% for a share). */
export function barChart(svg: SVGSVGElement, pts: Point[], key: string, fmt: (v: number) => string, label: string, top?: number) {
	const vals = pts.map((p) => p.values[key]).filter((v): v is number => v !== null);
	const t = top ?? nice(Math.max(...vals, 0));
	const W = frame(svg, pts, t, fmt, label);
	const step = (W - L - R) / pts.length, w = Math.max(step - 2, 1);
	pts.forEach((p, i) => {
		const g = hit(svg, L + i * step, step, p.tip);
		const v = p.values[key];
		if (v === null || v <= 0) return;
		const y = yOf(v, t);
		g.prepend(el('rect', { x: L + i * step + 1, y, width: w, height: Math.max(yOf(0, t) - y, 0.5), class: 'bar s-' + key }));
	});
}

/** One line per series, broken where a point has no value; a lone point is a dot. `top` fixes the axis (100% for shares). */
export function lineChart(svg: SVGSVGElement, pts: Point[], series: string[], fmt: (v: number) => string, label: string, fixedTop?: number) {
	const vals = pts.flatMap((p) => series.map((k) => p.values[k])).filter((v): v is number => v !== null);
	const top = fixedTop ?? nice(Math.max(...vals, 0));
	const W = frame(svg, pts, top, fmt, label);
	const step = (W - L - R) / pts.length;
	const x = (i: number) => L + step * (i + 0.5);
	for (const k of series) {
		let run: string[] = [];
		const flush = (i: number) => {
			if (run.length > 1) el('polyline', { points: run.join(' '), class: 'line s-' + k }, svg);
			else if (run.length === 1) el('circle', { cx: x(i - 1), cy: yOf(pts[i - 1].values[k]!, top), r: 2, class: 'point s-' + k }, svg);
			run = [];
		};
		pts.forEach((p, i) => {
			const v = p.values[k];
			if (v === null) flush(i);
			else run.push(`${x(i)},${yOf(v, top)}`);
		});
		flush(pts.length);
	}
	pts.forEach((p, i) => {
		const g = hit(svg, L + i * step, step, p.tip);
		for (const k of series) {
			const v = p.values[k];
			if (v !== null) g.prepend(el('circle', { cx: x(i), cy: yOf(v, top), r: 4, class: 'dot s-' + k }));
		}
	});
}
