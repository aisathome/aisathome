// A share card (Open Graph image, 1200 x 630 PNG) drawn at build time, without a browser or a native image library:
// the site's own fonts (the @fontsource WOFF2 files, read with fontkitten) turned into outlines, filled by a
// small scanline rasterizer with anti-aliasing, and written as a PNG with node:zlib. Used for the pages of the proved
// theorems (pages/science/prove2me/theorems/), whose cards carry each theorem's own title and numbers. The site-wide
// card (public/og.png) is still drawn by scripts/share-image.mjs.
//
// Build-only: it reads files from node_modules. No kerning or shaping: Latin text, as the cards' copy is; a character
// none of the fonts has is drawn as '?'.
//
// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { deflateSync } from 'node:zlib';
import { create, type Font } from 'fontkitten';

export const CARD_W = 1200;
export const CARD_H = 630;

type RGB = readonly [number, number, number];
const hex = (h: string): RGB => [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)];
// The dark theme of the site-wide card (scripts/share-image.mjs): background, text, muted text, the accent orange.
export const colors = { bg: hex('#15110e'), glow: hex('#22180f'), text: hex('#fff8f0'), muted: hex('#d8cdc1'), dim: hex('#a89c90'), accent: hex('#ff9842'), logo: hex('#c2410c'), line: hex('#4b423a') };

// ---- fonts

// The site's three families as static fonts (the @fontsource packages: fontkitten cannot set a variable font's weight
// in a WOFF2 file), one file per weight.
const require = createRequire(import.meta.url);
const families = { display: 'big-shoulders-display', sans: 'public-sans', mono: 'jetbrains-mono' } as const;
const cache = new Map<string, Font>();
/** A font of the site at a weight (100 to 800 in steps of 100). */
export function font(kind: keyof typeof families, weight: number): Font {
	const key = `${kind}:${weight}`;
	let f = cache.get(key);
	if (!f) {
		const name = families[kind];
		f = create(readFileSync(require.resolve(`@fontsource/${name}/files/${name}-latin-${weight}-normal.woff2`))) as Font;
		cache.set(key, f);
	}
	return f;
}

// ---- the canvas

/** An RGB canvas of the card's size, filled with outlines (non-zero winding, 4 sub-scanlines, exact horizontal cover). */
export class Card {
	readonly w = CARD_W;
	readonly h = CARD_H;
	readonly px = new Float32Array(CARD_W * CARD_H * 3);

	constructor(bg: RGB = colors.bg) {
		for (let i = 0; i < this.w * this.h; i++) this.px.set(bg, i * 3);
	}

	/** A soft radial glow (the site-wide card's background). */
	glow(cx: number, cy: number, r: number, c: RGB) {
		for (let y = 0; y < this.h; y++)
			for (let x = 0; x < this.w; x++) {
				const t = Math.max(0, 1 - Math.hypot(x - cx, y - cy) / r);
				if (t > 0) this.blend(x, y, c, t * t);
			}
	}

	private blend(x: number, y: number, c: RGB, a: number) {
		const i = (y * this.w + x) * 3;
		for (let k = 0; k < 3; k++) this.px[i + k] += (c[k] - this.px[i + k]) * a;
	}

	rect(x: number, y: number, w: number, h: number, c: RGB, r = 0) {
		this.fill([roundRect(x, y, w, h, r)], c);
	}

	/** Fill closed polygons (lists of x, y pairs) with a colour. */
	fill(polys: number[][], c: RGB) {
		const S = 4;
		let minY = Infinity, maxY = -Infinity, minX = Infinity, maxX = -Infinity;
		const edges: { x0: number; y0: number; x1: number; y1: number; dir: number }[] = [];
		for (const p of polys)
			for (let i = 0; i < p.length; i += 2) {
				const x0 = p[i], y0 = p[i + 1], x1 = p[(i + 2) % p.length], y1 = p[(i + 3) % p.length];
				minY = Math.min(minY, y0); maxY = Math.max(maxY, y0); minX = Math.min(minX, x0); maxX = Math.max(maxX, x0);
				if (y0 === y1) continue;
				edges.push(y0 < y1 ? { x0, y0, x1, y1, dir: 1 } : { x0: x1, y0: y1, x1: x0, y1: y0, dir: -1 });
			}
		if (!edges.length) return;
		const yA = Math.max(0, Math.floor(minY)), yB = Math.min(this.h - 1, Math.ceil(maxY));
		const xA = Math.max(0, Math.floor(minX)), xB = Math.min(this.w, Math.ceil(maxX) + 1);
		const cover = new Float32Array(this.w + 1);
		const xs: { x: number; dir: number }[] = [];
		for (let y = yA; y <= yB; y++) {
			cover.fill(0, xA, xB + 1);
			for (let s = 0; s < S; s++) {
				const sy = y + (s + 0.5) / S;
				xs.length = 0;
				for (const e of edges) if (sy >= e.y0 && sy < e.y1) xs.push({ x: e.x0 + ((sy - e.y0) / (e.y1 - e.y0)) * (e.x1 - e.x0), dir: e.dir });
				xs.sort((a, b) => a.x - b.x);
				let wind = 0;
				for (let k = 0; k < xs.length - 1; k++) {
					wind += xs[k].dir;
					if (wind !== 0) span(cover, Math.max(xs[k].x, 0), Math.min(xs[k + 1].x, this.w), 1 / S);
				}
			}
			for (let x = xA; x < xB; x++) if (cover[x] > 0) this.blend(x, y, c, Math.min(1, cover[x]));
		}
	}

