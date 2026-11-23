// The status block at the top of /science/prove2me/: where the work stands now, from the gateway's public records.
//
//  - the queue: every prove2me task running or waiting now, whatever trial it belongs to (owner, 2026-11-19: a new trial
//    can start while the last one still has tasks), as one bar in parts per trial, from GET /v1/tasks?project=prove2me;
//  - one line per open trial (a trial or probe-round campaign that is running or being prepared, or a trial label with
//    open tasks and no campaign yet): its tasks by state (proved, ended otherwise, stopped, running, waiting) and, for a
//    trial, its leaves proved (a run that proved its leaf, as the trials table counts it), from the campaigns and tasks;
//  - one line per open replay benchmark (a task label bench-<name>, e.g. bench-v13; no campaign record): the research
//    tooling replays recorded cases of earlier trials under one scaffold to compare scaffold versions, at a queue band
//    below the trials', so its tasks are claimed first. Its tasks count in the queue, as their own part, and nowhere
//    else: a benchmark is no trial, submits nothing and proves no leaf of a trial;
//  - the numbers: the network's nodes online and GPU slots busy (GET /v1/stats `nodes`: the whole network, not only
//    prove2me) and the proofs submitted and accepted (GET /v1/projects/prove2me/results).
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

/** A trial's tasks by state, in this order. */
export const BAR_PARTS = ['proved', 'ended', 'stopped', 'running', 'queued'] as const;
export type BarPart = (typeof BAR_PARTS)[number];
export const BAR_LABEL: Record<BarPart, string> = {
	proved: 'proved',
	ended: 'ended otherwise',
	stopped: 'stopped',
	running: 'running',
	queued: 'waiting',
};
export type Counts = Record<BarPart, number> & { total: number };

/** One open trial: its campaign's facts, its tasks by state and (a trial, not a probe round) its leaves. */
export interface OpenTrial {
	label: string;
	number: number;
	probe: boolean;
	status: 'preparing' | 'running';
	scaffold: string;
	start: string | null;
	counts: Counts;
	/** Leaves proved by a run, of those it tries (leaves closed by reuse left out); null for a probe round or a trial
	 * with no campaign yet. */
	leaves: { proved: number; of: number } | null;
	/** The open benchmarks running ahead of it while it has tasks waiting and none running. */
	behind: string[];
}

/** One open benchmark: a replay of earlier trials' tasks under one scaffold, with tasks running or waiting. */
export interface OpenBench {
	label: string;
	scaffold: string;
	/** The leaves its tasks name. */
	leaves: number;
	running: number;
	queued: number;
	/** The open trials it is ahead of in the queue: one of its tasks started after a task of theirs, still waiting,
	 * was made. */
	ahead: string[];
}

/** A benchmark's label on a task: bench-<name> (bench-v13, bench-v12b); never a trial's t<N>. */
export const BENCH_LABEL = /^bench-[A-Za-z0-9._-]+$/;
export const isBench = (label: string | undefined) => BENCH_LABEL.test(label ?? '');

