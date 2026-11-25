// A leaf's outlines in words (2026-11-15: the task table and its one-line count were hard to follow). Newest
// outline first, each with its state (in progress, refuted and why, proved, abandoned and why), one row per hole and
// the assembly; above them one line for the whole leaf ("Running: outline 2, hole 1"). The task table stays below as
// the detail.
//
// A hole shows one of three states (2026-11-25): Waiting (held back behind another hole, its task not made or not
// started, its task failed, its outline ended without it, or nothing known), Working (its own task is queued or
// running, with the try and its steps) or Solved (by any route: the pre-pass, a theorem already known, a proof from
// earlier, the in-place task once the assembly checked it, its own task). Why it waits and how it was solved go into a
// tooltip only ("starts when hole 3 is done", "a new outline is being made", "solved automatically: simp", "already
// known: X", "with the outline", "own task").
//
// Made from the leaf's tasks (their roles, outlines, holes, tries, states and outcomes, and `closed_by`: GET /v1/tasks
// or the build's data), the build's outline records (holes, how many in place, what the pre-pass closed) and, when the
// project's data document has them, its per-hole states: which holes are in place, what closed a hole without a task,
// and a verdict reached outside a task (a hole re-judged false after our check failed on the model's disproof). From
// 2026-11-25 the outline's tasks name every hole in one word (the label `hole_states`, set by the prove2me runner when
// it makes them): its own task, held back behind the riskiest hole, solved by the pre-pass, already known (an imported
// theorem), proved before, left in place; so every hole of the outline has its row, those after the last hole task
// too. What an outcome means (a leaf proved, the holes in place, closed_by, an outcome in words) is
// prove2me-outcomes.ts's.
// Pure functions and one HTML renderer (every value escaped) for the build and the browser alike.
//
// SPDX-License-Identifier: Apache-2.0
import type { LeafDetail, Outline, TaskRow } from './prove2me-live';
import { attemptOf, closedByText, inplaceStanding, isStaleCancel, outlineRefusal, provesLeaf, staleCancelReason, tryWord } from './prove2me-outcomes';

export { closedByText, tryWord };

/** A hole as the project's data document states it (prove2me-current/2): in place or extracted as a lemma, and its
 * standing when the trial's records know it without a task of this outline (`by`: prepass, index, ledger,
 * counterexample) or from a verdict outside a task (`by`: model or check, with a note). */
export interface HoleInfo {
	hole: number;
	kind: 'lemma' | 'inplace';
	state: 'open' | 'proved' | 'false';
	by?: string;
	note?: string;
}
export interface OutlineHoles {
	outline: number;
	holes: HoleInfo[];
}

export type HoleState = 'proved' | 'false' | 'prepass' | 'index' | 'ledger' | 'closed' | 'running' | 'queued' | 'waiting' | 'not_tried' | 'cancelled' | 'not_proved' | 'failed' | 'unknown';
/** What the page shows of a hole: three states only. */
export type HoleShow = 'waiting' | 'working' | 'solved';
export interface HoleView {
	/** The hole's number in the outline; null for the in-place holes' row when their numbers are not known, and for the assembly. */
	hole: number | null;
	label: string;
	kind: 'lemma' | 'inplace' | 'assembly';
	/** A hole's: waiting, working or solved; the assembly's own state. */
	state: HoleShow | HoleState;
	/** "Waiting", "Working (try 1, 14 steps so far)", "Solved"; the assembly's in words. */
	text: string;
	/** The detail, for a tooltip only: why it waits ("starts when hole 3 is done"), or how it was solved
	 * ("solved automatically: simp", "already known: X", "with the outline", "own task"). Empty when there is none. */
	title: string;
}
export type OutlineState = 'queued' | 'writing' | 'in_progress' | 'proved' | 'refuted' | 'abandoned' | 'ended' | 'stopped';
export interface OutlineView {
	gen: number;
	state: OutlineState;
	/** The state in words: "in progress", "refuted: hole 2 is false", "proved (assembled)". */
	text: string;
	/** Why, alone ("hole 2 is false"), for the leaf's line; empty when the state says it all. */
	reason: string;
	/** The outline task's own steps and model minutes: "written in 28 steps · 12.8 min". */
	cost: string;
	holes: HoleView[];
}
export interface LeafPlan {
	summary: string;
	outlines: OutlineView[];
}

