// TeX in prove2me's titles as plain text: `$\Pr[\Sigma_S=\sigma^-_S]\ge\pi^-$` reads "Pr[Σ_S = σ⁻_S] ≥ π⁻". The titles
// come live from the gateway's records, typed by prove2me's users, so this never fails: an unknown command is written
// as its name, unbalanced braces or `$` are dropped, and anything that throws falls back to the text without its `$`.
// No renderer and no dependency: the result is a string, escaped like any other value wherever it is shown, and the
// same in the build, the browser and the share cards (which draw it with asciiFallback for glyphs their fonts lack).
//
// SPDX-License-Identifier: Apache-2.0

/** [command, Unicode, ASCII]: the symbols written as a character (ASCII: what a share card draws when its fonts lack
 * the character; a Greek letter's is its name, for one even the cards' Greek font lacks). */
const SYMBOLS: [string, string, string][] = [
	// Greek
	...(
		[
			['alpha', 'α'], ['beta', 'β'], ['gamma', 'γ'], ['delta', 'δ'], ['epsilon', 'ϵ'], ['varepsilon', 'ε'], ['zeta', 'ζ'], ['eta', 'η'],
			['theta', 'θ'], ['vartheta', 'ϑ'], ['iota', 'ι'], ['kappa', 'κ'], ['lambda', 'λ'], ['mu', 'μ'], ['nu', 'ν'], ['xi', 'ξ'], ['pi', 'π'],
			['varpi', 'ϖ'], ['rho', 'ρ'], ['varrho', 'ϱ'], ['sigma', 'σ'], ['varsigma', 'ς'], ['tau', 'τ'], ['upsilon', 'υ'], ['phi', 'ϕ'],
			['varphi', 'φ'], ['chi', 'χ'], ['psi', 'ψ'], ['omega', 'ω'], ['Gamma', 'Γ'], ['Delta', 'Δ'], ['Theta', 'Θ'], ['Lambda', 'Λ'],
			['Xi', 'Ξ'], ['Pi', 'Π'], ['Sigma', 'Σ'], ['Upsilon', 'Υ'], ['Phi', 'Φ'], ['Psi', 'Ψ'], ['Omega', 'Ω'],
		] as const
	).map(([c, u]): [string, string, string] => [c, u, c]),
	// relations
	['le', '≤', '<='], ['leq', '≤', '<='], ['ge', '≥', '>='], ['geq', '≥', '>='], ['ne', '≠', '!='], ['neq', '≠', '!='], ['leqslant', '≤', '<='],
	['geqslant', '≥', '>='], ['ll', '≪', '<<'], ['gg', '≫', '>>'], ['approx', '≈', '~'], ['sim', '∼', '~'], ['simeq', '≃', '~='], ['cong', '≅', '~='],
	['equiv', '≡', '=='], ['propto', '∝', 'prop.'], ['in', '∈', 'in'], ['notin', '∉', 'not in'], ['ni', '∋', 'ni'], ['subset', '⊂', 'subset'],
	['subseteq', '⊆', 'subseteq'], ['supset', '⊃', 'supset'], ['supseteq', '⊇', 'supseteq'], ['subsetneq', '⊊', 'subsetneq'], ['mid', '∣', '|'],
	['parallel', '∥', '||'], ['perp', '⊥', 'perp'], ['models', '⊨', '|='], ['vdash', '⊢', '|-'], ['prec', '≺', '<'], ['succ', '≻', '>'],
	['preceq', '⪯', '<='], ['succeq', '⪰', '>='], ['to', '→', '->'], ['rightarrow', '→', '->'], ['leftarrow', '←', '<-'], ['gets', '←', '<-'],
	['mapsto', '↦', '|->'], ['Rightarrow', '⇒', '=>'], ['implies', '⟹', '=>'], ['Leftarrow', '⇐', '<='], ['impliedby', '⟸', '<='],
	['iff', '⟺', '<=>'], ['Leftrightarrow', '⇔', '<=>'], ['leftrightarrow', '↔', '<->'], ['longrightarrow', '⟶', '->'], ['longmapsto', '⟼', '|->'],
	['uparrow', '↑', '^'], ['downarrow', '↓', 'v'], ['searrow', '↘', '\\'], ['nearrow', '↗', '/'], ['hookrightarrow', '↪', '->'],
	['rightharpoonup', '⇀', '->'], ['coloneqq', '≔', ':='], ['triangleq', '≜', ':='],
	// operators
	['pm', '±', '+-'], ['mp', '∓', '-+'], ['times', '×', 'x'], ['div', '÷', '/'], ['cdot', '·', '*'], ['ast', '∗', '*'], ['star', '⋆', '*'],
	['circ', '∘', 'o'], ['bullet', '•', '*'], ['oplus', '⊕', '(+)'], ['ominus', '⊖', '(-)'], ['otimes', '⊗', '(x)'], ['odot', '⊙', '(.)'],
	['cap', '∩', 'cap'], ['cup', '∪', 'cup'], ['setminus', '∖', '\\'], ['wedge', '∧', 'and'], ['land', '∧', 'and'], ['vee', '∨', 'or'],
	['lor', '∨', 'or'], ['neg', '¬', 'not '], ['lnot', '¬', 'not '], ['sqcup', '⊔', 'u'], ['sqcap', '⊓', 'n'], ['uplus', '⊎', 'u+'],
	['wr', '≀', 'wr'], ['diamond', '⋄', '<>'], ['triangle', '△', 'triangle'], ['bigtriangleup', '△', 'triangle'],
	['sum', '∑', 'sum'], ['prod', '∏', 'prod'], ['coprod', '∐', 'coprod'], ['int', '∫', 'int'], ['oint', '∮', 'oint'], ['iint', '∬', 'iint'],
	['bigcup', '⋃', 'U'], ['bigcap', '⋂', 'n'], ['bigoplus', '⨁', '(+)'], ['bigotimes', '⨂', '(x)'], ['bigvee', '⋁', 'or'], ['bigwedge', '⋀', 'and'],
	['bigsqcup', '⨆', 'u'], ['nabla', '∇', 'grad'], ['partial', '∂', 'd'],
	// letters and other symbols
	['infty', '∞', 'inf'], ['emptyset', '∅', '{}'], ['varnothing', '∅', '{}'], ['forall', '∀', 'for all '], ['exists', '∃', 'exists '],
	['nexists', '∄', 'no '], ['top', '⊤', 'T'], ['bot', '⊥', 'bot'], ['ell', 'ℓ', 'l'], ['hbar', 'ℏ', 'h'], ['aleph', 'ℵ', 'aleph'], ['Re', 'ℜ', 'Re'],
	['Im', 'ℑ', 'Im'], ['wp', '℘', 'P'], ['prime', '′', "'"], ['dagger', '†', '+'], ['ddagger', '‡', '++'], ['angle', '∠', 'angle'],
	['cdots', '⋯', '...'], ['ldots', '…', '...'], ['dots', '…', '...'], ['vdots', '⋮', '...'], ['ddots', '⋱', '...'], ['square', '□', '[]'],
	['Box', '□', '[]'], ['blacksquare', '■', '[]'], ['checkmark', '✓', 'v'], ['sharp', '♯', '#'], ['flat', '♭', 'b'], ['S', '§', 'S'],
	['langle', '⟨', '<'], ['rangle', '⟩', '>'], ['lceil', '⌈', '['], ['rceil', '⌉', ']'], ['lfloor', '⌊', '['], ['rfloor', '⌋', ']'],
	['lvert', '|', '|'], ['rvert', '|', '|'], ['vert', '|', '|'], ['lVert', '‖', '||'], ['rVert', '‖', '||'], ['Vert', '‖', '||'],
	['lbrace', '{', '{'], ['rbrace', '}', '}'], ['backslash', '\\', '\\'], ['colon', ':', ':'], ['quad', ' ', ' '], ['qquad', ' ', ' '],
];
const symbol = new Map(SYMBOLS.map(([c, u]) => [c, u]));
/** Relations: written with a space each side outside sub- and superscripts. */
const RELATIONS = new Set('= < > ≤ ≥ ≠ ≪ ≫ ≈ ∼ ≃ ≅ ≡ ∝ ∈ ∉ ∋ ⊂ ⊆ ⊃ ⊇ ⊊ ⊨ ⊢ ≺ ≻ ⪯ ⪰ → ← ↦ ⇒ ⟹ ⇐ ⟸ ⟺ ⇔ ↔ ⟶ ⟼ ↪ ⇀ ≔ ≜'.split(' '));
/** Operators written as their name ("\log x" reads "log x"). */
const NAMED = new Set(
	'Pr log ln lg exp sin cos tan cot sec csc sinh cosh tanh arcsin arccos arctan max min sup inf lim liminf limsup det dim ker deg gcd lcm arg hom Hom mod bmod pmod rank tr Tr diag sgn sign supp Var Cov dist Ext Tor Aut End id'.split(' '),
);
/** Commands that change the font: the argument is written as it is (\mathbb on a letter that has one, ℝ). */
const FONTS = new Set('mathrm text textrm textnormal mathit textit mathbf textbf boldsymbol bm mathsf textsf mathtt texttt emph mathnormal mathcal mathscr mathfrak operatorname mbox hbox mathbb'.split(' '));
const BB: Record<string, string> = { R: 'ℝ', N: 'ℕ', Z: 'ℤ', Q: 'ℚ', C: 'ℂ', P: 'ℙ', E: '𝔼', F: '𝔽', H: 'ℍ' };
/** Accents, as a combining character on each letter. */
const ACCENTS: Record<string, string> = {
	bar: '\u0304', overline: '\u0305', hat: '\u0302', widehat: '\u0302', tilde: '\u0303', widetilde: '\u0303', vec: '\u20d7', dot: '\u0307', ddot: '\u0308',
	check: '\u030c', breve: '\u0306', acute: '\u0301', grave: '\u0300', underline: '\u0332',
};
/** Commands that are written as nothing (sizes, spacing, styles); \left and \right keep their delimiter. */
const SILENT = new Set(
	'left right big Big bigg Bigg bigl bigr Bigl Bigr biggl biggr Biggl Biggr middle displaystyle textstyle scriptstyle limits nolimits nonumber notag mathstrut strut phantom'.split(' '),
);
const SUP: Record<string, string> = { '0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾', n: 'ⁿ', i: 'ⁱ', T: 'ᵀ', '⊤': 'ᵀ', '*': '*', '′': '′', "'": '′' };
const SUB: Record<string, string> = {
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎',
	a: 'ₐ', e: 'ₑ', h: 'ₕ', i: 'ᵢ', j: 'ⱼ', k: 'ₖ', l: 'ₗ', m: 'ₘ', n: 'ₙ', o: 'ₒ', p: 'ₚ', r: 'ᵣ', s: 'ₛ', t: 'ₜ', u: 'ᵤ', v: 'ᵥ', x: 'ₓ',
};

