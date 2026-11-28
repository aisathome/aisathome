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
export { compact, leafOutcomeLabel, leafOutcomes, leafProgress, leafRoleLabel, plural, stateLabel } from './prove2me-live';
import type { Outline, StaticTask } from './prove2me-live';
import type { TaskTableRow } from './task-table';
import { resultLabel } from './prove2me-outcomes';

export type Group = 'calibration' | 'open';
export type Outcome = 'proved' | 'refused' | 'compile_error' | 'no_solution' | 'no-progress' | string;

export interface LeafRun {
	leaf: string;
	group: Group;
	/** The leaf's role in this trial (a comparison leaf is an open leaf run again to compare scaffolds). */
	role?: 'calibration' | 'open' | 'comparison';
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
	/** Scaffold v5 on (a leaf is many tasks): the outline that was proved, every task and each outline's holes. */
	proved_in_outline?: number | null;
	tasks?: StaticTask[];
	outlines?: Outline[];
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
	/** The trial of the run that proved it; for a reuse, the trial whose outline probes found it. */
	trial: number;
	/** "proof": the model's own proof from a run; "reuse": the leaf is the same statement as a theorem already proved on
	 * prove2me, or a part of one ("relation"), which our index found, and the proof is one line that uses it (not counted as
	 * a leaf proved). */
	kind: 'proof' | 'reuse';
	/** A reuse: the proved theorem it imports, its prove2me page and its author as that page shows it. */
	reuses?: { theorem: string; url: string; author: string; relation?: 'same' | 'part' };
	/** A reuse: found by the trial's outline probes (while it was prepared) or during the trial. */
	found?: 'probes' | 'trial';
	theorem: string;
	title: string;
	url: string;
	submitted_at: string;
	verdict: string;
	verdict_at: string | null;
}

/** One leaf ever tried (one row per theorem over every run; a leaf closed by reuse has no run). */
export interface Leaf {
	/** "p2m-" and the first 6 hex characters of prove2me's theorem_id (8 when two collide). */
	id: string;
	/** The trials' own ids (C11, F01, ...). */
	aliases: string[];
	theorem: string;
	title: string;
	url: string;
	roles: ('calibration' | 'open' | 'comparison')[];
	trials: number[];
	runs: number;
	proved_runs: number;
	outcome: 'accepted' | 'reused' | 'proved' | 'not_proved' | 'stopped' | 'not_scored';
	first_tried: string;
	/** Summed over its runs; null for a leaf with no run. */
	model_minutes: number | null;
	prompt_tokens: number | null;
	cached_tokens: number | null;
	output_tokens: number | null;
}

export interface ProbeRound {
	trial: string;
	for_trial: number;
	state: 'finished' | 'running';
	start: string;
	end: string | null;
	scaffold: string;
	caps: string;
	leaves: { id: string; alias: string; role: string; theorem: string; title: string; url: string }[];
	tasks: number | null;
	model_minutes: number | null;
	prompt_tokens: number | null;
	cached_tokens: number | null;
	output_tokens: number | null;
}

export interface RoleScore {
	role: 'calibration' | 'open' | 'comparison';
	leaves: number;
	runs: number;
	proved: number;
	accepted: number;
	reused: number;
	model_hours: number;
	prompt_tokens: number;
	cached_tokens: number;
	output_tokens: number;
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
		/** Accepted submissions closed by reuse (in accepted, not in open_leaves_proved). */
		reused: number;
		/** Leaves ever tried (rows of `leaves`). */
		leaves: number;
		/** The finished outline probe rounds (`probes`): not runs, so outside every count above; the page adds their
		 * model hours and tokens to its totals and names them as probes. */
		probes?: { rounds: number; tasks: number; model_hours: number; prompt_tokens: number; cached_tokens: number; output_tokens: number };
	};
	submissions: Submission[];
	current: {
		trial: number;
		/** "preparing": the next trial's texts only, no leaves and no task yet. */
		state?: 'preparing';
		start: string | null;
		scaffold: string;
		caps: string;
		summary: string;
		holes_as_of: string | null;
		leaves: CurrentLeaf[];
		/** Every task of the trial so far, with its tokens from its own records (null: not known). */
		tasks?: StaticTask[];
	} | null;
	trials: Trial[];
	/** Outline probe rounds (t6p: trial 6's leaves chosen by one outline task each): "finished" with its tasks' sums,
	 * "running" with its leaves only (its tasks come live from the gateway). */
	probes?: ProbeRound[];
	leaves: Leaf[];
	by_role: RoleScore[];
	/** Every task of every trial in the generic task table's fields (task-table.ts), relations included. */
	tasks: TaskTableRow[];
}

// through unknown: a JSON import types every string as string, not as the unions Prove2me names
export const p2m = raw as unknown as Prove2me;

/** Links go to prove2me's theorem pages only. */
export const theoremURL = /^https:\/\/prove2\.me\/theorems\/[0-9a-f-]{36}$/;


export function outcomeLabel(r: LeafRun): string {
	if (!r.scored) return 'Not scored';
	switch (r.outcome) {
		case 'proved':
			if (r.proved_in_outline && r.rounds > 1) return `Proved with outline ${r.proved_in_outline}`;
			return r.rounds > 1 && r.proved_in_round ? `Proved in round ${r.proved_in_round}` : 'Proved';
		default: {
			// a sorry left, did not compile, ... (prove2me-outcomes.ts)
			const w = resultLabel('done', r.outcome);
			return w.startsWith('Not proved') ? w : 'Not proved';
		}
	}
}


import { day, hhmm } from './prove2me-current';
export { day, hhmm };
// The tables' helpers live in prove2me-trials.ts and prove2me-view.ts, which the browser's script imports without this data.
export { computedTokens, shortSpan, sumTokens, tokTitle, tokensWithCache, type Tok } from './prove2me-trials';
export { plainTitle, span } from './prove2me-view';

/** Proved of run, by group, for one trial. */
export function counts(rows: LeafRun[]) {
	const of = (g?: Group) => {
		const rs = g ? rows.filter((r) => r.group === g) : rows;
		return { proved: rs.filter((r) => r.outcome === 'proved').length, runs: rs.length, unscored: rs.filter((r) => !r.scored).length };
	};
	return { all: of(), calibration: of('calibration'), open: of('open') };
}
