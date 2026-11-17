// The prove2me page's trials table: its helpers (the build's rows) and the rows of the trials the gateway has and the
// build does not (decision 0021, 2026-11-17: from trial 6 on the gateway is the record; trial 7 was missing from the page
// when trial 8 started). A trial's live row comes from its tasks in GET /v1/tasks once none of them is queued or
// running: its dates, scaffold, leaves proved of leaves run (by role), model hours and Qwen tokens. Its leaves' list and
// text come later, with the gateway's records of trials; the All row stays the build's. Pure
// functions and no data, so the browser's script can import them.
//
// SPDX-License-Identifier: Apache-2.0
import { isLiveTrial, type PublicTask, type TasksView } from './prove2me-current';

/** A trial's dates, short for a table: "7 Nov", "9–12 Nov", "30 Nov–2 Dec" (the year is in the trial's own list). */
export function shortSpan(t: { start: string; end: string }) {
	const m = (iso: string) => ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][new Date(`${iso}T00:00:00Z`).getUTCMonth()];
	const d = (iso: string) => new Date(`${iso}T00:00:00Z`).getUTCDate();
	if (t.start === t.end) return `${d(t.start)} ${m(t.start)}`;
	return m(t.start) === m(t.end) ? `${d(t.start)}–${d(t.end)} ${m(t.end)}` : `${d(t.start)} ${m(t.start)}–${d(t.end)} ${m(t.end)}`;
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
const compact = (v: number) => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v)));

type Count = { proved: number; runs: number };

/** A trial (or an outline probe round) of the gateway's tasks, as a row of the trials table. */
export interface LiveTrial {
	/** The table's label: "7", or "7p" for trial 7's outline probes. */
	trial: string;
	probe: boolean;
	/** The trial number (a probe round's: the trial it chose leaves for). */
	number: number;
	start: string;
	end: string;
	scaffold: string;
	all: Count;
	calibration: Count;
	open: Count;
	model_min: number;
	/** Prompt tokens with the cache reads in them, as the build's rows count them. */
	tok: Tok;
	/** False when a task that ran has no cache count: its tokens are then not all known ("at least"). */
	exact: boolean;
}

const ended = (t: PublicTask) => t.state === 'done' || t.state === 'failed' || t.state === 'cancelled';
// a leaf is proved by an outline without holes, or an assembly, that our full check passed (prove2me-live.ts detailProved)
const proves = (t: PublicTask) => t.state === 'done' && t.outcome === 'proved' && !['hole', 'rest'].includes(t.role ?? '');

/** The trials and probe rounds of the gateway's tasks that the build's trials table lacks (`built`: its labels, "6",
 * "7p"), each once all its tasks have ended, oldest first. A leaf counts as run when one of its tasks started. */
export function liveTrials(v: TasksView, built: string[]): LiveTrial[] {
	const by = new Map<string, PublicTask[]>();
	for (const t of v.tasks) {
		const label = t.labels.trial;
		if (!isLiveTrial(label) || built.includes(label.slice(1))) continue;
		by.set(label, [...(by.get(label) ?? []), t]);
	}
	const out: LiveTrial[] = [];
	for (const [label, ts] of by) {
		if (!ts.every(ended)) continue;
		const probe = label.endsWith('p');
		const leaves = new Map<string, PublicTask[]>();
		for (const t of ts) {
			const k = t.labels.leaf || t.labels.alias || '';
			if (k) leaves.set(k, [...(leaves.get(k) ?? []), t]);
		}
		const runs = [...leaves.values()].filter((l) => l.some((t) => t.started_at));
		const of = (g?: string): Count => {
			const rs = g ? runs.filter((l) => l.some((t) => t.labels.group === g)) : runs;
			return { proved: rs.filter((l) => l.some(proves)).length, runs: rs.length };
		};
		const scaffolds = ts.map((t) => t.labels.scaffold).filter(Boolean);
		const ran = ts.filter((t) => t.started_at || (t.steps ?? 0) > 0);
		const tok: Tok = { prompt_tokens: 0, cached_tokens: 0, output_tokens: 0 };
		for (const t of ts) {
			if (t.cached_tokens === null) continue;
			tok.prompt_tokens += (t.prompt_tokens ?? 0) + t.cached_tokens;
			tok.cached_tokens += t.cached_tokens;
			tok.output_tokens += t.completion_tokens ?? 0;
		}
		const day = (iso: string) => iso.slice(0, 10);
		const starts = ts.map((t) => day(t.started_at ?? t.created_at)).sort();
		const ends = ts.map((t) => day(t.finished_at ?? t.updated_at)).sort();
		out.push({
			trial: label.slice(1),
			probe,
			number: Number(label.slice(1, probe ? -1 : undefined)),
			start: starts[0],
			end: ends[ends.length - 1] < starts[0] ? starts[0] : ends[ends.length - 1],
			// the newest task's scaffold (a trial runs on one)
			scaffold: scaffolds[scaffolds.length - 1] ?? '',
			all: of(),
			calibration: of('calibration'),
			open: of('open'),
			model_min: ts.reduce((a, t) => a + (t.model_min ?? 0), 0),
			tok,
			exact: ran.every((t) => t.cached_tokens !== null),
		});
	}
	return out.sort((a, b) => a.number - (a.probe ? 0.5 : 0) - (b.number - (b.probe ? 0.5 : 0)));
}

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

/** A live trial's row of the trials table: the build's cells (dashes for a probe round's counts), the trial's name not a
 * link (it has no list of its own yet), marked as from the gateway. */
export function liveTrialHTML(r: LiveTrial): string {
	const cnt = (c: Count, dashIfNone = false) => (r.probe || (dashIfNone && !c.runs) ? '–' : `${c.proved} of ${c.runs}`);
	const total = qwenTokens(r.tok);
	const title = `${r.exact ? '' : 'At least: '}${tokTitle(r.tok)}`;
	const name = r.probe ? `Trial ${r.number} probes` : `Trial ${r.number}`;
	return `<tr data-trial="${esc(r.trial)}" data-source="live"${r.probe ? ' data-probes' : ''}>
<th scope="row"><span title="From the gateway’s task records">${esc(name)}</span><span class="sub">live</span></th>
<td data-label="Dates" class="nowrap"><time datetime="${esc(r.start)}">${esc(shortSpan(r))}</time></td>
<td data-label="Scaffold">${esc(r.scaffold || '–')}</td>
<td data-label="Proved" class="num" data-cell="proved">${cnt(r.all)}</td>
<td data-label="Calibration" class="num" data-cell="calibration">${cnt(r.calibration, true)}</td>
<td data-label="Open" class="num" data-cell="open">${cnt(r.open)}</td>
<td data-label="Model hours" class="num">${(r.model_min / 60).toFixed(1)}</td>
<td data-label="Qwen tokens" class="num tok" data-tokens="${total}" data-output="${r.tok.output_tokens}" data-cached="${r.tok.cached_tokens}" title="${esc(title)}">${r.exact ? '' : '≥ '}${compact(total)}</td>
</tr>`;
}

/** The note under the table while it has live rows: which trials, and that the All row does not count them yet. */
export function liveTrialsNote(rows: LiveTrial[]): string {
	if (!rows.length) return '';
	const names = rows.map((r) => (r.probe ? `trial ${r.number}’s outline probes` : `trial ${r.number}`));
	const list = names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : names[0];
	return `Live from the gateway’s task records: ${list}. The All row and the counts at the top of the page do not include ${rows.length > 1 ? 'them' : 'it'} yet.`;
}
