// prove2me's outcome rules in one place (refactor review 2026-11-17, finding 2: the "in-place holes count when the
// assembly passes" rule lived in three modules). Every view of a task or a leaf (the leaf's outlines in words, its task
// table, the table of every task, the trials table) asks these functions what a task's outcome means:
//
//  - a leaf is proved by an outline without holes, or an assembly, that our full check passed (a run of trials 1 to 4:
//    the run itself);
//  - a hole is proved by its own task; the holes left in the outline's file ("in place") share one task that never says
//    proved: it ends `rest_done` with the file filled in, and the outline's assembly decides;
//  - `closed_by` on a task that ended `closed` names what closed its hole before a model ran (the pre-pass, our index
//    of prove2me's library, the ledger, another task's proof); on a *cancelled* task it names the task that ended the
//    outline (a hole that failed or was disproved, a proved leaf, a newer outline), so the cancellation says why;
//  - an outcome in a few plain words ("a sorry left" for `refused`, never the raw word).
//
// Pure functions over a small task shape (Attempt) that both the leaf view's rows and the task table's rows map to.
//
// SPDX-License-Identifier: Apache-2.0

/** A task as the rules read it. `kind`: outline, hole, rest (the holes in place), assembly, run (trials 1 to 4).
 * `state`: queued, running, done, failed, cancelled. */
export interface Attempt {
	id: string;
	kind: string;
	state: string;
	outcome: string | null;
	gen: number;
	hole: number | null;
	try: number;
	closed_by?: string | null;
	/** Whether a node ever took it; absent: read from its steps. */
	started?: boolean;
	steps?: number | null;
}

/** The leaf view's roles (skeleton, asm) and the task table's kinds (outline, assembly) as one vocabulary. */
export const kindOf = (role: string | null | undefined): string =>
	({ skeleton: 'outline', outline: 'outline', asm: 'assembly', assembly: 'assembly', hole: 'hole', rest: 'rest', run: 'run' })[role ?? ''] ?? role ?? '';
/** The leaf view's states (finished) and the gateway's (leased) as one vocabulary. */
export const stateOf = (s: string): string => ({ finished: 'done', leased: 'running' })[s] ?? s;

/** A row of the leaf view (prove2me-live.ts TaskRow) as an Attempt. */
export const attemptOf = (r: { id: string; role: string; state: string; outcome: string | null; gen: number; hole: number | null; attempt: number; closed_by?: string | null; started?: boolean; steps?: number | null }): Attempt => ({
	id: r.id, kind: kindOf(r.role), state: stateOf(r.state), outcome: r.outcome, gen: r.gen, hole: r.hole, try: r.attempt, closed_by: r.closed_by,
	started: r.started, steps: r.steps,
});

// ---- what a task's outcome means

/** A leaf proved: an outline without holes, an assembly or a whole run that our full check passed. */
export const provesLeaf = (a: Pick<Attempt, 'kind' | 'state' | 'outcome'>) =>
	stateOf(a.state) === 'done' && a.outcome === 'proved' && ['outline', 'assembly', 'run'].includes(kindOf(a.kind));

/** The in-place task ended with the outline's file filled in (`rest_done`; an older worker said `proved`). */
export const restFilled = (rest: Pick<Attempt, 'state' | 'outcome'> | undefined) =>
	!!rest && stateOf(rest.state) === 'done' && (rest.outcome === 'rest_done' || rest.outcome === 'proved');

export type Standing = { state: 'proved' | 'not_proved' | 'waiting'; text: string };

/** The holes in place, from the newest in-place task and the outline's newest assembly: proved when the assembly passed
 * our full check, not proved when it ended otherwise, waiting for it before; undefined while they are not filled in. */
export function inplaceStanding(rest: Pick<Attempt, 'state' | 'outcome'> | undefined, asm: Pick<Attempt, 'state' | 'outcome'> | undefined): Standing | undefined {
	if (!restFilled(rest)) return undefined;
	if (asm && stateOf(asm.state) === 'done') {
		return asm.outcome === 'proved'
			? { state: 'proved', text: 'proved (checked in the assembly)' }
			: { state: 'not_proved', text: 'filled in, but the assembly did not check' };
	}
	return { state: 'waiting', text: 'filled in; the assembly checks it' };
}

// A try's outcome in a few words (the task's own outcome; a hole's or the in-place task's).
const tryWords: Record<string, string> = {
	proved: 'proved',
	disproved: 'disproved',
	refused: 'a sorry left',
	compile_error: 'did not compile',
	no_solution: 'no solution file',
	'no-progress': 'stopped, no progress',
	no_progress: 'stopped, no progress',
	timeout: 'out of time',
	infra: 'a fault on our side',
	check_error: 'our check failed',
	type_mismatch: 'proved another statement',
	axioms: 'used an axiom not allowed',
	not_theorem: 'not a theorem',
	assembly_failed: 'did not check',
	no_manifest: 'no outline record',
	rest_done: 'filled in',
};
/** An outcome in a few words: "a sorry left", "did not compile"; an unknown one with its underscores as spaces. */
export const tryWord = (o: string | null | undefined) => (o ? tryWords[o] ?? o.replace(/[_-]/g, ' ') : 'ended');

/** An outline task that ended without an outline because its shape was refused: the reason in words, else undefined. */
export const outlineRefusal = (o: string | null | undefined): string | undefined =>
	({ degenerate_outline: 'degenerate (a hole is the leaf’s own claim)', inline_proof: 'an inline proof too long' })[o ?? ''];

