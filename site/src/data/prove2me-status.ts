// The status block at the top of /science/prove2me/: where the work stands now, from the gateway's public records.
//
//  - the queue bar: the current trial's tasks by state (proved, ended otherwise, stopped, running, waiting), from
//    GET /v1/tasks?project=prove2me; the trial from GET /v1/projects/prove2me/campaigns; tasks waiting or running in
//    other trials (an outline probe round beside a trial) as one more count;
//  - the numbers: the network's nodes online and GPU slots busy (GET /v1/stats `nodes`: the whole network, not only
//    prove2me), the trial's leaves proved (a run that proved its leaf, as the trials table counts it) and the proofs
//    submitted and accepted (GET /v1/projects/prove2me/results).
//
// There is no fixed total of work (a trial's leaves are chosen when it is prepared, and a leaf's tasks grow with its
// outlines), so the block shows counts, never a percentage of a whole. Pure functions; the page draws them.
//
// SPDX-License-Identifier: Apache-2.0
import { isLiveTrial, type PublicTask } from './prove2me-current';
import { campaignRuns, trialOf, type Campaign, type History, type Records } from './prove2me-records';

/** The network's nodes now, as GET /v1/stats gives them. */
export interface Nodes {
	online: number;
	busy: number;
	slots: number;
	updated: string;
}

/** The queue bar's parts, in the bar's order. */
export const BAR_PARTS = ['proved', 'ended', 'stopped', 'running', 'queued'] as const;
export type BarPart = (typeof BAR_PARTS)[number];
export const BAR_LABEL: Record<BarPart, string> = {
	proved: 'proved',
	ended: 'ended otherwise',
	stopped: 'stopped',
	running: 'running',
	queued: 'waiting',
};

export interface StatusView {
	source: 'build' | 'live';
	/** The newest time of the data shown (ISO). */
	updated: string;
	/** The trial the bar shows, or null when none is open. */
	trial: { label: string; number: number; probe: boolean; status: 'preparing' | 'running' | 'finished'; scaffold: string; start: string | null } | null;
	/** The newest trial that has ended, when no trial is open. */
	lastEnded: { number: number; end: string | null } | null;
	/** The current trial's tasks by state; null without the gateway's task list. */
	bar: (Record<BarPart, number> & { total: number }) | null;
	/** The current trial's leaves: proved by a run, of those it tries (leaves closed by reuse left out). */
	leaves: { proved: number; of: number } | null;
	/** Tasks waiting or running in other trials than the bar's, and in how many trials. */
	elsewhere: { tasks: number; trials: number };
	submissions: { submitted: number; accepted: number; pending: number };
	nodes: Nodes | null;
}

const isNum = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n >= 0;

/** GET /v1/stats's nodes, or null when the answer is not what it should be. */
export function parseNodes(d: unknown): Nodes | null {
	const v = d as { nodes?: { online?: unknown; busy?: unknown; slots?: unknown }; updated?: unknown };
	const n = v?.nodes;
	if (!n || ![n.online, n.busy, n.slots].every(isNum)) return null;
	if (typeof v.updated !== 'string' || Number.isNaN(Date.parse(v.updated))) return null;
	return { online: n.online as number, busy: Math.min(n.busy as number, n.slots as number), slots: n.slots as number, updated: v.updated };
}

/** A task's part of the bar. */
export function barPart(t: Pick<PublicTask, 'state' | 'outcome'>): BarPart {
	if (t.state === 'queued') return 'queued';
	if (t.state === 'leased') return 'running';
	if (t.state === 'done') return t.outcome === 'proved' ? 'proved' : 'ended';
	return 'stopped'; // failed or cancelled
}

const open = (t: PublicTask) => t.state === 'queued' || t.state === 'leased';
const statusRank = { running: 0, preparing: 1, finished: 2 } as const;