/** What a share card draws for a character its fonts lack: the ASCII of a symbol, sub- and superscripts as `_x` and `^x`,
 * ℝ as R; '' for a combining accent. undefined: none known. */
export function asciiFallback(ch: string): string | undefined {
	if (/^[\u0300-\u036f\u20d0-\u20ff]$/.test(ch)) return '';
	return ASCII.get(ch);
}
const ASCII = new Map<string, string>([
	...SYMBOLS.map(([, u, a]): [string, string] => [u, a]),
	...Object.entries(SUP).filter(([k, v]) => k !== v).map(([k, v]): [string, string] => [v, `^${k === '⊤' ? 'T' : k}`]),
	...Object.entries(SUB).map(([k, v]): [string, string] => [v, `_${k}`]),
	...Object.entries(BB).map(([k, v]): [string, string] => [v, k]),
	['√', 'sqrt'], ['⊈', 'not subseteq'], ['⊄', 'not subset'], ['−', '-'], ['–', '-'], ['—', '-'], ['…', '...'], ['’', "'"], ['‘', "'"], ['“', '"'], ['”', '"'], ['‾', '-'],
]);

class Reader {
	i = 0;
	constructor(readonly s: string) {}
	get done() {
		return this.i >= this.s.length;
	}
	peek() {
		return this.s[this.i];
	}
	/** A control word or symbol after the backslash at i: "\alpha" → "alpha", "\{" → "{". The spaces after a word go. */
	command(): string {
		this.i++; // the backslash
		const m = /^[A-Za-z]+/.exec(this.s.slice(this.i));
		if (!m) return this.s[this.i++] ?? '';
		this.i += m[0].length;
		while (this.s[this.i] === ' ') this.i++;
		return m[0];
	}
	skipSpaces() {
		while (this.s[this.i] === ' ') this.i++;
	}
}