/** A task's result for the per-leaf task table, in words. */
export function resultLabel(state: string, outcome: string | null): string {
	const s = stateOf(state);
	if (s !== 'done') return ({ running: 'Running', queued: 'Queued', failed: 'Stopped by a fault', cancelled: 'Cancelled' })[s] ?? s;
	const o: Record<string, string> = {
		proved: 'Proved',
		outline: 'Outline written',
		refused: 'Not proved: a sorry left',
		compile_error: 'Not proved: did not compile',
		no_solution: 'Not proved: no solution file',
		'no-progress': 'Not proved: stopped, no progress',
		infra: 'Not proved: a fault on our side',
		assembly_failed: 'Assembly did not check',
		no_manifest: 'Not assembled: no outline record',
		rest_done: 'Filled in, checked by the assembly',
		disproved: 'Disproved: the hole is false',
		check_error: 'Our check failed',
	};
	return o[outcome ?? ''] ?? (outcome ? `Ended: ${tryWord(outcome)}` : 'Finished');
}

// ---- closed_by: what closed a hole, or why a task was cancelled

// <trial>-<leaf>[-s<gen>][-h<hole>|-rest|-asm][-a<try>] (the trial runner names them so); a run of trials 1 to 4 is
// t<N>-<leaf>; an outline probe's trial is t<N>p.
const TASK_ID = /^(t[0-9]+p?)-(p2m-[0-9a-f]{6,8}|[A-Za-z0-9]+)(?:-s([0-9]+))?(?:-(?:h([0-9]+)|(rest|asm)))?(?:-a([0-9]+))?$/;
/** A task id's parts, or null when it is not one of ours. */
export function taskRef(id: string) {
	const m = TASK_ID.exec(id);
	if (!m) return null;
	return {
		trial: m[1], leaf: m[2], gen: Number(m[3] ?? 1), hole: m[4] ? Number(m[4]) : null,
		kind: m[4] ? 'hole' : m[5] ? ({ rest: 'rest', asm: 'assembly' })[m[5]]! : 'outline', attempt: Number(m[6] ?? 1),
	};
}

export type Closer = { state: 'prepass' | 'index' | 'ledger' | 'closed'; text: string };

/** What a hole's `closed_by` says when the hole was closed before a model ran: the index's theorem, the ledger's
 * proof, the pre-pass (run by the outline task), or another task's proof. */
export function closedByText(c: string): Closer {
	if (c.startsWith('reuse:formalpedia:')) return { state: 'index', text: 'closed by the index (reuse)' };
	if (c.startsWith('reuse:ledger')) return { state: 'ledger', text: 'closed by the ledger' };
	if (c.startsWith('reuse:prepass')) return { state: 'prepass', text: 'closed by the pre-pass' };
	if (c.startsWith('reuse:')) return { state: 'closed', text: 'closed by reuse' };
	if (taskRef(c)?.kind === 'outline') return { state: 'prepass', text: 'closed by the pre-pass' }; // run by that outline task
	return { state: 'closed', text: `closed by another task (${c})` };
}

/** A cancelled task's `closed_by` is the task that ended its outline (the trial runner's stale cancel), not a closer. */
export const isStaleCancel = (a: Pick<Attempt, 'state' | 'closed_by'>) => stateOf(a.state) === 'cancelled' && !!a.closed_by;

/** Why a cancelled task was cancelled, from the task its `closed_by` names: "outline abandoned after hole 1 failed",
 * "outline refuted: hole 2 is false", "leaf proved", "outline replaced". `peers`: the leaf's tasks (the named task's
 * outcome); without it, its id alone. Empty when the task has no such `closed_by`. */
export function staleCancelReason(a: Attempt, peers: Attempt[] = []): string {
	if (!isStaleCancel(a)) return '';
	const by = peers.find((p) => p.id === a.closed_by);
	const ref = taskRef(a.closed_by!);
	const kind = by ? kindOf(by.kind) : ref?.kind ?? '';
	const gen = by?.gen ?? ref?.gen ?? a.gen;
	const hole = by?.hole ?? ref?.hole ?? null;
	const outcome = by && stateOf(by.state) === 'done' ? by.outcome : null;
	switch (kind) {
		case 'hole':
			if (outcome === 'disproved') return `outline refuted: hole ${hole} is false`;
			return `outline abandoned after hole ${hole} failed`;
		case 'rest':
			return 'outline abandoned after the holes in place failed';
		case 'assembly':
			return outcome === 'proved' ? 'leaf proved' : 'outline abandoned after its assembly failed';
		case 'outline':
			if (outcome === 'proved') return 'leaf proved';
			return gen > a.gen ? 'outline replaced' : 'outline abandoned';
		default:
			return 'outline abandoned';
	}
}

/** Why a cancelled task was cancelled: its `closed_by` first (staleCancelReason, `own` true), else what its leaf's other
 * tasks show (its outline refuted or replaced, the leaf proved); `refuted` says whether the trial's data names its
 * outline refuted. Empty when nothing says. */
export function cancelReason(a: Attempt, peers: Attempt[], refuted = false): { text: string; own: boolean } {
	const own = staleCancelReason(a, peers);
	if (own) return { text: own, own: true };
	const g = a.gen;
	if (refuted) return { text: 'outline refuted', own: false };
	if (peers.some((x) => kindOf(x.kind) === 'hole' && x.gen === g && x.outcome === 'disproved')) return { text: 'outline refuted', own: false };
	if (peers.some((x) => kindOf(x.kind) === 'outline' && x.gen > g)) return { text: 'outline replaced', own: false };
	if (peers.some((x) => x.id !== a.id && provesLeaf(x))) return { text: 'leaf proved', own: false };
	return { text: '', own: false };
}