/** The trial the bar shows: an open trial campaign (running before preparing, a trial before its probe round, the
 * newest first), else the newest trial with tasks waiting or running that has no campaign yet; null when none. */
function currentTrial(campaigns: Campaign[], tasks: PublicTask[]): StatusView['trial'] {
	const camps = campaigns
		.filter((c) => isLiveTrial(c.id) && c.status !== 'finished')
		.map((c) => ({ c, t: trialOf(c.id)! }))
		.sort((a, b) => statusRank[a.c.status] - statusRank[b.c.status] || Number(a.t.probe) - Number(b.t.probe) || b.t.number - a.t.number);
	const pick = camps[0];
	if (pick) return { label: pick.c.id, number: pick.t.number, probe: pick.t.probe, status: pick.c.status, scaffold: pick.c.labels.scaffold ?? '', start: pick.c.started_at };
	const known = new Set(campaigns.map((c) => c.id));
	const labels = [...new Set(tasks.filter((t) => open(t) && isLiveTrial(t.labels.trial) && !known.has(t.labels.trial)).map((t) => t.labels.trial))];
	const newest = labels.map((l) => ({ l, t: trialOf(l)! })).sort((a, b) => Number(a.t.probe) - Number(b.t.probe) || b.t.number - a.t.number)[0];
	return newest ? { label: newest.l, number: newest.t.number, probe: newest.t.probe, status: 'running', scaffold: '', start: null } : null;
}

/** The block from the gateway's records (campaigns, results, tasks) and its nodes; either may be missing. Without the
 * records, the trial and submissions stay `fallback`'s (the build's). Null with neither: the page keeps what it shows. */
export function statusView(live: Records | null, nodes: Nodes | null, fallback: StatusView | null = null): StatusView | null {
	if (!live && !nodes) return null;
	const updated = [live?.updated, nodes?.updated].filter((x): x is string => !!x).sort().at(-1)!;
	if (!live) {
		const none = { source: 'build' as const, updated, trial: null, lastEnded: null, bar: null, leaves: null, elsewhere: { tasks: 0, trials: 0 }, submissions: { submitted: 0, accepted: 0, pending: 0 } };
		return { ...(fallback ?? none), nodes };
	}
	const tasks = live.tasks.tasks;
	const trial = currentTrial(live.campaigns, tasks);
	let bar: StatusView['bar'] = null;
	let leaves: StatusView['leaves'] = null;
	if (trial) {
		const mine = tasks.filter((t) => t.labels.trial === trial.label);
		const b = { proved: 0, ended: 0, stopped: 0, running: 0, queued: 0, total: mine.length };
		for (const t of mine) b[barPart(t)]++;
		bar = b;
		const camp = live.campaigns.find((c) => c.id === trial.label);
		if (camp && !trial.probe) {
			const reused = new Set(live.results.filter((r) => r.campaign === camp.id && r.labels.kind === 'reuse' && r.item).map((r) => r.item));
			const runs = campaignRuns(camp, tasks, live.results);
			leaves = { proved: runs.filter((r) => r.proved).length, of: camp.items.filter((i) => !reused.has(i.id)).length };
		}
	}
	const others = tasks.filter((t) => open(t) && isLiveTrial(t.labels.trial) && t.labels.trial !== trial?.label);
	const ended = trial
		? null
		: live.campaigns
				.filter((c) => isLiveTrial(c.id) && !trialOf(c.id)!.probe && c.status === 'finished')
				.sort((a, b) => trialOf(b.id)!.number - trialOf(a.id)!.number)[0];
	const rs = live.results;
	return {
		source: 'live',
		updated,
		trial,
		lastEnded: ended ? { number: trialOf(ended.id)!.number, end: ended.ended_at } : null,
		bar,
		leaves,
		elsewhere: { tasks: others.length, trials: new Set(others.map((t) => t.labels.trial)).size },
		submissions: { submitted: rs.length, accepted: rs.filter((r) => r.state === 'accepted').length, pending: rs.filter((r) => r.state === 'pending').length },
		nodes,
	};
}