export interface StatusView {
	/** live: the browser's, from the gateway now; build: the records the site's build fetched (or, `saved`, its committed
	 * copy when the gateway did not answer the build), else the build's history alone (no queue). */
	source: 'build' | 'live';
	saved?: boolean;
	/** The newest time of the data shown (ISO). */
	updated: string;
	/** Every open trial, oldest first; empty when none is open. */
	trials: OpenTrial[];
	/** Every open benchmark, by label. */
	benches: OpenBench[];
	/** The newest trial that has ended, when no trial is open. */
	lastEnded: { number: number; end: string | null } | null;
	/** The whole queue now: tasks running and waiting per open benchmark (they are claimed first), then per trial
	 * (every trial with any), oldest trial first; null without the gateway's task list. */
	queue: { label: string; running: number; queued: number; bench: boolean }[] | null;
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

/** A task's state as the block counts it. */
export function barPart(t: Pick<PublicTask, 'state' | 'outcome'>): BarPart {
	if (t.state === 'queued') return 'queued';
	if (t.state === 'leased') return 'running';
	if (t.state === 'done') return t.outcome === 'proved' ? 'proved' : 'ended';
	return 'stopped'; // failed or cancelled
}

const isOpen = (t: PublicTask) => t.state === 'queued' || t.state === 'leased';
/** Trials in order: by number, a probe round before the trial it chose leaves for. */
const order = (a: { number: number; probe: boolean }, b: { number: number; probe: boolean }) => a.number - b.number || Number(b.probe) - Number(a.probe);

function countsOf(tasks: PublicTask[]): Counts {
	const c = { proved: 0, ended: 0, stopped: 0, running: 0, queued: 0, total: tasks.length };
	for (const t of tasks) c[barPart(t)]++;
	return c;
}

/** The block from the gateway's records (campaigns, results, tasks) and its nodes; either may be missing. Without the
 * records, the trials and submissions stay `fallback`'s (the build's). Null with neither: the page keeps what it shows. */
export function statusView(live: Records | null, nodes: Nodes | null, fallback: StatusView | null = null): StatusView | null {
	if (!live && !nodes) return null;
	const updated = [live?.updated, nodes?.updated].filter((x): x is string => !!x).sort().at(-1)!;
	if (!live) {
		const none: StatusView = { source: 'build', updated, trials: [], benches: [], lastEnded: null, queue: null, submissions: { submitted: 0, accepted: 0, pending: 0 }, nodes: null };
		return { ...(fallback ?? none), nodes };
	}
	const tasks = live.tasks.tasks.filter((t) => isLiveTrial(t.labels.trial)); // a test's or a benchmark's tasks are no trial's
	const benchTasks = new Map<string, PublicTask[]>();
	for (const t of live.tasks.tasks) if (isBench(t.labels.trial)) benchTasks.set(t.labels.trial, [...(benchTasks.get(t.labels.trial) ?? []), t]);
	const byTrial = new Map<string, PublicTask[]>();
	for (const t of tasks) byTrial.set(t.labels.trial, [...(byTrial.get(t.labels.trial) ?? []), t]);
	const camps = new Map(live.campaigns.filter((c) => isLiveTrial(c.id)).map((c) => [c.id, c]));
	// open: a campaign running or being prepared, or a label with open tasks (its campaign finished, or none yet)
	const labels = new Set([...camps.values()].filter((c) => c.status !== 'finished').map((c) => c.id));
	for (const t of tasks) if (isOpen(t)) labels.add(t.labels.trial);
	const trials: OpenTrial[] = [...labels]
		.map((label) => {
			const n = trialOf(label)!;
			const c: Campaign | undefined = camps.get(label);
			let leaves: OpenTrial['leaves'] = null;
			if (c && !n.probe) {
				const reused = new Set(live.results.filter((r) => r.campaign === c.id && r.labels.kind === 'reuse' && r.item).map((r) => r.item));
				leaves = { proved: campaignRuns(c, tasks, live.results).filter((r) => r.proved).length, of: c.items.filter((i) => !reused.has(i.id)).length };
			}
			return {
				label, number: n.number, probe: n.probe, status: c?.status === 'preparing' ? 'preparing' : 'running', scaffold: c?.labels.scaffold ?? '',
				start: c?.started_at ?? null, counts: countsOf(byTrial.get(label) ?? []), leaves, behind: [],
			} as OpenTrial;
		})
		.sort(order);
	// a benchmark is ahead of a trial when one of its tasks started after a trial task that still waits was made
	const firstWaiting = new Map(trials.map((t) => [t.label, Math.min(...(byTrial.get(t.label) ?? []).filter((x) => x.state === 'queued').map((x) => Date.parse(x.created_at)))]));
	const benches: OpenBench[] = [...benchTasks]
		.filter(([, ts]) => ts.some(isOpen))
		.map(([label, ts]) => {
			const lastStart = Math.max(...ts.map((x) => (x.started_at ? Date.parse(x.started_at) : -Infinity)));
			const c = countsOf(ts);
			return {
				label, scaffold: ts.find((x) => x.labels.scaffold)?.labels.scaffold ?? '', leaves: new Set(ts.map((x) => x.labels.leaf).filter(Boolean)).size,
				running: c.running, queued: c.queued, ahead: trials.filter((t) => firstWaiting.get(t.label)! < lastStart).map((t) => t.label),
			};
		})
		.sort((a, b) => a.label.localeCompare(b.label));
	for (const t of trials)
		if (t.counts.queued > 0 && t.counts.running === 0) t.behind = benches.filter((b) => b.running > 0 && b.ahead.includes(t.label)).map((b) => b.label);
	const queue = [
		...benches.map((b) => ({ label: b.label, running: b.running, queued: b.queued, bench: true })),
		...trials.map((t) => ({ label: t.label, running: t.counts.running, queued: t.counts.queued, bench: false })),
	].filter((q) => q.running + q.queued > 0);
	const ended = trials.length
		? null
		: [...camps.values()].filter((c) => !trialOf(c.id)!.probe && c.status === 'finished').sort((a, b) => trialOf(b.id)!.number - trialOf(a.id)!.number)[0];
	const rs = live.results; // a benchmark submits nothing (its tasks run with submit off)
	return {
		source: 'live',
		updated,
		trials,
		benches,
		lastEnded: ended ? { number: trialOf(ended.id)!.number, end: ended.ended_at } : null,
		queue,
		submissions: { submitted: rs.length, accepted: rs.filter((r) => r.state === 'accepted').length, pending: rs.filter((r) => r.state === 'pending').length },
		nodes,
	};
}

/** The build's copy (no JavaScript, or before the gateway answers): its trial and submissions, no queue and no nodes. */
export function builtStatus(h: History): StatusView {
	const c = h.current;
	const last = h.trials.filter((t) => !t.probe && t.status === 'finished').sort((a, b) => b.number - a.number)[0];
	const subs = h.submissions;
	const none = { proved: 0, ended: 0, stopped: 0, running: 0, queued: 0, total: 0 };
	return {
		source: 'build',
		updated: h.generated_at,
		trials: c ? [{ label: `t${c.number}`, number: c.number, probe: false, status: c.status, scaffold: c.scaffold, start: c.start, counts: none, leaves: null, behind: [] }] : [],
		benches: [],
		lastEnded: c || !last ? null : { number: last.number, end: last.end },
		queue: null,
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
const and = (xs: string[]) => (xs.length < 2 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);

/** A trial's name: "Trial 9", or "Outline probes for trial 10". */
export const trialName = (t: { number: number; probe: boolean }) => (t.probe ? `Outline probes for trial ${t.number}` : `Trial ${t.number}`);
/** The short name a queue part carries: "Trial 9", "Probes 10", a benchmark's label ("bench-v13"). */
export const shortName = (label: string) => {
	const t = trialOf(label);
	return t ? (t.probe ? `Probes ${t.number}` : `Trial ${t.number}`) : label;
};
const facts = (t: OpenTrial) => [t.scaffold ? `scaffold ${t.scaffold}` : '', t.start ? `since ${shortDay(t.start)}` : ''].filter(Boolean).join(', ');

/** The head line: every open trial, with its scaffold and start. */
function headText(v: StatusView): string {
	if (!v.trials.length) {
		const e = v.lastEnded;
		return e ? `No trial is running. Trial ${e.number} ended${e.end ? ` on ${shortDay(e.end)}` : ''}.` : 'No trial is running.';
	}
	const part = (ts: OpenTrial[], verb: [string, string]) => {
		if (!ts.length) return '';
		if (ts.length === 1) {
			const f = facts(ts[0]);
			return `${trialName(ts[0])} ${ts[0].probe ? verb[1] : verb[0]}${f ? `, ${f}` : ''}`;
		}
		const names = ts.map((t) => `${trialName(t)}${t.scaffold ? ` (scaffold ${t.scaffold})` : ''}`);
		// "Trials 8 (scaffold v9) and 9 (scaffold v10)" when every one is a trial
		const plain = ts.every((t) => !t.probe)
			? `Trials ${and(ts.map((t) => `${t.number}${t.scaffold ? ` (scaffold ${t.scaffold})` : ''}`))}`
			: and(names.map((n, i) => (i ? n.replace(/^Trial/, 'trial').replace(/^Outline/, 'outline') : n)));
		return `${plain} ${verb[1]}`;
	};
	const running = part(v.trials.filter((t) => t.status === 'running'), ['is running', 'are running']);
	const preparing = part(v.trials.filter((t) => t.status === 'preparing'), ['is being prepared', 'are being prepared']);
	return [running, preparing].filter(Boolean).join('; ') + '.';
}

/** One open trial's line: its tasks by state and its leaves. */
export function trialLine(t: OpenTrial): string {
	const c = t.counts;
	const tasks = c.total === 0 ? 'no tasks yet' : `${plural(c.total, 'task')}: ` + BAR_PARTS.filter((p) => c[p] > 0).map((p) => `${c[p]} ${BAR_LABEL[p]}`).join(', ');
	const leaves = t.leaves ? `; ${t.leaves.proved} of ${plural(t.leaves.of, 'leaf', 'leaves')} proved` : '';
	const behind = t.behind.length ? ` Its tasks wait behind ${t.behind.length > 1 ? 'benchmarks' : 'benchmark'} ${and(t.behind)}.` : '';
	return `${trialName(t)}, ${tasks}${leaves}.${behind}`;
}

/** The trials' names as one: "trial 10", "trials 10 and 11", "outline probes for trial 11". */
const trialsNamed = (ts: { number: number; probe: boolean }[]) =>
	ts.every((t) => !t.probe) ? `${ts.length > 1 ? 'trials' : 'trial'} ${and(ts.map((t) => String(t.number)))}` : and(ts.map((t) => trialName(t).replace(/^T/, 't').replace(/^O/, 'o')));

/** One open benchmark's line: what it is, what it is ahead of, its tasks running and waiting. */
export function benchLine(b: OpenBench, trials: OpenTrial[] = []): string {
	const ahead = trials.filter((t) => b.ahead.includes(t.label));
	return (
		`Benchmark ${b.label} is ${b.running ? 'running' : 'waiting'}${b.scaffold ? `, scaffold ${b.scaffold}` : ''}: it replays earlier tasks` +
		`${b.leaves ? ` on ${plural(b.leaves, 'leaf', 'leaves')}` : ''} to compare scaffold versions` +
		`${ahead.length ? `, ahead of ${trialsNamed(ahead)} in the queue` : ''}; ${b.running} running, ${b.queued} waiting.`
	);
}

/** The block's texts: the head line, the queue in words (the bar's text equivalent), each open trial's line, the
 * numbers and the time. */
export function statusTexts(v: StatusView, now: Date = new Date()) {
	const q = v.queue;
	const running = q ? q.reduce((a, x) => a + x.running, 0) : 0;
	const queued = q ? q.reduce((a, x) => a + x.queued, 0) : 0;
	const queue = !q
		? ''
		: running + queued === 0
			? 'Nothing in the queue now.'
			: `${plural(running + queued, 'task')} in the queue: ${running} running, ${queued} waiting` +
				(q.length > 1 ? ` (${q.map((x) => `${shortName(x.label)}: ${x.running} running, ${x.queued} waiting`).join('; ')})` : '') + '.';
	const age = (now.getTime() - Date.parse(v.updated)) / 60000;
	const hm = `${v.updated.slice(11, 16)} UTC`;
	const asOf =
		v.queue === null
			? `As of the site’s last build, ${shortDay(v.updated)}, ${hm}.`
			: v.saved
				? `As of a saved copy of the gateway’s records, ${shortDay(v.updated)}, ${hm}: the gateway did not answer the site’s last build.`
				: `As of the gateway’s records at the site’s last build, ${shortDay(v.updated)}, ${hm}.`;
	const updated =
		v.source === 'build'
			? v.nodes
				? `Trials and proofs as of the site’s last build, ${shortDay(v.updated)}, ${hm}; nodes live from the gateway, updated ${v.nodes.updated.slice(11, 16)} UTC.`
				: `${asOf} The page refreshes it every minute when it can reach the gateway.`
			: age > 10
				? `Updated ${shortDay(v.updated)}, ${hm}; the gateway has not answered since.`
				: `Live from the gateway, updated ${hm}. It refreshes every minute.`;
	const s = v.submissions;
	const nums: Record<string, string> = {
		nodes: v.nodes ? String(v.nodes.online) : '',
		slots: v.nodes ? `${v.nodes.busy} of ${v.nodes.slots}` : '',
		accepted: `${s.accepted} of ${s.submitted}`,
	};
	// the benchmarks' lines first (their tasks are claimed first), then the trials'; `keys` names each line's label
	const recs = v.queue !== null; // the records, live or the build's
	const lines = recs ? [...v.benches.map((b) => benchLine(b, v.trials)), ...v.trials.map(trialLine)] : [];
	const keys = recs ? [...v.benches.map((b) => b.label), ...v.trials.map((t) => t.label)] : [];
	return { head: headText(v), queue, lines, keys, updated, nums };
}

/** The queue bar's parts (the bar's children) and their names under it: a part per benchmark and per trial, its running
 * tasks then its waiting ones. Empty with nothing in the queue. */
export function queueBarHTML(v: StatusView): { bar: string; labels: string } {
	const q = (v.queue ?? []).filter((x) => x.running + x.queued > 0);
	let nth = 0; // the trials' parts alternate their colour; a benchmark's has its own
	const attr = (x: { bench: boolean; label: string }) => `data-${x.bench ? 'bench' : 'trial'}="${escAttr(x.label)}"`;
	const bar = q
		.map((x) => {
			const segs = (['running', 'queued'] as const)
				.filter((k) => x[k] > 0)
				.map((k) => `<span class="seg q-${k}" data-part="${k}" style="flex-grow: ${x[k]}"></span>`)
				.join('');
			return `<span class="grp ${x.bench ? 'grp-bench' : `grp-${nth++ % 2}`}" ${attr(x)} style="flex-grow: ${x.running + x.queued}">${segs}</span>`;
		})
		.join('');
	const labels = q.map((x) => `<span ${attr(x)} style="flex-grow: ${x.running + x.queued}">${escAttr(`${shortName(x.label)} · ${x.running + x.queued}`)}</span>`).join('');
	return { bar, labels };
}
const escAttr = (v: string) => v.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);

/** "What comes next", the part the queue says: what runs first and what waits behind it (owner, 2026-11-23: it must not
 * go stale). Empty without the records. */
export function nextText(v: StatusView): string {
	if (!v.queue) return '';
	const tasks = (x: { running: number; queued: number }) =>
		x.running && x.queued ? `${plural(x.running, 'task')} running, ${x.queued} waiting` : x.running ? `${plural(x.running, 'task')} running` : `${plural(x.queued, 'task')} waiting`;
	const inQueue = (label: string) => v.queue!.find((q) => q.label === label && q.running + q.queued > 0);
	const name = (t: OpenTrial) => (t.probe ? `the outline probes for trial ${t.number}` : `trial ${t.number}`);
	const tpart = (t: OpenTrial) => `${name(t)}${t.scaffold ? ` on scaffold ${t.scaffold}` : ''} (${tasks(inQueue(t.label)!)})`;
	const benches = v.benches.filter((b) => inQueue(b.label));
	const ahead = new Set(benches.flatMap((b) => b.ahead));
	const parts = [
		...benches.map((b) => {
			const ts = v.trials.filter((t) => b.ahead.includes(t.label) && inQueue(t.label));
			return `benchmark ${b.label}${b.scaffold ? ` on scaffold ${b.scaffold}` : ''} (${tasks(b)})${ts.length ? `, ahead of ${and(ts.map(tpart))}` : ''}`;
		}),
		...v.trials.filter((t) => !ahead.has(t.label) && inQueue(t.label)).map(tpart),
	];
	const preparing = v.trials.filter((t) => t.status === 'preparing' && !inQueue(t.label)).map((t) => `${trialName(t)} is being prepared.`);
	return [parts.length ? `In the queue now: ${parts.join('; ')}.` : 'Nothing is in the queue now.', ...preparing].join(' ');
}