	/** Text on a baseline; returns its width. */
	text(str: string, f: Font, size: number, x: number, baseline: number, c: RGB, spacing = 0): number {
		const polys: number[][] = [];
		const w = layout(str, f, size, spacing, (g, gx) => polys.push(...outline(g.path.commands, gx + x, baseline, size / f.unitsPerEm)));
		this.fill(polys, c);
		return w;
	}

	png(): Buffer {
		const raw = Buffer.alloc((this.w * 3 + 1) * this.h);
		for (let y = 0; y < this.h; y++) {
			const row = y * (this.w * 3 + 1);
			raw[row] = 0;
			for (let i = 0; i < this.w * 3; i++) raw[row + 1 + i] = Math.max(0, Math.min(255, Math.round(this.px[y * this.w * 3 + i])));
		}
		const ihdr = Buffer.alloc(13);
		ihdr.writeUInt32BE(this.w, 0);
		ihdr.writeUInt32BE(this.h, 4);
		ihdr.set([8, 2, 0, 0, 0], 8); // 8 bits, RGB
		return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw, { level: 9 })), chunk('IEND', Buffer.alloc(0))]);
	}
}

/** Add a horizontal span's exact cover to a row (weight: one sub-scanline's share). */
function span(cover: Float32Array, a: number, b: number, weight: number) {
	if (b <= a) return;
	const ia = Math.floor(a), ib = Math.floor(b);
	if (ia === ib) {
		cover[ia] += (b - a) * weight;
		return;
	}
	cover[ia] += (ia + 1 - a) * weight;
	for (let i = ia + 1; i < ib; i++) cover[i] += weight;
	if (ib < cover.length) cover[ib] += (b - ib) * weight;
}

function roundRect(x: number, y: number, w: number, h: number, r: number): number[] {
	if (r <= 0) return [x, y, x + w, y, x + w, y + h, x, y + h];
	const pts: number[] = [];
	const corner = (cx: number, cy: number, a0: number) => {
		for (let i = 0; i <= 8; i++) {
			const a = a0 + (i / 8) * (Math.PI / 2);
			pts.push(cx + r * Math.cos(a), cy + r * Math.sin(a));
		}
	};
	corner(x + w - r, y + r, -Math.PI / 2);
	corner(x + w - r, y + h - r, 0);
	corner(x + r, y + h - r, Math.PI / 2);
	corner(x + r, y + r, Math.PI);
	return pts;
}

/** A glyph's outline as polygons, its curves flattened; font units scaled by k, y up in the font and down on the card. */
function outline(cmds: { command: string; args: number[] }[], ox: number, oy: number, k: number): number[][] {
	const polys: number[][] = [];
	let cur: number[] = [];
	let px = 0, py = 0;
	const P = (x: number, y: number) => cur.push(ox + x * k, oy - y * k);
	const N = 10;
	for (const { command, args: a } of cmds) {
		if (command === 'moveTo') {
			if (cur.length > 4) polys.push(cur);
			cur = [];
			P(a[0], a[1]);
			[px, py] = a;
		} else if (command === 'lineTo') {
			P(a[0], a[1]);
			[px, py] = a;
		} else if (command === 'quadraticCurveTo') {
			for (let i = 1; i <= N; i++) {
				const t = i / N, u = 1 - t;
				P(u * u * px + 2 * u * t * a[0] + t * t * a[2], u * u * py + 2 * u * t * a[1] + t * t * a[3]);
			}
			[px, py] = [a[2], a[3]];
		} else if (command === 'bezierCurveTo') {
			for (let i = 1; i <= N; i++) {
				const t = i / N, u = 1 - t;
				P(u * u * u * px + 3 * u * u * t * a[0] + 3 * u * t * t * a[2] + t * t * t * a[4], u * u * u * py + 3 * u * u * t * a[1] + 3 * u * t * t * a[3] + t * t * t * a[5]);
			}
			[px, py] = [a[4], a[5]];
		} else if (command === 'closePath') {
			if (cur.length > 4) polys.push(cur);
			cur = [];
		}
	}
	if (cur.length > 4) polys.push(cur);
	return polys;
}

/** Walk a string's glyphs (a missing character as '?'); returns the width at `size`. */
function layout(str: string, f: Font, size: number, spacing: number, each?: (g: ReturnType<Font['glyphForCodePoint']>, x: number) => void): number {
	const k = size / f.unitsPerEm;
	let x = 0;
	for (const ch of str) {
		const cp = ch.codePointAt(0)!;
		const g = f.glyphForCodePoint(f.hasGlyphForCodePoint(cp) ? cp : 0x3f);
		each?.(g, x);
		x += g.advanceWidth * k + spacing;
	}
	return x;
}