/** The build's copy (no JavaScript, or before the gateway answers): its trial and submissions, no queue and no nodes. */
export function builtStatus(h: History): StatusView {
	const c = h.current;
	const last = h.trials.filter((t) => !t.probe && t.status === 'finished').sort((a, b) => b.number - a.number)[0];
	const subs = h.submissions;
	return {
		source: 'build',
		updated: h.generated_at,
		trial: c ? { label: `t${c.number}`, number: c.number, probe: false, status: c.status, scaffold: c.scaffold, start: c.start } : null,
		lastEnded: c || !last ? null : { number: last.number, end: last.end },
		bar: null,
		leaves: null,
		elsewhere: { tasks: 0, trials: 0 },
		submissions: { submitted: subs.length, accepted: subs.filter((s) => s.verdict === 'accepted').length, pending: subs.filter((s) => s.verdict === 'pending').length },
		nodes: null,
	};
}

const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const shortDay = (iso: string) => {
	const d = new Date(iso.length === 10 ? `${iso}T00:00:00Z` : iso);
	return `${d.getUTCDate()} ${months[d.getUTCMonth()]}`;
};
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/** The block's texts: the trial's line, the bar's words (its text equivalent), the other trials' line, the numbers. */
export function statusTexts(v: StatusView, now: Date = new Date()) {
	const t = v.trial;
	const trialName = t ? (t.probe ? `Outline probes for trial ${t.number}` : `Trial ${t.number}`) : '';
	const scaffold = t?.scaffold ? `, scaffold ${t.scaffold}` : '';
	const head = t
		? t.status === 'preparing'
			? `${trialName} ${t.probe ? 'are' : 'is'} being prepared${scaffold}`
			: `${trialName} ${t.probe ? 'are' : 'is'} running${t.start ? ` since ${shortDay(t.start)}` : ''}${scaffold}`
		: v.lastEnded
			? `No trial is running. Trial ${v.lastEnded.number} ended${v.lastEnded.end ? ` on ${shortDay(v.lastEnded.end)}` : ''}.`
			: 'No trial is running.';
	const b = v.bar;
	const barWords = b
		? b.total === 0
			? 'No tasks yet.'
			: `${plural(b.total, 'task')}: ` + BAR_PARTS.filter((p) => b[p] > 0).map((p) => `${b[p]} ${BAR_LABEL[p]}`).join(', ') + '.'
		: '';
	const e = v.elsewhere;
	const elsewhere = e.tasks > 0 ? `${plural(e.tasks, 'more task')} waiting or running in ${e.trials === 1 ? 'another trial' : `${e.trials} other trials`}.` : '';
	const age = (now.getTime() - Date.parse(v.updated)) / 60000;
	const hm = `${v.updated.slice(11, 16)} UTC`;
	const updated =
		v.source === 'build'
			? v.nodes
				? `Trial and proofs as of the site’s last build, ${shortDay(v.updated)}, ${hm}; nodes live from the gateway, updated ${v.nodes.updated.slice(11, 16)} UTC.`
				: `As of the site’s last build, ${shortDay(v.updated)}, ${hm}. The queue and the network show when the page can reach the gateway.`
			: age > 10
				? `Updated ${shortDay(v.updated)}, ${hm}; the gateway has not answered since.`
				: `Live from the gateway, updated ${hm}. It refreshes every minute.`;
	const s = v.submissions;
	const nums: Record<string, string> = {
		nodes: v.nodes ? String(v.nodes.online) : '',
		slots: v.nodes ? `${v.nodes.busy} of ${v.nodes.slots}` : '',
		leaves: v.leaves ? `${v.leaves.proved} of ${v.leaves.of}` : '',
		accepted: `${s.accepted} of ${s.submitted}`,
	};
	return { head, barWords, elsewhere, updated, nums };
}
