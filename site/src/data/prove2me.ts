// The prove2me page (/science/prove2me/): the trials in which Qwen3.8 27B, on the AIs@home network, tried leaves of
// prove2me (prove2.me), and the proofs we submitted there. The data is science/prove2me.json, made from our own trial
// records by our export script (public fields only: prove2me's theorem names, ids and titles, our
// outcomes and model minutes, the platform's verdict) and committed here. The build renders it; the page also serves
// it at /science/prove2me.json. The network's share (requests, tokens, node time of project prove2me) is live: the
// page reads it from the gateway's public /v1/stats in the browser.
//
// SPDX-License-Identifier: Apache-2.0
import raw from './science/prove2me.json';

// The pure helpers live in prove2me-live.ts, which the browser's script imports without this data.
export { compact, leafProgress, plural, stateLabel } from './prove2me-live';
import type { Outline, StaticTask } from './prove2me-live';

export type Group = 'calibration' | 'open';
export type Outcome = 'proved' | 'refused' | 'compile_error' | 'no_solution' | 'no-progress' | string;

export interface LeafRun {
	leaf: string;
	group: Group;
	theorem: string;
	title: string;
	url: string;
	outcome: Outcome;
	proved_in_round: number | null;
	rounds: number;
	scored: boolean;
	not_scored_why?: string;
	model_minutes: number;
	/** Summed over the run's model steps: prompt tokens (cache re-reads included), of them from the cache, and output. */
	prompt_tokens: number;
	cached_tokens: number;
	output_tokens: number;
	earlier: number[];
}

export interface Trial {
	trial: number;
	start: string;
	end: string;
	scaffold: string;
	caps: string;
	summary: string;
	leaves: LeafRun[];
}

export interface CurrentLeaf {
	leaf: string;
	group: Group;
	theorem: string;
	title: string;
	url: string;
	state: 'running' | 'queued' | 'finished' | 'failed' | 'cancelled' | string;
	earlier: number[];
	/** The outline task's state (scaffold v5: the model writes the proof's outline with sorry holes first). */
	outline?: string;
	/** The ledger's count, when a trial summary has it. */
	holes?: { proved: number; n: number };
	/** The hole tasks on the queue: one per extracted hole. */
	hole_tasks?: { n: number; proved: number; running: number; queued: number };
	/** Each outline (generation) with its holes and the pre-pass, from the outline task's records. */
	outlines?: Outline[];
}

export interface Submission {
	leaf: string;
	trial: number;
	theorem: string;
	title: string;
	url: string;
	submitted_at: string;
	verdict: string;
	verdict_at: string | null;
}

export interface Prove2me {
	project: string;
	project_url: string;
	model: string;
	generated_at: string;
	as_of: string;
	lean: string;
	mathlib: string;
	totals: {
		runs: number;
		proved: number;
		scored: number;
		calibration_runs: number;
		calibration_proved: number;
		open_runs: number;
		open_leaves: number;
		open_leaves_proved: number;
		model_hours: number;
		prompt_tokens: number;
		cached_tokens: number;
		output_tokens: number;
		submitted: number;
		accepted: number;
	};
	submissions: Submission[];
	current: {
		trial: number;
		start: string;
		scaffold: string;
		caps: string;
		summary: string;
		holes_as_of: string | null;
		leaves: CurrentLeaf[];
		/** Every task of the trial so far, with its tokens from its own records (null: not known). */
		tasks?: StaticTask[];
	} | null;
	trials: Trial[];
}

export const p2m = raw as Prove2me;

/** Links go to prove2me's theorem pages only. */
export const theoremURL = /^https:\/\/prove2\.me\/theorems\/[0-9a-f-]{36}$/;

/** A prove2me title without its TeX delimiters (the page has no TeX renderer). */
export const plainTitle = (t: string) => t.replace(/\$([^$]*)\$/g, '$1').trim();

export function outcomeLabel(r: LeafRun): string {
	if (!r.scored) return 'Not scored';
	switch (r.outcome) {
		case 'proved':
			return r.rounds > 1 && r.proved_in_round ? `Proved in round ${r.proved_in_round}` : 'Proved';
		case 'refused':
			return 'Not proved: a sorry left';
		case 'compile_error':
			return 'Not proved: did not compile';
		case 'no_solution':
			return 'Not proved: no solution file';
		case 'no-progress':
			return 'Not proved: stopped, no progress';
		default:
			return 'Not proved';
	}
}


const months = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
/** "2026-11-12" -> "12 November 2026"; with `year` false, "12 November". */
export function day(iso: string, year = true) {
	const d = new Date(iso.length === 10 ? `${iso}T00:00:00Z` : iso);
	return `${d.getUTCDate()} ${months[d.getUTCMonth()]}${year ? ` ${d.getUTCFullYear()}` : ''}`;
}
/** "2026-11-12T06:51:23Z" -> "06:51 UTC". */
export const hhmm = (iso: string) => `${iso.slice(11, 16)} UTC`;

/** A trial's dates, short for a table: "7 Nov", "9–12 Nov", "30 Nov–2 Dec" (the year is in the trial's own list). */
export function shortSpan(t: { start: string; end: string }) {
	const m = (iso: string) => ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][new Date(`${iso}T00:00:00Z`).getUTCMonth()];
	const d = (iso: string) => new Date(`${iso}T00:00:00Z`).getUTCDate();
	if (t.start === t.end) return `${d(t.start)} ${m(t.start)}`;
	return m(t.start) === m(t.end) ? `${d(t.start)}–${d(t.end)} ${m(t.end)}` : `${d(t.start)} ${m(t.start)}–${d(t.end)} ${m(t.end)}`;
}

/** A trial's dates: "7 November 2026", or "9 to 12 November 2026". */
export function span(t: { start: string; end: string }) {
	if (t.start === t.end) return day(t.start);
	const a = new Date(`${t.start}T00:00:00Z`), b = new Date(`${t.end}T00:00:00Z`);
	if (a.getUTCMonth() === b.getUTCMonth()) return `${a.getUTCDate()} to ${day(t.end)}`;
	return `${day(t.start, false)} to ${day(t.end)}`;
}

/** Proved of run, by group, for one trial. */
export function counts(rows: LeafRun[]) {
	const of = (g?: Group) => {
		const rs = g ? rows.filter((r) => r.group === g) : rows;
		return { proved: rs.filter((r) => r.outcome === 'proved').length, runs: rs.length, unscored: rs.filter((r) => !r.scored).length };
	};
	return { all: of(), calibration: of('calibration'), open: of('open') };
}

/** Qwen tokens as on the Tau Ceti page: everything the model read (cache re-reads included) and wrote. */
export type Tok = { prompt_tokens: number; cached_tokens: number; output_tokens: number };
export const qwenTokens = (r: Tok) => r.prompt_tokens + r.output_tokens;
export const sumTokens = (rs: Tok[]): Tok => ({
	prompt_tokens: rs.reduce((a, r) => a + r.prompt_tokens, 0),
	cached_tokens: rs.reduce((a, r) => a + r.cached_tokens, 0),
	output_tokens: rs.reduce((a, r) => a + r.output_tokens, 0),
});
const exact = (v: number) => new Intl.NumberFormat('en').format(v);
/** The hover text of a token count: the exact number, how many were written and how many came from the cache. */
export const tokTitle = (r: Tok) =>
	`${exact(qwenTokens(r))} Qwen tokens, ${exact(r.output_tokens)} of them written; ${exact(r.cached_tokens)} of the ${exact(r.prompt_tokens)} read came from the cache`;