/** "hole 2", "holes 2 and 3", "holes 1, 2 and 4". */
export function holesText(ns: number[]): string {
	const s = [...ns].sort((a, b) => a - b).map(String);
	if (s.length <= 1) return `hole ${s[0] ?? ''}`;
	return `holes ${s.slice(0, -1).join(', ')} and ${s.at(-1)}`;
}
/** A hole as its outline's tasks name it (the label `hole_states`: "1=wait:3 2=task 3=prepass:simp 4=inplace"). */
export interface HolePlan {
	state: 'task' | 'wait' | 'busy' | 'prepass' | 'known' | 'reused' | 'false' | 'inplace' | 'unknown';
	/** wait: the hole it waits for, or "inplace"; prepass: the tactic; known: the theorem's name. */
	arg?: string;
}
const planWord = /^(task|busy|reused|false|inplace|wait:(?:[1-9][0-9]{0,2}|inplace)|(?:prepass|known)(?::[A-Za-z0-9_.'?!]{1,80})?)$/;
/** The label `hole_states` as hole number -> state; a word the page does not know is "unknown"; null without one. */
export function parseHoleStates(v: string | undefined | null): Map<number, HolePlan> | null {
	if (!v || v.length > 200) return null;
	const out = new Map<number, HolePlan>();
	for (const tok of v.split(' ')) {
		const m = /^([1-9][0-9]{0,2})=(.+)$/.exec(tok);
		if (!m) continue;
		const [state, ...rest] = planWord.test(m[2]) ? m[2].split(':') : ['unknown'];
		out.set(Number(m[1]), { state: state as HolePlan['state'], ...(rest.length ? { arg: rest.join(':') } : {}) });
	}
	return out.size ? out : null;
}

/** A hole without a task of its own, as its outline's tasks name it, when that settles it. */
function planHole(p: HolePlan): { state: HoleState; text: string } | null {
	if (p.state === 'prepass') return { state: 'prepass', text: p.arg ? `solved automatically: ${p.arg}` : 'solved automatically' };
	if (p.state === 'known') return { state: 'index', text: p.arg ? `already known: ${p.arg}` : 'already known' };
	if (p.state === 'reused') return { state: 'ledger', text: 'proved earlier' };
	if (p.state === 'false') return { state: 'false', text: 'false: a counterexample was found' };
	return null;
}

const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);

/** A hole as the document states it, when that settles it (a verdict, or closed without a task). */
function docHole(e: HoleInfo): { state: HoleState; text: string } | null {
	const note = e.note ? `; ${e.note}` : '';
	if (e.state === 'false') {
		const how = e.by === 'counterexample' ? 'false: a counterexample was found before any task' : e.by === 'model' ? 'false, disproved by the model' : e.by === 'check' ? 'false, disproved' : 'false';
		return { state: 'false', text: how + note };
	}
	if (e.state === 'proved') {
		if (e.by === 'prepass') return { state: 'prepass', text: `closed by the pre-pass${note}` };
		if (e.by === 'index') return { state: 'index', text: `closed by the index (reuse)${note}` };
		if (e.by === 'ledger') return { state: 'ledger', text: `closed by the ledger${note}` };
		if (e.note) return { state: 'proved', text: `proved${note}` };
	}
	return null;
}

const byTry = (a: TaskRow, b: TaskRow) => a.attempt - b.attempt;
/** The states a hole counts as solved in (by any route). */
const SOLVED: HoleState[] = ['proved', 'prepass', 'index', 'ledger', 'closed'];

/** A hole's (or the in-place holes') tasks in words; `why` names the outline's end for a try that was cancelled or never
 * made, unless the try's own `closed_by` names the task that ended the outline (`peers`: the leaf's tasks). */
function triesText(ts: TaskRow[], why: string, peers: TaskRow[] = []): { state: HoleState; text: string } {
	const sorted = [...ts].sort(byTry);
	const last = sorted.at(-1)!;
	// what closed the hole before a model ran; a cancelled try's closed_by is why it was cancelled (below)
	const closed = sorted.find((t) => t.closed_by && !isStaleCancel(attemptOf(t)));
	if (closed?.closed_by) return closedByText(closed.closed_by);
	const before = sorted.slice(0, -1).filter((t) => t.state === 'finished' || t.state === 'failed');
	const earlier = before.length ? before.map((t) => `try ${t.attempt}: ${t.state === 'failed' ? 'a fault' : tryWord(t.outcome)}`).join(', ') : '';
	const a = last.attempt;
	switch (last.state) {
		case 'running':
			return { state: 'running', text: `running (try ${a}${last.steps !== null ? `, ${last.steps} steps so far` : ''})${earlier ? `; ${earlier}` : ''}` };
		case 'queued':
			return { state: 'queued', text: `${a > 1 ? `queued (try ${a})` : 'queued'}${earlier ? `; ${earlier}` : ''}` };
		case 'failed':
			return { state: 'failed', text: `stopped by a fault (try ${a})${earlier ? `; ${earlier}` : ''}` };
		case 'cancelled': {
			const started = last.started ?? (last.steps ?? 0) > 0;
			const word = started ? `cancelled${last.steps ? ` after ${last.steps} steps` : ''}` : 'not tried';
			// "cancelled: outline abandoned after hole 1 failed" (the task its closed_by names), else the outline's end
			const own = staleCancelReason(attemptOf(last), peers.map(attemptOf));
			const tail = own ? `${word}: ${own}` : `${word}${why ? ` (${why})` : ''}`;
			// after earlier tries: "try 1: our check failed; try 2 not tried (outline refuted)"
			return { state: started ? 'cancelled' : 'not_tried', text: earlier ? `${earlier}; try ${a} ${tail}` : a > 1 ? `try ${a} ${tail}` : tail };
		}
	}
	if (last.outcome === 'proved') return { state: 'proved', text: a > 1 ? `proved in try ${a}${earlier ? ` (${earlier})` : ''}` : 'proved' };
	if (last.outcome === 'disproved') return { state: 'false', text: `false, disproved by the model${a > 1 ? ` in try ${a}` : ''}${earlier ? ` (${earlier})` : ''}` };
	if (sorted.length > 1) {
		const all = sorted.filter((t) => t.state === 'finished').map((t) => tryWord(t.outcome));
		return { state: 'not_proved', text: `not proved in ${a} tries (${all.join(', then ')})` };
	}
	return { state: 'not_proved', text: `not proved: ${tryWord(last.outcome)}${a > 1 ? ` (try ${a})` : ''}` };
}

const cost = (sk: TaskRow | undefined) => {
	if (!sk || sk.steps === null) return '';
	if (sk.state === 'running') return `${sk.steps} steps so far`;
	return `written in ${sk.steps} steps${sk.minutes !== null ? ` · ${sk.minutes.toFixed(1)} min` : ''}`;
};

/** One outline in words: its state and one row per hole (and the in-place holes and the assembly). */
export function outlineView(gen: number, rows: TaskRow[], o: Outline | undefined, doc: HoleInfo[] | undefined, newer: boolean, peers: TaskRow[] = rows): OutlineView {
	const newest = (role: string) => rows.filter((r) => r.role === role).sort(byTry).at(-1);
	const sk = newest('skeleton');
	const asm = newest('asm');
	const view = (state: OutlineState, text: string, reason = '', holes: HoleView[] = []): OutlineView => ({ gen, state, text, reason, cost: cost(sk), holes });
	if (sk && sk.state === 'queued') return view('queued', 'queued');
	if (sk && sk.state === 'running') return view('writing', 'being written');
	if (sk && sk.state === 'failed') return view('stopped', 'stopped by a fault');
	if (sk && sk.state === 'cancelled') {
		const started = sk.started ?? (sk.steps ?? 0) > 0;
		return view('abandoned', started ? 'abandoned: cancelled while being written' : 'abandoned: cancelled before it started', 'cancelled');
	}
	if (sk && provesLeaf(attemptOf(sk))) return view('proved', 'proved without holes');
	if (sk && sk.outcome && sk.outcome !== 'outline') {
		const r = outlineRefusal(sk.outcome);
		return r ? view('refuted', `refuted: ${r}`, r) : view('ended', `no outline: ${tryWord(sk.outcome)}`, tryWord(sk.outcome));
	}

	// The holes: which are lemmas (a task of their own) and which stay in place (one task for them all).
	const holeTasks = new Map<number, TaskRow[]>();
	for (const r of rows) if (r.role === 'hole' && r.hole) holeTasks.set(r.hole, [...(holeTasks.get(r.hole) ?? []), r]);
	const rest = rows.filter((r) => r.role === 'rest');
	const docBy = new Map((doc ?? []).map((h) => [h.hole, h]));
	// the outline's holes as its tasks name them (the data document's, when it has them, come first)
	const plan = doc?.length ? null : parseHoleStates(rows.find((r) => r.hole_states)?.hole_states);
	let lemmas: number[], inplace: number[] | null, inplaceCount: number | null;
	if (doc?.length) {
		lemmas = doc.filter((h) => h.kind === 'lemma').map((h) => h.hole);
		inplace = doc.filter((h) => h.kind === 'inplace').map((h) => h.hole);
		for (const h of holeTasks.keys()) if (!lemmas.includes(h)) lemmas.push(h);
		inplaceCount = inplace.length;
	} else if (plan) {
		inplace = [...plan].filter(([h, p]) => p.state === 'inplace' && !holeTasks.has(h)).map(([h]) => h).sort((a, b) => a - b);
		lemmas = [...new Set([...plan.keys(), ...holeTasks.keys()])].filter((h) => !inplace!.includes(h));
		inplaceCount = inplace.length;
	} else {
		const known = new Set([...holeTasks.keys(), ...(o?.prepass_holes ?? []).map((p) => p.hole)]);
		lemmas = [...known];
		inplaceCount = o ? o.inplace : null;
		// every lemma hole known and the outline's size known: the others are the ones in place
		inplace = o && o.lemmas === known.size && o.holes === known.size + o.inplace ? Array.from({ length: o.holes }, (_, i) => i + 1).filter((n) => !known.has(n)) : null;
	}
	lemmas.sort((a, b) => a - b);
	// Nothing known of the outline but its tasks: a number below the highest hole task without a task of its own is not
	// known (closed before the hole tasks, held back, or left in place).
	const gaps = !doc?.length && !o && !plan && lemmas.length ? Array.from({ length: lemmas.at(-1)! }, (_, i) => i + 1).filter((n) => !lemmas.includes(n)) : [];
	const pp = new Map((o?.prepass_holes ?? []).map((p) => [p.hole, p]));

	// First pass: each hole's own state (a cancelled or untried one gets the outline's reason in the second pass).
	type Draft = { hole: number | null; kind: 'lemma' | 'inplace'; ts: TaskRow[]; fixed?: { state: HoleState; text: string } };
	const drafts: Draft[] = lemmas.map((h) => {
		const e = docBy.get(h);
		const fromDoc = e ? docHole(e) : null;
		if (fromDoc) return { hole: h, kind: 'lemma', ts: [], fixed: fromDoc };
		const ts = holeTasks.get(h) ?? [];
		const fromPlan = !ts.length && plan?.has(h) ? planHole(plan.get(h)!) : null;
		if (fromPlan) return { hole: h, kind: 'lemma', ts, fixed: fromPlan };
		if (!ts.length) {
			const p = pp.get(h);
			if (p?.closed_by) return { hole: h, kind: 'lemma', ts, fixed: p.closed_by.startsWith('formalpedia:') ? { state: 'index', text: 'closed by the index (reuse)' } : { state: 'prepass', text: 'closed by the pre-pass' } };
		}
		return { hole: h, kind: 'lemma', ts };
	});
	const anyInplace = (inplaceCount ?? 0) > 0 || rest.length > 0 || (inplace?.length ?? 0) > 0;
	if (anyInplace) {
		// The in-place task never says "proved": the assembly's full check decides (prove2me-outcomes.ts).
		const byAsm: Draft['fixed'] = inplaceStanding([...rest].sort(byTry).at(-1), asm);
		// a verdict the data document holds for a hole in place (e.g. false, found outside a task) comes first
		const fixedFor = (h: number | null) => (h !== null && docBy.has(h) ? docHole(docBy.get(h)!) : null) ?? byAsm;
		// one row per hole in place, as many as two; more share one row (they are one task)
		// every hole in place whose number is known has its row
		if (inplace?.length) for (const h of inplace) drafts.push({ hole: h, kind: 'inplace', ts: rest, fixed: fixedFor(h) });
		else drafts.push({ hole: null, kind: 'inplace', ts: rest, fixed: byAsm });
	}
	const own = (d: Draft) => d.fixed ?? (d.ts.length ? triesText(d.ts, '', peers) : null);
	const states = drafts.map(own);
	const live = [...rows.filter((r) => r.role !== 'skeleton')].some((r) => r.state === 'running' || r.state === 'queued');

	// The outline's state from its holes.
	const falseHoles = drafts.filter((d, i) => d.kind === 'lemma' && states[i]?.state === 'false').map((d) => d.hole!);
	const failed = drafts.filter((d, i) => states[i]?.state === 'not_proved');
	const failedText = () => {
		const hs = failed.filter((d) => d.kind === 'lemma').map((d) => d.hole!);
		const parts = [hs.length ? holesText(hs) : '', failed.some((d) => d.kind === 'inplace') ? 'the holes in place' : ''].filter(Boolean);
		const tries = [...new Set(failed.filter((d) => d.kind === 'lemma').map((d) => Math.max(...d.ts.map((t) => t.attempt))))];
		return `${andList(parts)} not proved${tries.length === 1 && tries[0] > 1 && !failed.some((d) => d.kind === 'inplace') ? ` in ${tries[0]} tries` : ''}`;
	};
	const cancelled = rows.some((r) => r.role !== 'skeleton' && r.state === 'cancelled');
	let state: OutlineState, text = '', reason = '';
	if (asm && provesLeaf(attemptOf(asm))) [state, text] = ['proved', 'proved (assembled)'];
	else if (falseHoles.length) [state, reason] = ['refuted', `${holesText(falseHoles)} ${falseHoles.length > 1 ? 'are' : 'is'} false`];
	else if (live) [state, text] = ['in_progress', newer ? 'in progress (a newer outline was started)' : asm?.state === 'running' ? 'being assembled' : 'in progress'];
	else if (newer) [state, reason] = ['abandoned', failed.length ? failedText() : cancelled ? 'its tasks were cancelled' : ''];
	else if (asm && asm.state === 'finished') [state, reason] = ['ended', 'the assembly did not check'];
	else if (asm && asm.state === 'failed') [state, reason] = ['stopped', 'the assembly stopped by a fault'];
	else if (cancelled) [state, reason] = ['abandoned', 'its tasks were cancelled'];
	else if (failed.length) [state, reason] = ['ended', failedText()];
	else [state, text] = ['ended', 'ended'];
	if (state === 'refuted') text = `refuted: ${reason}`;
	if (state === 'abandoned') text = reason ? `abandoned: ${reason}` : 'abandoned';
	if (state === 'ended' && reason) text = `ended: ${reason}`;
	if (state === 'stopped') text = reason;
	const why = { refuted: 'outline refuted', abandoned: 'outline abandoned', proved: 'not needed', ended: 'outline ended', stopped: 'outline stopped' }[state as string] ?? '';
	const ended = state !== 'in_progress';

	const stateOf = (h: number) => states[drafts.findIndex((d) => d.kind === 'lemma' && d.hole === h)]?.state;
	/** A lemma hole without a task, as the outline's tasks name it: held back, an earlier outline's, or its own to come. */
	const planWait = (p: HolePlan): { state: HoleState; text: string } => {
		if (p.state === 'wait') {
			const k = p.arg === 'inplace' ? null : Number(p.arg);
			if (!ended) return { state: 'waiting', text: `starts when ${k === null ? 'the holes in place are' : `hole ${k} is`} done` };
			const kState = k === null ? undefined : stateOf(k);
			const lost = k !== null && kState !== undefined && !SOLVED.includes(kState);
			return { state: 'not_tried', text: lost ? `hole ${k} was not solved` : why };
		}
		if (p.state === 'busy') return { state: 'waiting', text: 'a task of an earlier outline works on it' };
		if (p.state === 'task') return ended ? { state: 'not_tried', text: `its own task never started${why ? ` (${why})` : ''}` } : { state: 'waiting', text: 'its own task is not made yet' };
		return { state: 'unknown', text: 'not known yet' };
	};
	// what an ended outline's waiting holes wait for: a newer outline
	const newerLive = peers.some((r) => r.gen > gen && (r.state === 'running' || r.state === 'queued'));
	const next = !ended || state === 'proved' ? '' : newerLive ? 'a new outline is being made' : newer ? 'a newer outline replaced this one' : '';
	/** The three states the page shows, and the detail for the tooltip. */
	const show = (d: Draft, s: { state: HoleState; text: string }, pre = ''): { state: HoleShow; text: string; title: string } => {
		const tip = (t: string) => (t ? pre + t : pre.replace(/, $/, ''));
		const last = [...d.ts].sort(byTry).at(-1);
		if (SOLVED.includes(s.state)) {
			// a task's closed_by names the tactic or the theorem: reuse:prepass:simp, reuse:formalpedia:X.y
			const by = d.ts.find((t) => t.closed_by?.startsWith('reuse:'))?.closed_by?.split(':').slice(2).join(':');
			const route =
				s.state === 'prepass' ? (s.text.startsWith('solved automatically') ? s.text : by ? `solved automatically: ${by}` : 'solved automatically') :
				s.state === 'index' ? (s.text.startsWith('already known') ? s.text : by ? `already known: ${by}` : 'already known') :
				s.state === 'ledger' ? 'proved earlier' :
				s.state === 'closed' ? s.text :
				d.kind === 'inplace' ? 'with the outline' :
				last ? `own task${last.attempt > 1 ? ` (try ${last.attempt})` : ''}` : s.text;
			return { state: 'solved', text: 'Solved', title: tip(route) };
		}
		const earlier = s.text.includes('; ') ? s.text.slice(s.text.indexOf('; ') + 2) : '';
		if (last && (s.state === 'running' || s.state === 'queued') && (last.state === 'running' || last.state === 'queued')) {
			const a = last.attempt;
			const detail = last.state === 'running' ? `try ${a}${last.steps !== null ? `, ${last.steps} steps so far` : ''}` : a > 1 ? `queued, try ${a}` : 'queued';
			return { state: 'working', text: `Working (${detail})`, title: tip(earlier) };
		}
		// the holes in place, filled in: the assembly checks them now
		if (d.kind === 'inplace' && s.state === 'waiting' && d.ts.length) return { state: 'working', text: 'Working (the assembly checks it)', title: tip('') };
		return { state: 'waiting', text: 'Waiting', title: tip([s.text, next].filter(Boolean).join('; ')) };
	};
	const holes: HoleView[] = drafts.map((d, i) => {
		let s = states[i];
		if (s && (s.state === 'cancelled' || s.state === 'not_tried')) s = triesText(d.ts, why, peers);
		const p = d.kind === 'lemma' && d.hole !== null ? plan?.get(d.hole) : undefined;
		if (!s && p) s = planWait(p);
		if (!s) {
			// no task: in place before the in-place task exists, or a lemma hole with none
			if (d.kind === 'inplace') s = ended ? { state: 'not_tried', text: `not tried${why ? ` (${why})` : ''}` } : { state: 'waiting', text: 'its task is not made yet' };
			else if (!pp.has(d.hole!) && o && !doc?.length) s = { state: 'closed', text: 'closed before the pre-pass (proved before, or disproved)' };
			else if (state === 'proved') s = { state: 'closed', text: 'closed without a task of its own' };
			else s = ended ? { state: 'not_tried', text: `not tried${why ? ` (${why})` : ''}` } : { state: 'waiting', text: 'its task is not made yet' };
		}
		if (d.kind === 'inplace' && d.hole === null) {
			// the holes in place, their numbers not known: one row
			const n = inplaceCount;
			const pre = n && n > 1 ? `${n} holes left in the outline’s file, ` : n === 1 ? '1 hole left in the outline’s file, ' : '';
			return { hole: null, label: 'In place', kind: 'inplace', ...show(d, s, pre) };
		}
		return { hole: d.hole, label: `Hole ${d.hole}`, kind: d.kind, ...show(d, s, d.kind === 'inplace' ? 'in place, ' : '') };
	});
	for (const h of gaps) holes.push({ hole: h, label: `Hole ${h}`, kind: 'lemma', state: 'waiting', text: 'Waiting', title: 'not known yet' });
	holes.sort((a, b) => (a.hole ?? 1e9) - (b.hole ?? 1e9));
	if (asm) {
		const asmText: Record<string, string> = { running: 'running', queued: 'queued', failed: 'stopped by a fault', cancelled: 'cancelled' };
		const t = asmText[asm.state] ?? (asm.outcome === 'proved' ? (asm.attempt > 1 ? `proved: our full check passed (try ${asm.attempt})` : 'proved: our full check passed') : `did not check${asm.attempt > 1 ? ` (try ${asm.attempt})` : ''}`);
		const st: HoleState = asm.state === 'finished' ? (asm.outcome === 'proved' ? 'proved' : 'not_proved') : asm.state === 'running' ? 'running' : asm.state === 'queued' ? 'queued' : asm.state === 'failed' ? 'failed' : 'cancelled';
		holes.push({ hole: null, label: 'Assembly', kind: 'assembly', state: st, text: t, title: '' });
	}
	return view(state, text, reason, holes);
}

/** The parts of an outline that run (or wait): "outline 2, hole 1", "outline 2 (being written)", "outline 1, holes 2 and 3, in place". */
function partsIn(gen: number, rows: TaskRow[], state: string): string {
	const mine = rows.filter((r) => r.gen === gen && r.state === state);
	if (!mine.length) return '';
	if (mine.some((r) => r.role === 'skeleton')) return `outline ${gen}${state === 'running' ? ' (being written)' : ''}`;
	const hs = [...new Set(mine.filter((r) => r.role === 'hole' && r.hole).map((r) => r.hole!))];
	const parts = [hs.length ? holesText(hs) : '', mine.some((r) => r.role === 'rest') ? 'the holes in place' : '', mine.some((r) => r.role === 'asm') ? 'the assembly' : ''].filter(Boolean);
	return `outline ${gen}${parts.length ? `, ${andList(parts)}` : ''}`;
}

/** The leaf in one line: "Running: outline 2, hole 1"; "Proved: outline 1 assembled"; "Closed by reuse: imports X". */
export function leafSummary(views: OutlineView[], rows: TaskRow[], reuse?: string): string {
	if (!rows.length) return 'Not started yet';
	const proved = views.find((v) => v.state === 'proved');
	if (proved) {
		if (reuse) return `Closed by reuse: imports ${reuse}`;
		return proved.text === 'proved (assembled)' ? `Proved: outline ${proved.gen} assembled` : `Proved: outline ${proved.gen}, without holes`;
	}
	const gens = [...new Set(rows.map((r) => r.gen))].sort((a, b) => b - a);
	for (const [state, word] of [['running', 'Running'], ['queued', 'Queued']] as const) {
		const parts = gens.map((g) => partsIn(g, rows, state)).filter(Boolean);
		if (parts.length) return `${word}: ${parts.join('; ')}`;
	}
	const v = views.reduce((a, b) => (b.gen > a.gen ? b : a));
	const o = `outline ${v.gen}`;
	switch (v.state) {
		case 'refuted':
			return `Not proved: ${o} was refuted (${v.reason})`;
		case 'abandoned':
			return `Stopped: ${o} was abandoned${v.reason ? ` (${v.reason})` : ''}`;
		case 'stopped':
			return `Stopped: ${o} ${v.text}`;
		default:
			return `Not proved: ${o} ${v.reason ? `ended (${v.reason})` : 'ended'}`;
	}
}

/** The leaf's outlines, newest first, and its line. `doc` is the data document's per-outline holes for this leaf;
 * `reuse` the proved theorem the leaf's accepted proof imports, when it was closed by reuse. */
export function leafPlan(d: LeafDetail, doc: OutlineHoles[] = [], reuse?: string): LeafPlan {
	const gens = [...new Set([...d.rows.map((r) => r.gen)])].sort((a, b) => a - b);
	const top = gens.at(-1) ?? 0;
	const views = gens.map((g) =>
		outlineView(g, d.rows.filter((r) => r.gen === g), d.outlines.find((o) => o.gen === g), doc.find((x) => x.outline === g)?.holes, g < top, d.rows),
	);
	return { summary: leafSummary(views, d.rows, reuse), outlines: [...views].reverse() };
}

const esc = (v: unknown) => String(v).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

/** The outlines as HTML: per outline a heading line (name, state, cost) and a list of its holes. With `summary`, the
 * leaf's line first (the trials' history; "Working on now" has it on the leaf's own line). */
export function leafPlanHTML(leaf: string, p: LeafPlan, summary = false): string {
	if (!p.outlines.length) return '';
	const outlines = p.outlines
		.map(
			(o) =>
				`<div class="p2m-outline" data-outline="${o.gen}" data-outline-state="${esc(o.state)}">` +
				`<p class="p2m-ohead"><span class="p2m-oname">Outline ${o.gen}</span><span class="p2m-ostate" data-outline-text>${esc(o.text)}</span>` +
				(o.cost ? `<span class="p2m-ocost" data-outline-cost>${esc(o.cost)}</span>` : '') +
				`</p>` +
				(o.holes.length
					? `<ul class="p2m-holes">${o.holes
							.map(
								(h) =>
									`<li data-hole="${h.hole ?? (h.kind === 'assembly' ? 'asm' : 'inplace')}" data-hole-kind="${h.kind}" data-hole-state="${esc(h.state)}"${h.title ? ` title="${esc(h.title)}"` : ''}>` +
									`<span class="p2m-hname">${esc(h.label)}</span><span class="p2m-htext">${esc(h.text)}</span></li>`,
							)
							.join('')}</ul>`
					: '') +
				`</div>`,
		)
		.join('');
	return `<div class="p2m-plan" data-leaf-plan="${esc(leaf)}">${summary ? `<p class="p2m-sum" data-leaf-summary>${esc(p.summary)}</p>` : ''}${outlines}</div>`;
}
