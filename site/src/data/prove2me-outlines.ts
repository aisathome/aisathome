// A leaf's outlines in words (owner, 2026-11-15: the task table and its one-line count were hard to follow). Newest
// outline first, each with its state (in progress, refuted and why, proved, abandoned and why), one row per hole with
// its state (proved, false, closed by the pre-pass, the index or the ledger, running with its live steps, queued, not
// tried, cancelled), the holes left in the outline's file ("in place") and the assembly; above them one line for the
// whole leaf ("Running: outline 2, hole 1"). The task table stays below as the detail.
//
// Made from the leaf's tasks (their roles, outlines, holes, tries, states and outcomes, and `closed_by`: GET /v1/tasks
// or the build's data), the build's outline records (holes, how many in place, what the pre-pass closed) and, when the
// project's data document has them, its per-hole states: which holes are in place, what closed a hole without a task,
// and a verdict reached outside a task (a hole re-judged false after our check failed on the model's disproof). Pure
// functions and one HTML renderer (every value escaped) for the build and the browser alike.
//
// SPDX-License-Identifier: Apache-2.0
import type { LeafDetail, Outline, TaskRow } from './prove2me-live';

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

export type HoleState = 'proved' | 'false' | 'prepass' | 'index' | 'ledger' | 'closed' | 'running' | 'queued' | 'waiting' | 'not_tried' | 'cancelled' | 'not_proved' | 'failed';
export interface HoleView {
	/** The hole's number in the outline; null for the in-place holes' row when their numbers are not known, and for the assembly. */
	hole: number | null;
	label: string;
	kind: 'lemma' | 'inplace' | 'assembly';
	state: HoleState;
	text: string;
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
};
const tryWord = (o: string | null) => (o ? tryWords[o] ?? o.replace(/[_-]/g, ' ') : 'ended');
// An outline task that ended without an outline: refuted (the outline's own shape) or no outline at all.
const refusedOutline: Record<string, string> = { degenerate_outline: 'degenerate (a hole is the leaf’s own claim)', inline_proof: 'an inline proof too long' };

/** "hole 2", "holes 2 and 3", "holes 1, 2 and 4". */
export function holesText(ns: number[]): string {
	const s = [...ns].sort((a, b) => a - b).map(String);
	if (s.length <= 1) return `hole ${s[0] ?? ''}`;
	return `holes ${s.slice(0, -1).join(', ')} and ${s.at(-1)}`;
}
/** "holes 2 to 12", "holes 1, 3 and 5", "holes 2 to 4 and 7". */
export function rangesText(ns: number[]): string {
	const s = [...ns].sort((a, b) => a - b);
	const runs: string[] = [];
	for (let i = 0; i < s.length; ) {
		let j = i;
		while (j + 1 < s.length && s[j + 1] === s[j] + 1) j++;
		runs.push(j - i >= 2 ? `${s[i]} to ${s[j]}` : j > i ? `${s[i]}, ${s[j]}` : `${s[i]}`);
		i = j + 1;
	}
	const parts = runs.join(', ').split(', ');
	return `hole${s.length > 1 ? 's' : ''} ${parts.length > 1 ? `${parts.slice(0, -1).join(', ')} and ${parts.at(-1)}` : parts[0]}`;
}
const andList = (xs: string[]) => (xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);

/** What a task's `closed_by` says: the index's theorem, the ledger's proof, the pre-pass, or another task. */
export function closedByText(c: string): { state: HoleState; text: string } {
	if (c.startsWith('reuse:formalpedia:')) return { state: 'index', text: 'closed by the index (reuse)' };
	if (c.startsWith('reuse:ledger')) return { state: 'ledger', text: 'closed by the ledger' };
	if (c.startsWith('reuse:prepass')) return { state: 'prepass', text: 'closed by the pre-pass' };
	if (c.startsWith('reuse:')) return { state: 'closed', text: 'closed by reuse' };
	if (/-s[0-9]+$|^t[0-9]+-p2m-[0-9a-f]+$/.test(c)) return { state: 'prepass', text: 'closed by the pre-pass' }; // run by that outline task
	return { state: 'closed', text: `closed by another task (${c})` };
}

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

