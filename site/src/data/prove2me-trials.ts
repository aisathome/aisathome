// The prove2me page's token and date helpers for its tables. Tokens (2026-11-17): the computed tokens
// lead (what the GPU worked through fresh: the model's reads without the cache, plus its writes); the reads again from
// the cache, most of an agent's reading, show only on hover as "with cache re-reads". Pure functions and no data, so
// the browser's script can import them.
//
// SPDX-License-Identifier: Apache-2.0

/** A trial's dates, short for a table: "7 Nov", "9–12 Nov", "30 Nov–2 Dec" (the year is in the trial's own list). */
export function shortSpan(t: { start: string; end: string }) {
	const m = (iso: string) => ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][new Date(`${iso}T00:00:00Z`).getUTCMonth()];
	const d = (iso: string) => new Date(`${iso}T00:00:00Z`).getUTCDate();
	if (t.start === t.end) return `${d(t.start)} ${m(t.start)}`;
	return m(t.start) === m(t.end) ? `${d(t.start)}–${d(t.end)} ${m(t.end)}` : `${d(t.start)} ${m(t.start)}–${d(t.end)} ${m(t.end)}`;
}

/** The build's token counts: prompt tokens with the cache re-reads in them, of them from the cache, and written. */
export type Tok = { prompt_tokens: number; cached_tokens: number; output_tokens: number };
/** Tokens computed: read without the cache, plus written. */
export const computedTokens = (r: Tok) => r.prompt_tokens - r.cached_tokens + r.output_tokens;
/** Everything read (the cache re-reads included) and written. */
export const tokensWithCache = (r: Tok) => r.prompt_tokens + r.output_tokens;
export const sumTokens = (rs: Tok[]): Tok => ({
	prompt_tokens: rs.reduce((a, r) => a + r.prompt_tokens, 0),
	cached_tokens: rs.reduce((a, r) => a + r.cached_tokens, 0),
	output_tokens: rs.reduce((a, r) => a + r.output_tokens, 0),
});
const exact = (v: number) => new Intl.NumberFormat('en').format(v);
/** The hover text of a token count: computed, of them written, and the count with the cache re-reads. */
export const tokTitle = (r: Tok) =>
	`${exact(computedTokens(r))} tokens computed, ${exact(r.output_tokens)} of them written; ${exact(tokensWithCache(r))} with ${exact(r.cached_tokens)} cache re-reads`;
/** The same from the two totals (the gateway's tokens_computed and tokens_with_cache). */
export const tokTitleOf = (computed: number, withCache: number, exactCount = true) =>
	`${exactCount ? '' : 'At least: '}${exact(computed)} tokens computed; ${exact(withCache)} with the cache re-reads`;