/** One argument of a command or script: a group, a command or one character, as text. */
function arg(r: Reader, script: boolean): string {
	r.skipSpaces();
	if (r.done) return '';
	if (r.peek() === '{') {
		r.i++;
		return seq(r, script, '}');
	}
	if (r.peek() === '\\') return command(r, r.command(), script);
	return r.s[r.i++];
}
/** A group's content as one unit: wrapped in parentheses when it is more than one simple token. */
const unit = (t: string) => (!t || /^[\p{L}\p{N}.′']+$/u.test(t) || /^\(.*\)$/.test(t) ? t : `(${t})`);

function command(r: Reader, name: string, script: boolean): string {
	if (FONTS.has(name)) {
		const a = arg(r, script);
		return name === 'mathbb' ? [...a].map((c) => BB[c] ?? c).join('') : a;
	}
	if (name in ACCENTS) return [...arg(r, script)].map((c) => (/\s/.test(c) ? c : c + ACCENTS[name])).join('');
	if (name === 'frac' || name === 'dfrac' || name === 'tfrac') {
		const a = arg(r, true), b = arg(r, true);
		return `${unit(a)}/${unit(b)}`;
	}
	if (name === 'binom') {
		const a = arg(r, true), b = arg(r, true);
		return `C(${a},${b})`;
	}
	if (name === 'sqrt') {
		if (r.peek() === '[') {
			const end = r.s.indexOf(']', r.i);
			r.i = end < 0 ? r.s.length : end + 1;
		}
		const a = arg(r, true);
		return [...a].length === 1 ? `√${a}` : `√(${a})`;
	}
	if (name === 'not') {
		const a = arg(r, true).trim();
		const u = a === '∈' ? '∉' : a === '=' ? '≠' : a === '⊆' ? '⊈' : a === '⊂' ? '⊄' : `${a}\u0338`;
		return script || !RELATIONS.has(a) ? u : ` ${u} `;
	}
	if (SILENT.has(name)) {
		if (name === 'left' || name === 'right' || name === 'middle' || name.startsWith('big') || name.startsWith('Big')) {
			r.skipSpaces();
			if (r.peek() === '.') r.i++;
		}
		if (name === 'phantom') arg(r, script);
		return '';
	}
	if (NAMED.has(name)) return /^[A-Za-z0-9]/.test(r.peek() ?? '') ? `${name} ` : name;
	const u = symbol.get(name);
	if (u !== undefined) return script || !RELATIONS.has(u) ? u : ` ${u} `;
	// control symbols
	if (name === ',' || name === ';' || name === ':' || name === ' ' || name === '>' || name === '\\') return ' ';
	if (name === '!' || name === '/') return '';
	if (name === '|') return '‖';
	if (name.length === 1) return name; // \{ \} \% \& \_ \# \$
	// an unknown command: its name
	return /^[A-Za-z]/.test(r.peek() ?? '') ? `${name} ` : name;
}

/** A sub- or superscript: Unicode when every character has one, else `_x` or `_(…)`. */
function script(base: string, mark: '^' | '_', t: string): string {
	const map = mark === '^' ? SUP : SUB;
	const s = t.trim();
	if (!s) return base;
	if ([...s].every((c) => c in map)) return base + [...s].map((c) => map[c]).join('');
	return `${base}${mark}${/^[\p{L}\p{N}]+$/u.test(s) || [...s].length === 1 ? s : `(${s})`}`;
}

/** Math up to `end` (a '}' or the end of the text) as plain text. */
function seq(r: Reader, inScript: boolean, end?: string): string {
	let out = '';
	while (!r.done) {
		const c = r.peek();
		if (end && c === end) {
			r.i++;
			return out;
		}
		if (c === '}') {
			r.i++; // unbalanced: dropped
			continue;
		}
		if (c === '^' || c === '_') {
			r.i++;
			out = script(out, c, arg(r, true));
			continue;
		}
		if (c === '{') {
			r.i++;
			out += seq(r, inScript, '}');
			continue;
		}
		if (c === '\\') {
			out += command(r, r.command(), inScript);
			continue;
		}
		if (c === '~') {
			r.i++;
			out += ' ';
			continue;
		}
		r.i++;
		out += !inScript && RELATIONS.has(c) ? ` ${c} ` : c;
	}
	return out;
}

/** One piece of math (without its `$`) as plain text. */
export function texMath(s: string): string {
	return seq(new Reader(s.replace(/\s+/g, ' ')), false)
		.replace(/ {2,}/g, ' ')
		.replace(/([([{⟨]) /g, '$1')
		.replace(/ ([)\]}⟩,;.])/g, '$1')
		.trim();
}

/** A title with its TeX (`$…$`, `$$…$$`, `\(…\)`) as plain text; the rest of the text as it is. */
export function texText(t: string): string {
	try {
		return t
			.replace(/\$\$([^$]*)\$\$|\$([^$]*)\$|\\\((.*?)\\\)/g, (_m, a: string | undefined, b: string | undefined, c: string | undefined) => {
				const m = a ?? b ?? c ?? '';
				// the spaces at its ends stay: "costs $5, and $x$" pairs the two $ as TeX does
				return `${/^\s/.test(m) ? ' ' : ''}${texMath(m)}${/\s$/.test(m) ? ' ' : ''}`;
			})
			.replace(/\$/g, '')
			.replace(/\s+/g, ' ')
			.trim();
	} catch {
		return t.replace(/\$/g, '').replace(/\s+/g, ' ').trim();
	}
}
