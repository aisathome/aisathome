// TeX in prove2me's titles as plain text (tex-text.ts): the titles come live from the gateway, so the conversion must
// read well on the titles we have and never fail on the ones we do not.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import snapshot from '../src/data/science/prove2me-records.json';
import { asciiFallback, texText } from '../src/data/tex-text';
import { plainTitle } from '../src/data/prove2me-view';
import { font, theoremCardPNG, wrap } from '../src/data/og-card';
import { pngSize } from '../scripts/seo-checks.mjs';

/** Every title in the committed records copy that has TeX. */
const titles = (() => {
	const out = new Set<string>();
	JSON.stringify(snapshot, (k, v) => {
		if (k === 'title' && typeof v === 'string' && v.includes('$')) out.add(v);
		return v;
	});
	return [...out];
})();

test('TeX in a title reads as text', () => {
	const cases: [string, string][] = [
		['§5.2, Eq. (46) — $\\Pr[\\Sigma_S=\\sigma^-_S]\\ge\\pi^-$', '§5.2, Eq. (46) — Pr[Σ_S = σ⁻_S] ≥ π⁻'],
		['Claim A.2: $\\ker(a)\\cap\\operatorname{im}(a)=\\{0\\}$', 'Claim A.2: ker(a)∩im(a) = {0}'],
		['$\\langle v,x-y\\rangle\\le\\|x-y\\|$ for $v\\in B^*$ and $x,y\\in\\mathcal C$', '⟨v,x-y⟩ ≤ ‖x-y‖ for v ∈ B* and x,y ∈ C'],
		['$y^*_n(i)$ converge as $n\\to\\infty$', 'y*ₙ(i) converge as n → ∞'],
		['$\\sum_z w(y\\to z)=(-1)^{h(0,y)}\\in\\{-1,1\\}$', '∑_z w(y → z) = (-1)^(h(0,y)) ∈ {-1,1}'],
		['$\\mathrm{Dec}_{O,h}(\\sigma_h,k;\\tau)=\\bot\\iff\\mathrm{Reject}_O(\\Gamma_O,k)=1$', 'Dec_(O,h)(σₕ,k;τ) = ⊥ ⟺ Reject_O(Γ_O,k) = 1'],
		['size $2^{H+O(\\sqrt n)}$', 'size 2^(H+O(√n))'],
		['$\\frac{a+b}{2}\\le\\sqrt{ab}$, $\\bar v$, $\\mathbb{R}^n$, $\\kappa^\\top x$', '(a+b)/2 ≤ √(ab), v̄, ℝⁿ, κᵀx'],
		['$\\left( x \\right.$ and $\\not\\in$ and $\\log n$', '(x and ∉ and log n'],
		['no maths here: ε-Pareto', 'no maths here: ε-Pareto'],
	];
	for (const [tex, text] of cases) assert.equal(texText(tex), text, tex);
	assert.equal(plainTitle(cases[0][0]), cases[0][1]); // the pages' plainTitle is this conversion
});

test('a title from the gateway never breaks it: unknown commands, broken braces, stray $', () => {
	assert.equal(texText('$\\foo x$'), 'foo x');
	assert.equal(texText('$\\frac{a$'), 'a/');
	assert.equal(texText('$a}}b$'), 'ab');
	assert.equal(texText('costs $5, and $x^2$'), 'costs 5, and x^2'); // TeX pairs the $ the same way
	assert.equal(texText('costs $5'), 'costs 5');
	for (const t of ['$', '$$', '\\', '$\\$', '$^_^_{$', '$\\left$', '$\\sqrt[$', '$\\not$', '$\\frac$', '$x_{$', '${{{{$', '\\(\\alpha\\)', '$\\\\$']) {
		const out = texText(t);
		assert.equal(typeof out, 'string', t);
		assert.ok(!out.includes('$'), `${t} -> ${out}`);
	}
	assert.equal(texText('\\(\\alpha\\)'), 'α');
	// long input in linear time
	const long = `$${'\\alpha_{x^{y}}'.repeat(2000)}$`;
	const t0 = Date.now();
	texText(long);
	assert.ok(Date.now() - t0 < 1000);
});

test('every TeX title in the records reads without TeX', () => {
	assert.ok(titles.length > 10, `${titles.length} titles with TeX`);
	for (const t of titles) {
		const out = texText(t);
		assert.ok(!/[$\\]/.test(out), `${t} -> ${out}`);
		assert.ok(!/[{}]/.test(out.replace(/\{[^{}]*\}/g, '')), `unbalanced braces: ${out}`);
	}
});

test('a share card draws every character of those titles: its font, the Greek file, or an ASCII spelling', () => {
	const sans = font('sans', 700), greek = font('mono', 700, 'greek');
	for (const t of titles)
		for (const ch of texText(t)) {
			const cp = ch.codePointAt(0)!;
			assert.ok(sans.hasGlyphForCodePoint(cp) || greek.hasGlyphForCodePoint(cp) || asciiFallback(ch) !== undefined, `no glyph and no fallback for ${ch} (U+${cp.toString(16)}) in ${t}`);
		}
	assert.equal(asciiFallback('≤'), '<=');
	assert.equal(asciiFallback('ₙ'), '_n');
	assert.equal(asciiFallback('̄'), '');
	// a word that ends in '.' keeps its space; only a Lean name split to fit joins without one
	assert.deepEqual(wrap('§5.2, Eq. (46) — x', sans, 54, 1056, 3), ['§5.2, Eq. (46) — x']);
	const lean = 'Aaaaaaaaaaaaaaaaaaaa.Bbbbbbbbbbbbbbbbbbbbbbbbbb.c_d';
	const pieces = wrap(lean, sans, 54, 700, 4);
	assert.ok(pieces.length > 1 && pieces.join('') === lean, pieces.join('|'));
	const png = theoremCardPNG({ kicker: 'Accepted by prove2me', title: texText(titles[0]), mono: 'x', facts: [{ label: 'Trial', value: '16' }] });
	assert.deepEqual(pngSize(png), { width: 1200, height: 630 });
});