/** A string's width at `size`. */
export const measure = (str: string, f: Font, size: number, spacing = 0) => layout(str, f, size, spacing);

/** Break text into at most `lines` lines no wider than `width`; the last one ends in '…' when the text does not fit. A word
 * longer than a line (a Lean name) is broken at '.' or '_' where it can, else anywhere. */
export function wrap(str: string, f: Font, size: number, width: number, lines: number): string[] {
	const fits = (s: string) => measure(s, f, size) <= width;
	const words = str.split(/\s+/).filter(Boolean).flatMap((w) => (fits(w) ? [w] : w.split(/(?<=[._])/)));
	const out: string[] = [];
	let line = '';
	for (const w of words) {
		const glue = line && !/[._]$/.test(line) ? ' ' : '';
		if (!line || fits(line + glue + w)) {
			line = line + (line ? glue : '') + w;
			while (!fits(line)) {
				// one piece still too long: cut it where it overflows
				let n = line.length - 1;
				while (n > 1 && !fits(line.slice(0, n))) n--;
				out.push(line.slice(0, n));
				line = line.slice(n);
			}
			continue;
		}
		out.push(line);
		line = w;
	}
	if (line) out.push(line);
	if (out.length <= lines) return out;
	const kept = out.slice(0, lines);
	let last = kept[lines - 1];
	while (last && !fits(`${last}…`)) last = last.slice(0, -1);
	kept[lines - 1] = `${last.trimEnd()}…`;
	return kept;
}

/** Shorten one line to `width`, ending in '…'. */
export function clip(str: string, f: Font, size: number, width: number): string {
	if (measure(str, f, size) <= width) return str;
	let s = str;
	while (s && measure(`${s}…`, f, size) > width) s = s.slice(0, -1);
	return `${s}…`;
}

// ---- PNG

const crcTable = (() => {
	const t = new Uint32Array(256);
	for (let n = 0; n < 256; n++) {
		let c = n;
		for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
		t[n] = c >>> 0;
	}
	return t;
})();
function crc32(b: Buffer): number {
	let c = 0xffffffff;
	for (const x of b) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8);
	return (c ^ 0xffffffff) >>> 0;
}
function chunk(type: string, data: Buffer): Buffer {
	const len = Buffer.alloc(4);
	len.writeUInt32BE(data.length);
	const td = Buffer.concat([Buffer.from(type, 'ascii'), data]);
	const crc = Buffer.alloc(4);
	crc.writeUInt32BE(crc32(td));
	return Buffer.concat([len, td, crc]);
}

// ---- the theorem cards

/** A proved theorem's card: the brand, its title, its Lean name and three facts. */
export interface TheoremCard {
	/** The small line over the title ("Accepted by prove2me"). */
	kicker: string;
	title: string;
	/** A monospace line under the title (the Lean name). */
	mono?: string;
	facts: { label: string; value: string }[];
}

/** The card as a PNG. */
export function theoremCardPNG(t: TheoremCard): Buffer {
	const c = new Card();
	c.glow(1020, 120, 700, colors.glow);
	const display = font('display', 800), sans = font('sans', 600), sansBold = font('sans', 700), mono = font('mono', 500);
	const X = 72, inner = CARD_W - 2 * X;
	// the brand
	c.rect(X, 56, 52, 52, colors.logo, 12);
	const at = font('mono', 800);
	c.text('@', at, 34, X + 26 - measure('@', at, 34) / 2, 94, colors.text);
	c.text('AIs@home', display, 44, X + 70, 96, colors.text);
	const tag = 'prove2me';
	c.text(tag, mono, 24, CARD_W - X - measure(tag, mono, 24), 92, colors.dim);
	// the kicker and the title
	c.text(t.kicker.toUpperCase(), sansBold, 22, X, 176, colors.accent, 2);
	const titleSize = measure(t.title, sansBold, 54) <= inner * 2 ? 54 : 46;
	const lines = wrap(t.title, sansBold, titleSize, inner, 3);
	let y = 176 + titleSize * 1.25;
	for (const l of lines) {
		c.text(l, sansBold, titleSize, X, y, colors.text);
		y += titleSize * 1.18;
	}
	if (t.mono) c.text(clip(t.mono, mono, 24, inner), mono, 24, X, Math.min(y + 8, 470), colors.muted);
	// the facts
	c.rect(X, 500, inner, 2, colors.line);
	const col = inner / Math.max(1, t.facts.length);
	t.facts.forEach((f, i) => {
		const fx = X + i * col;
		c.text(clip(f.value, display, 50, col - 24), display, 50, fx, 568, colors.text);
		c.text(clip(f.label.toUpperCase(), sans, 17, col - 24), sans, 17, fx, 596, colors.dim, 1.5);
	});
	return c.png();
}