/** A hole's (or the in-place holes') tasks in words; `why` names the outline's end for a try that was cancelled or never made. */
function triesText(ts: TaskRow[], why: string): { state: HoleState; text: string } {
	const sorted = [...ts].sort(byTry);
	const last = sorted.at(-1)!;
	const closed = sorted.find((t) => t.closed_by);
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
			const tail = `${word}${why ? ` (${why})` : ''}`;
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
export function outlineView(gen: number, rows: TaskRow[], o: Outline | undefined, doc: HoleInfo[] | undefined, newer: boolean): OutlineView {
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
	if (sk && sk.outcome === 'proved') return view('proved', 'proved without holes');
	if (sk && sk.outcome && sk.outcome !== 'outline') {
		const r = refusedOutline[sk.outcome];
		return r ? view('refuted', `refuted: ${r}`, r) : view('ended', `no outline: ${tryWord(sk.outcome)}`, tryWord(sk.outcome));
	}

	// The holes: which are lemmas (a task of their own) and which stay in place (one task for them all).
	const holeTasks = new Map<number, TaskRow[]>();
	for (const r of rows) if (r.role === 'hole' && r.hole) holeTasks.set(r.hole, [...(holeTasks.get(r.hole) ?? []), r]);
	const rest = rows.filter((r) => r.role === 'rest');
	const docBy = new Map((doc ?? []).map((h) => [h.hole, h]));
	let lemmas: number[], inplace: number[] | null, inplaceCount: number | null;
	if (doc?.length) {
		lemmas = doc.filter((h) => h.kind === 'lemma').map((h) => h.hole);
		inplace = doc.filter((h) => h.kind === 'inplace').map((h) => h.hole);
		for (const h of holeTasks.keys()) if (!lemmas.includes(h)) lemmas.push(h);
		inplaceCount = inplace.length;
	} else {
		const known = new Set([...holeTasks.keys(), ...(o?.prepass_holes ?? []).map((p) => p.hole)]);
		lemmas = [...known];
		inplaceCount = o ? o.inplace : null;
		// every lemma hole known and the outline's size known: the others are the ones in place
		inplace = o && o.lemmas === known.size && o.holes === known.size + o.inplace ? Array.from({ length: o.holes }, (_, i) => i + 1).filter((n) => !known.has(n)) : null;
	}
	lemmas.sort((a, b) => a - b);
	// Nothing known of the outline but its tasks: a number below the highest hole task without a task of its own is
	// either closed before the hole tasks or left in place.
	const gaps = !doc?.length && !o && lemmas.length ? Array.from({ length: lemmas.at(-1)! }, (_, i) => i + 1).filter((n) => !lemmas.includes(n)) : [];
	const pp = new Map((o?.prepass_holes ?? []).map((p) => [p.hole, p]));

	// First pass: each hole's own state (a cancelled or untried one gets the outline's reason in the second pass).
	type Draft = { hole: number | null; kind: 'lemma' | 'inplace'; ts: TaskRow[]; fixed?: { state: HoleState; text: string } };
	const drafts: Draft[] = lemmas.map((h) => {
		const e = docBy.get(h);
		const fromDoc = e ? docHole(e) : null;
		if (fromDoc) return { hole: h, kind: 'lemma', ts: [], fixed: fromDoc };
		const ts = holeTasks.get(h) ?? [];
		if (!ts.length) {
			const p = pp.get(h);
			if (p?.closed_by) return { hole: h, kind: 'lemma', ts, fixed: p.closed_by.startsWith('formalpedia:') ? { state: 'index', text: 'closed by the index (reuse)' } : { state: 'prepass', text: 'closed by the pre-pass' } };
		}
		return { hole: h, kind: 'lemma', ts };
	});
	const anyInplace = (inplaceCount ?? 0) > 0 || rest.length > 0 || (inplace?.length ?? 0) > 0;
	if (anyInplace) {
		// The in-place task never says "proved": it ends `rest_done` with the outline's file filled in, and the
		// assembly's full check decides. So its holes are proved when the assembly is, and wait for it until then.
		const last = [...rest].sort(byTry).at(-1);
		const done = last?.state === 'finished' && last.outcome === 'rest_done';
		const byAsm: Draft['fixed'] = !done
			? undefined
			: asm?.state === 'finished'
				? asm.outcome === 'proved'
					? { state: 'proved', text: 'proved (checked in the assembly)' }
					: { state: 'not_proved', text: 'filled in, but the assembly did not check' }
				: { state: 'waiting', text: 'filled in; the assembly checks it' };
		// a verdict the data document holds for a hole in place (e.g. false, found outside a task) comes first
		const fixedFor = (h: number | null) => (h !== null && docBy.has(h) ? docHole(docBy.get(h)!) : null) ?? byAsm;
		// one row per hole in place, as many as two; more share one row (they are one task)
		if (inplace?.length && inplace.length <= 2) for (const h of inplace) drafts.push({ hole: h, kind: 'inplace', ts: rest, fixed: fixedFor(h) });
		else drafts.push({ hole: null, kind: 'inplace', ts: rest, fixed: byAsm });
	}
	const own = (d: Draft) => d.fixed ?? (d.ts.length ? triesText(d.ts, '') : null);
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
	if (asm && asm.state === 'finished' && asm.outcome === 'proved') [state, text] = ['proved', 'proved (assembled)'];
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

	const holes: HoleView[] = drafts.map((d, i) => {
		let s = states[i];
		if (s && (s.state === 'cancelled' || s.state === 'not_tried')) s = triesText(d.ts, why);
		if (!s) {
			// no task: in place before the in-place task exists, or a lemma hole with none
			if (d.kind === 'inplace') s = ended ? { state: 'not_tried', text: `not tried${why ? ` (${why})` : ''}` } : { state: 'waiting', text: 'waiting for its task' };
			else if (!pp.has(d.hole!) && o && !doc?.length) s = { state: 'closed', text: 'closed before the pre-pass (proved before, or disproved)' };
			else if (state === 'proved') s = { state: 'closed', text: 'closed without a task of its own' };
			else s = ended ? { state: 'not_tried', text: `not tried${why ? ` (${why})` : ''}` } : { state: 'waiting', text: 'waiting for its task' };
		}
		if (d.kind === 'inplace') {
			const n = inplace?.length ? null : inplaceCount;
			const pre =
				d.hole !== null ? 'in place, ' : inplace?.length ? `${rangesText(inplace)}, ` : n && n > 1 ? `${n} holes left in the outline’s file, ` : n === 1 ? '1 hole left in the outline’s file, ' : '';
			return { hole: d.hole, label: d.hole !== null ? `Hole ${d.hole}` : 'In place', kind: 'inplace', state: s.state, text: pre + s.text };
		}
		return { hole: d.hole, label: `Hole ${d.hole}`, kind: 'lemma', state: s.state, text: s.text };
	});
	for (const h of gaps) holes.push({ hole: h, label: `Hole ${h}`, kind: 'lemma', state: 'closed', text: 'no task of its own (closed before the hole tasks, or left in place)' });
	holes.sort((a, b) => (a.hole ?? 1e9) - (b.hole ?? 1e9));
	if (asm) {
		const asmText: Record<string, string> = { running: 'running', queued: 'queued', failed: 'stopped by a fault', cancelled: 'cancelled' };
		const t = asmText[asm.state] ?? (asm.outcome === 'proved' ? (asm.attempt > 1 ? `proved: our full check passed (try ${asm.attempt})` : 'proved: our full check passed') : `did not check${asm.attempt > 1 ? ` (try ${asm.attempt})` : ''}`);
		const st: HoleState = asm.state === 'finished' ? (asm.outcome === 'proved' ? 'proved' : 'not_proved') : asm.state === 'running' ? 'running' : asm.state === 'queued' ? 'queued' : asm.state === 'failed' ? 'failed' : 'cancelled';
		holes.push({ hole: null, label: 'Assembly', kind: 'assembly', state: st, text: t });
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
		outlineView(g, d.rows.filter((r) => r.gen === g), d.outlines.find((o) => o.gen === g), doc.find((x) => x.outline === g)?.holes, g < top),
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
									`<li data-hole="${h.hole ?? (h.kind === 'assembly' ? 'asm' : 'inplace')}" data-hole-kind="${h.kind}" data-hole-state="${esc(h.state)}">` +
									`<span class="p2m-hname">${esc(h.label)}</span><span class="p2m-htext">${esc(h.text)}</span></li>`,
							)
							.join('')}</ul>`
					: '') +
				`</div>`,
		)
		.join('');
	return `<div class="p2m-plan" data-leaf-plan="${esc(leaf)}">${summary ? `<p class="p2m-sum" data-leaf-summary>${esc(p.summary)}</p>` : ''}${outlines}</div>`;
}
