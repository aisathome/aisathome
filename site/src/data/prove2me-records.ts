// The prove2me page's trials, submissions and headline numbers from the gateway's records:
// its public campaigns (prove2me's trials and outline probe rounds), external results (prove2me's verdicts on our
// submissions) and tasks, read live in the browser:
//
//   GET /v1/projects/prove2me/campaigns   every campaign, its items (the leaves) and labels (kind, scaffold, budget)
//   GET /v1/projects/prove2me/results     every submission and its verdict
//   GET /v1/tasks?project=prove2me        every task (prove2me-current.ts)
//
// Trials 1 to 5 are frozen history: they always come from the build's data (science/prove2me.json), as do submissions
// the gateway does not hold. From trial 6 on, a trial with a campaign comes from the gateway; one without (a gateway
// that lacks it) keeps the build's row. Without the gateway the page is the build's (History alone), so the same
// function makes the build's numbers and the live ones. O04 (trial 3) has no campaign: its result, if the gateway
// holds it, joins the build's run.
//
// Pure functions, no data: the page's script and the home page's import them without the build's JSON.
//
// SPDX-License-Identifier: Apache-2.0
import { FROZEN_THROUGH, TRIAL_LABEL, fetchAllTasks, theoremURL, trialQueue, walkPages, type CurrentDoc, type PublicTask, type TasksView } from './prove2me-current';
import { fromStatic, leafDetail, liveLeaves, type LeafDetail, type Outline, type StaticTask } from './prove2me-live';
import { leafPlan } from './prove2me-outlines';
import { provesLeaf } from './prove2me-outcomes';
import type { Prove2me } from './prove2me';
import type { TaskTableRow } from './task-table';
import curatedReuses from './science/prove2me-reuses.json';

export const CAMPAIGNS_URL = '/v1/projects/prove2me/campaigns?limit=200';
export const RESULTS_URL = '/v1/projects/prove2me/results?limit=500';
/** The home page and /science/ need only the tasks that decide a leaf (an outline or an assembly) and their starts. */
export const DECIDING_TASKS_URL = '/v1/tasks?project=prove2me&role=outline,assembly&limit=500';

// ---- the gateway's records (docs/gateway.md, "Campaigns and external results")

export interface CampaignItem {
	id: string;
	title: string;
	url: string;
	role: string;
	labels: Record<string, string>;
}
export interface Campaign {
	id: string;
	title: string;
	status: 'preparing' | 'running' | 'finished';
	started_at: string | null;
	ended_at: string | null;
	labels: Record<string, string>;
	text: string;
	items: CampaignItem[];
	updated_at: string;
}
export interface ExtResult {
	id: string;
	campaign: string | null;
	item: string | null;
	task: string | null;
	url: string | null;
	state: 'pending' | 'accepted' | 'rejected' | 'error';
	verdict: string;
	labels: Record<string, string>;
	submitted_at: string;
	verdict_at: string | null;
}
export interface Records {
	updated: string;
	campaigns: Campaign[];
	results: ExtResult[];
	tasks: TasksView;
}

const isStr = (v: unknown): v is string => typeof v === 'string';
const isTime = (v: unknown): v is string => isStr(v) && !Number.isNaN(Date.parse(v));
const optTime = (v: unknown) => v === null || v === undefined || isTime(v);
const isLabels = (v: unknown): v is Record<string, string> => !!v && typeof v === 'object' && !Array.isArray(v) && Object.values(v).every(isStr);
const leafID = /^p2m-[0-9a-f]{6}([0-9a-f]{2})?$/;
const campaignID = /^[A-Za-z0-9._-]{1,64}$/;
const resultStates = ['pending', 'accepted', 'rejected', 'error'];

/** One page of GET .../campaigns, or null when it is not one; a campaign that is not what it should be is left out. */
export function parseCampaigns(d: unknown): { items: Campaign[] } | null {
	const v = d as { campaigns?: unknown };
	if (!v || !Array.isArray(v.campaigns)) return null;
	const items = (v.campaigns as Campaign[])
		.filter(
			(c) =>
				c && isStr(c.id) && campaignID.test(c.id) && ['preparing', 'running', 'finished'].includes(c.status) && optTime(c.started_at) &&
				optTime(c.ended_at) && isLabels(c.labels) && (c.text === undefined || c.text === null || isStr(c.text)) && Array.isArray(c.items),
		)
		.map((c) => ({
			id: c.id,
			title: isStr(c.title) ? c.title : c.id,
			status: c.status,
			started_at: c.started_at ?? null,
			ended_at: c.ended_at ?? null,
			labels: c.labels,
			text: c.text ?? '',
			updated_at: isTime(c.updated_at) ? c.updated_at : '',
			items: c.items
				.filter((i) => i && isStr(i.id) && isLabels(i.labels ?? {}))
				.map((i) => ({ id: i.id, title: isStr(i.title) ? i.title : '', url: theoremURL.test(i.url ?? '') ? i.url : '', role: isStr(i.role) ? i.role : 'open', labels: i.labels ?? {} })),
		}));
	return { items };
}

/** A reuse the records cannot mark: the leaf, the trial whose result it corrects, the theorem the proof imports. */
type CuratedReuse = { trial: string; theorem: string; url: string; author: string; relation: string };
const reuseFixes = Object.fromEntries(Object.entries(curatedReuses).filter(([k]) => leafID.test(k))) as Record<string, CuratedReuse>;

/** A result corrected by science/prove2me-reuses.json: a leaf's accepted "proof" whose proof imports someone else's
 * proved theorem is a reuse (kind reuse, the theorem, its page and author in the labels). */
function curated(r: ExtResult): ExtResult {
	const fix = r.item ? reuseFixes[r.item] : undefined;
	if (!fix || (r.labels.trial ?? r.campaign) !== fix.trial) return r;
	return { ...r, labels: { ...r.labels, kind: 'reuse', reuses: fix.theorem, relation: fix.relation, reuses_url: fix.url, reuses_author: fix.author } };
}

/** One page of GET .../results, or null when it is not one; a result that is not what it should be is left out, a
 * curated reuse corrected (curated). */
export function parseResults(d: unknown): { items: ExtResult[] } | null {
	const v = d as { results?: unknown };
	if (!v || !Array.isArray(v.results)) return null;
	const items = (v.results as ExtResult[])
		.filter((r) => r && isStr(r.id) && resultStates.includes(r.state) && isTime(r.submitted_at) && optTime(r.verdict_at) && isLabels(r.labels))
		.map((r) => ({
			id: r.id,
			campaign: isStr(r.campaign) ? r.campaign : null,
			item: isStr(r.item) && leafID.test(r.item) ? r.item : null,
			task: isStr(r.task) ? r.task : null,
			url: theoremURL.test(r.url ?? '') ? r.url : null,
			state: r.state,
			verdict: isStr(r.verdict) ? r.verdict : '',
			labels: r.labels,
			submitted_at: r.submitted_at,
			verdict_at: r.verdict_at ?? null,
		}))
		.map(curated);
	return { items };
}

/** Campaigns, results and tasks, every page of each; null when any of them fails (the page keeps what it shows). */
export async function fetchRecords(get: (url: string) => Promise<unknown>, tasksURL?: string): Promise<Records | null> {
	const [c, r, t] = await Promise.all([
		walkPages(get, CAMPAIGNS_URL, parseCampaigns),
		walkPages(get, RESULTS_URL, parseResults),
		fetchAllTasks(get, tasksURL),
	]);
	if (!c || !r || !t) return null;
	return { updated: t.updated, campaigns: c.items, results: r.items, tasks: t };
}

// ---- the page's model: the build's history and, when the gateway answers, its records

export type Count = { proved: number; runs: number };
/** A run: one leaf in one trial (not an outline probe). */
export interface RunRec {
	trial: number;
	leaf: string;
	alias: string;
	group: 'calibration' | 'open';
	/** The leaf's role in its trial: calibration, open, or comparison (an open leaf run again to compare scaffolds). */
	role: 'calibration' | 'open' | 'comparison';
	proved: boolean;
	/** False for a run cut short by a fault on our side (the build's; a gateway trial's runs are all scored). */
	scored: boolean;
	/** A run we stopped (not scored, its outcome "cancelled"). */
	stopped: boolean;
	/** The day its trial started. */
	date: string | null;
	model_min: number;
	computed: number;
	withCache: number;
	/** False when a task that ran has no cache count: its tokens are then a lower bound. */
	exact: boolean;
}
/** A leaf's theorem, its prove2me title and page. */
export type LeafInfo = { theorem: string; title: string; url: string };
/** A submission to prove2me and its verdict. */
export interface SubRec {
	/** The leaf's id (p2m-...); `alias` the id the trials used. */
	leaf: string;
	alias: string;
	kind: 'proof' | 'reuse';
	theorem: string;
	title: string;
	url: string;
	/** The trial of the run that proved it; for a reuse, the trial whose probes or run found it. */
	trial: number;
	found?: 'probes' | 'trial';
	reuses?: { theorem: string; url?: string; author?: string; relation?: string };
	submitted_at: string;
	verdict: string;
	verdict_at: string | null;
}
/** A row of the trials table (a trial, or the outline probes that chose a trial's leaves). */
export interface TrialSum {
	/** "7", or "7p" for trial 7's outline probes. */
	label: string;
	number: number;
	probe: boolean;
	source: 'build' | 'live';
	status: 'preparing' | 'running' | 'finished';
	start: string | null;
	end: string | null;
	scaffold: string;
	all: Count;
	calibration: Count;
	open: Count;
	model_min: number;
	computed: number;
	withCache: number;
	exact: boolean;
}
/** What the page needs of the build's data: every run, submission and trials-table row, and each theorem's leaf id. */
export interface History {
	generated_at: string;
	runs: RunRec[];
	trials: TrialSum[];
	submissions: SubRec[];
	current: { number: number; status: 'preparing' | 'running'; start: string | null; scaffold: string } | null;
	/** Every leaf of the build's data by id: its theorem, title and page (a gateway campaign's items add theirs). */
	leafInfo: Record<string, LeafInfo>;
	/** The leaves of each finished outline probe round of the build's data, by the trials table's label ("6p"). */
	probeLeaves: Record<string, string[]>;
}
export interface Kpis {
	accepted: number;
	submitted: number;
	reused: number;
	openProved: number;
	openTried: number;
	openProvedLeaves: { leaf: string; alias: string }[];
	runs: number;
	proved: number;
	calibrationRuns: number;
	calibrationProved: number;
	/** Model hours of the runs; of the outline probes. */
	hours: number;
	probeHours: number;
	/** The newest trial with a run. */
	lastTrial: number;
	computed: number;
	withCache: number;
}
export interface ScienceView {
	source: 'build' | 'live';
	updated: string;
	kpis: Kpis;
	submissions: SubRec[];
	trials: TrialSum[];
	current: History['current'];
}

/** A leaf's id from its prove2me page: p2m- and the start of the theorem's id. */
export const idFromURL = (url: string) => (theoremURL.test(url) ? `p2m-${url.slice(-36).replace(/-/g, '').slice(0, 6)}` : '');
const day = (iso: string | null | undefined) => (iso ? iso.slice(0, 10) : null);
const sum = <T>(xs: T[], f: (x: T) => number) => xs.reduce((a, x) => a + f(x), 0);
/** "t7" -> 7 and false; "t7p" -> 7 and true; null for a label that is no trial. */
export function trialOf(label: string | null | undefined): { number: number; probe: boolean } | null {
	const m = TRIAL_LABEL.exec(label ?? '');
	return m ? { number: Number(m[1]), probe: m[2] === 'p' } : null;
}

type Tok = { prompt_tokens: number; cached_tokens: number; output_tokens: number };
/** The build's counts: prompt tokens with the cache re-reads in them. */
export const computedOf = (t: Tok) => t.prompt_tokens - t.cached_tokens + t.output_tokens;
export const withCacheOf = (t: Tok) => t.prompt_tokens + t.output_tokens;

/** The build's data as History (the build runs this; the page embeds the result for its script). */
export function historyOf(p: Prove2me): History {
	const ids: Record<string, string> = Object.fromEntries(p.leaves.map((l) => [l.theorem, l.id]));
	const idOf = (theorem: string, url: string) => ids[theorem] ?? idFromURL(url);
	const runs: RunRec[] = p.trials.flatMap((tr) =>
		tr.leaves.map((r) => ({
			trial: tr.trial, leaf: idOf(r.theorem, r.url), alias: r.leaf, group: r.group === 'calibration' ? 'calibration' : 'open',
			role: r.role ?? (r.group === 'calibration' ? 'calibration' : 'open'), proved: r.outcome === 'proved', scored: r.scored,
			stopped: !r.scored && r.outcome === 'cancelled', date: tr.start, model_min: r.model_minutes, computed: computedOf(r), withCache: withCacheOf(r), exact: true,
		})),
	);
	const count = (rs: RunRec[]): Count => ({ proved: rs.filter((r) => r.proved).length, runs: rs.length });
	const trials: TrialSum[] = p.trials.map((tr) => {
		const rs = runs.filter((r) => r.trial === tr.trial);
		return {
			label: String(tr.trial), number: tr.trial, probe: false, source: 'build', status: 'finished', start: tr.start, end: tr.end, scaffold: tr.scaffold,
			all: count(rs), calibration: count(rs.filter((r) => r.group === 'calibration')), open: count(rs.filter((r) => r.group === 'open')),
			model_min: sum(rs, (r) => r.model_min), computed: sum(rs, (r) => r.computed), withCache: sum(rs, (r) => r.withCache), exact: true,
		};
	});
	for (const pr of p.probes ?? []) {
		if (pr.state !== 'finished' || pr.prompt_tokens === null) continue;
		const tok = { prompt_tokens: pr.prompt_tokens, cached_tokens: pr.cached_tokens ?? 0, output_tokens: pr.output_tokens ?? 0 };
		trials.push({
			label: pr.trial.slice(1), number: pr.for_trial, probe: true, source: 'build', status: 'finished', start: pr.start, end: pr.end ?? pr.start,
			scaffold: pr.scaffold, all: { proved: 0, runs: 0 }, calibration: { proved: 0, runs: 0 }, open: { proved: 0, runs: 0 },
			model_min: pr.model_minutes ?? 0, computed: computedOf(tok), withCache: withCacheOf(tok), exact: true,
		});
	}
	const submissions: SubRec[] = p.submissions.map((s) => ({
		leaf: idOf(s.theorem, s.url), alias: s.leaf, kind: s.kind, theorem: s.theorem, title: s.title, url: s.url, trial: s.trial, found: s.found,
		reuses: s.reuses, submitted_at: s.submitted_at, verdict: s.verdict, verdict_at: s.verdict_at,
	}));
	const cur = p.current;
	const running = !!cur && cur.state !== 'preparing' && cur.leaves.some((l) => l.state === 'running' || l.state === 'queued');
	return {
		generated_at: p.generated_at,
		runs,
		trials: sortTrials(trials),
		submissions,
		current: cur && (cur.state === 'preparing' || running) ? { number: cur.trial, status: cur.state === 'preparing' ? 'preparing' : 'running', start: cur.start, scaffold: cur.scaffold } : null,
		leafInfo: Object.fromEntries(p.leaves.map((l) => [l.id, { theorem: l.theorem, title: l.title, url: l.url }])),
		probeLeaves: Object.fromEntries((p.probes ?? []).filter((r) => r.state === 'finished').map((r) => [r.trial.slice(1), r.leaves.map((l) => l.id)])),
	};
}

/** The trials table's order: by trial, trial N's outline probes just before trial N. */
export const sortTrials = (ts: TrialSum[]) => [...ts].sort((a, b) => a.number - (a.probe ? 0.5 : 0) - (b.number - (b.probe ? 0.5 : 0)));

/** A campaign that is a trial or a probe round after the frozen ones: its number and kind. */
function liveCampaign(c: Campaign): { number: number; probe: boolean } | null {
	const t = trialOf(c.id);
	if (!t || t.number <= FROZEN_THROUGH) return null;
	const kind = c.labels.kind;
	if (kind && kind !== (t.probe ? 'probes' : 'trial')) return null;
	return t;
}

/** A campaign's tasks of one leaf: by its leaf id, else its alias (a task made before v7 names the alias "leaf"). */
const leafTasks = (ts: PublicTask[], item: CampaignItem) =>
	ts.filter((t) => t.labels.leaf === item.id || (!!item.labels.alias && (t.labels.alias === item.labels.alias || t.labels.leaf === item.labels.alias)));
const started = (t: PublicTask) => !!t.started_at || (t.steps ?? 0) > 0;
/** A task's tokens: the explicit fields, else (an older gateway) from the result's own fields. */
const tokensOf = (t: PublicTask) => {
	if (t.tokens_computed !== undefined) return { computed: t.tokens_computed, withCache: t.tokens_with_cache ?? null };
	if (t.cached_tokens === null) return { computed: null, withCache: null };
	const p = t.prompt_tokens ?? 0, o = t.completion_tokens ?? 0;
	return { computed: p + o, withCache: p + t.cached_tokens + o };
};
const tokSum = (ts: PublicTask[]) => {
	let computed = 0, withCache = 0, exact = true;
	for (const t of ts) {
		const k = tokensOf(t);
		if (k.computed === null || k.withCache === null) {
			if (started(t)) exact = false;
			computed += k.computed ?? 0;
			continue;
		}
		computed += k.computed;
		withCache += k.withCache;
	}
	return { computed, withCache, exact };
};

/** The leaves closed by reuse in a campaign (an accepted reuse result): credited in the reuse note, never as a run. */
const reusedIn = (rs: ExtResult[], c: string) => new Set(rs.filter((r) => r.campaign === c && r.labels.kind === 'reuse' && r.item).map((r) => r.item!));

/** A trial campaign's runs: each leaf with a task that started, its outcome and counts from its tasks. */
export function campaignRuns(c: Campaign, tasks: PublicTask[], results: ExtResult[]): RunRec[] {
	const t = trialOf(c.id)!;
	const mine = tasks.filter((x) => x.labels.trial === c.id);
	const reused = reusedIn(results, c.id);
	const out: RunRec[] = [];
	for (const item of c.items) {
		if (reused.has(item.id)) continue;
		const ts = leafTasks(mine, item);
		if (!ts.some(started)) continue;
		const tok = tokSum(ts);
		out.push({
			trial: t.number, leaf: item.id, alias: item.labels.alias ?? '', group: item.role === 'calibration' ? 'calibration' : 'open',
			role: item.role === 'calibration' || item.role === 'comparison' ? item.role : 'open', scored: true, stopped: false, date: day(c.started_at),
			proved: ts.some((x) => provesLeaf({ kind: x.role ?? '', state: x.state, outcome: x.outcome })),
			model_min: sum(ts, (x) => x.model_min ?? 0), ...tok,
		});
	}
	return out;
}

/** A campaign as a trials-table row. */
export function campaignSum(c: Campaign, tasks: PublicTask[], results: ExtResult[]): TrialSum {
	const t = trialOf(c.id)!;
	const count = (rs: RunRec[]): Count => ({ proved: rs.filter((r) => r.proved).length, runs: rs.length });
	const base = {
		label: c.id.slice(1), number: t.number, probe: t.probe, source: 'live' as const, status: c.status, start: day(c.started_at), end: day(c.ended_at),
		scaffold: c.labels.scaffold ?? '',
	};
	if (t.probe) {
		const ts = tasks.filter((x) => x.labels.trial === c.id);
		const none = { proved: 0, runs: 0 };
		return { ...base, all: none, calibration: none, open: none, model_min: sum(ts, (x) => x.model_min ?? 0), ...tokSum(ts) };
	}
	const rs = campaignRuns(c, tasks, results);
	return {
		...base, all: count(rs), calibration: count(rs.filter((r) => r.group === 'calibration')), open: count(rs.filter((r) => r.group === 'open')),
		model_min: sum(rs, (r) => r.model_min), computed: sum(rs, (r) => r.computed), withCache: sum(rs, (r) => r.withCache), exact: rs.every((r) => r.exact),
	};
}

/** A result as a submission, its theorem and title from its campaign's item or the build's submission of the leaf. */
function resultSub(r: ExtResult, campaigns: Campaign[], built: SubRec[]): SubRec | null {
	if (!r.item) return null;
	const c = campaigns.find((x) => x.id === r.campaign);
	const item = c?.items.find((i) => i.id === r.item) ?? campaigns.flatMap((x) => x.items).find((i) => i.id === r.item);
	const b = built.find((s) => s.leaf === r.item);
	const t = trialOf(r.labels.trial ?? r.campaign);
	const kind = r.labels.kind === 'reuse' ? 'reuse' : 'proof';
	const reuses = kind === 'reuse' && (r.labels.reuses || b?.reuses)
		? {
				...(b?.reuses ?? { theorem: '' }),
				...(r.labels.reuses ? { theorem: r.labels.reuses } : {}),
				...(r.labels.relation ? { relation: r.labels.relation } : {}),
				...(r.labels.reuses_url && theoremURL.test(r.labels.reuses_url) ? { url: r.labels.reuses_url } : {}),
				...(r.labels.reuses_author ? { author: r.labels.reuses_author } : {}),
			}
		: undefined;
	return {
		leaf: r.item,
		alias: r.labels.alias ?? item?.labels.alias ?? b?.alias ?? '',
		kind,
		theorem: item?.labels.theorem ?? b?.theorem ?? r.item,
		title: item?.title ?? b?.title ?? '',
		url: r.url ?? item?.url ?? b?.url ?? '',
		trial: t?.number ?? b?.trial ?? 0,
		...(kind === 'reuse' ? { found: t?.probe ? 'probes' : 'trial' } : {}),
		...(reuses ? { reuses } : {}),
		submitted_at: r.submitted_at,
		verdict: r.state,
		verdict_at: r.verdict_at,
	} as SubRec;
}

/** Trials 1 to 5 and any later trial the gateway has no campaign of stay the build's (by the trials table's label). */
const keepBuilt = (h: History, live: Records) => {
	const have = new Set(live.campaigns.filter((c) => liveCampaign(c)).map((c) => c.id.slice(1)));
	return (label: string, n: number) => n <= FROZEN_THROUGH || !have.has(label);
};

/** Every run: the build's that stand and the gateway's trials'. */
export function allRuns(h: History, live: Records): RunRec[] {
	const keep = keepBuilt(h, live);
	const camps = live.campaigns.filter((c) => liveCampaign(c) && !trialOf(c.id)!.probe);
	const runs = [...h.runs.filter((r) => keep(String(r.trial), r.trial)), ...camps.flatMap((c) => campaignRuns(c, live.tasks.tasks, live.results))];
	return runs.sort((a, b) => a.trial - b.trial); // stable: a trial's leaves in their order
}

/** The page's numbers and rows: the build's history alone (`live` null), or with the gateway's records. */
export function scienceView(h: History, live: Records | null): ScienceView {
	let runs = h.runs, trials = h.trials, submissions = h.submissions, current = h.current;
	if (live) {
		const camps = live.campaigns.filter((c) => liveCampaign(c));
		const ts = live.tasks.tasks;
		const keep = keepBuilt(h, live);
		runs = allRuns(h, live);
		trials = sortTrials([
			...h.trials.filter((t) => keep(t.label, t.number)),
			...camps.map((c) => campaignSum(c, ts, live.results)).filter((t) => t.start !== null || t.status !== 'preparing'),
		]);
		const fromGateway = live.results.map((r) => resultSub(r, live.campaigns, h.submissions)).filter((s): s is SubRec => !!s);
		const got = new Set(fromGateway.map((s) => s.leaf));
		submissions = [...h.submissions.filter((s) => !got.has(s.leaf)), ...fromGateway].sort((a, b) => a.submitted_at.localeCompare(b.submitted_at));
		const newest = camps.filter((c) => !trialOf(c.id)!.probe).sort((a, b) => trialOf(b.id)!.number - trialOf(a.id)!.number)[0];
		current = newest && newest.status !== 'finished'
			? { number: trialOf(newest.id)!.number, status: newest.status, start: day(newest.started_at), scaffold: newest.labels.scaffold ?? '' }
			: newest ? null : h.current;
	}
	const open = runs.filter((r) => r.group === 'open');
	const openProvedLeaves = [...new Map(open.filter((r) => r.proved).map((r) => [r.leaf, { leaf: r.leaf, alias: r.alias }])).values()];
	const probes = trials.filter((t) => t.probe);
	const real = trials.filter((t) => !t.probe && t.all.runs > 0);
	const kpis: Kpis = {
		accepted: submissions.filter((s) => s.verdict === 'accepted').length,
		submitted: submissions.length,
		reused: submissions.filter((s) => s.kind === 'reuse' && s.verdict === 'accepted').length,
		openProved: openProvedLeaves.length,
		openTried: new Set(open.map((r) => r.leaf)).size,
		openProvedLeaves,
		runs: runs.length,
		proved: runs.filter((r) => r.proved).length,
		calibrationRuns: runs.filter((r) => r.group === 'calibration').length,
		calibrationProved: runs.filter((r) => r.group === 'calibration' && r.proved).length,
		hours: sum(runs, (r) => r.model_min) / 60,
		probeHours: sum(probes, (t) => t.model_min) / 60,
		lastTrial: Math.max(0, ...real.map((t) => t.number)),
		computed: sum(trials, (t) => t.computed),
		withCache: sum(trials, (t) => t.withCache),
	};
	return { source: live ? 'live' : 'build', updated: live?.updated ?? h.generated_at, kpis, submissions, trials, current };
}

// ---- a trial's own list (its leaves, each with its outlines and tasks), from trial 6 on

export interface DetailLeaf {
	id: string;
	alias: string;
	theorem: string;
	title: string;
	url: string;
	role: string;
	/** The trials before this one that ran the leaf. */
	earlier: number[];
	/** The leaf's line ("Proved: outline 1 assembled", "Not proved: outline 2 was refuted (hole 2 is false)"). */
	summary: string;
	proved: boolean;
	detail: LeafDetail;
	/** The theorem a leaf closed by reuse imports. */
	reuse?: string;
}
export interface TrialDetail {
	sum: TrialSum;
	budget: string;
	/** The trial's own words: the campaign's text, else the build's summary (plain text). */
	text: string;
	leaves: DetailLeaf[];
	/** Proofs from the trial that prove2me accepted, and leaves it closed by reuse. */
	accepted: number;
	reused: number;
}

/** A campaign's leaves and tasks as the leaf view's data document (the trial, its leaves and submissions). */
export function docFromCampaign(c: Campaign, results: ExtResult[]): CurrentDoc {
	const t = trialOf(c.id)!;
	return {
		schema: 'campaign',
		project: 'prove2me',
		updated_at: c.updated_at || c.started_at || '',
		trial: { id: c.id, number: t.number, scaffold: c.labels.scaffold ?? '', start: day(c.started_at), status: c.status, description: c.text, budget: c.labels.budget ?? '' },
		leaves: c.items
			.filter((i) => leafID.test(i.id))
			.map((i) => ({ id: i.id, alias: i.labels.alias ?? i.id, role: (['calibration', 'open', 'comparison'].includes(i.role) ? i.role : 'open') as 'open', theorem: i.labels.theorem ?? '', url: i.url })),
		submissions: results
			.filter((r) => r.campaign === c.id && r.item)
			.map((r) => ({
				leaf: r.item!, alias: r.labels.alias ?? '', kind: r.labels.kind === 'reuse' ? 'reuse' : 'proof', theorem: '', url: r.url ?? '', trial: t.number,
				submitted_at: r.submitted_at, verdict: r.state, verdict_at: r.verdict_at,
				...(r.labels.reuses ? { reuses: { theorem: r.labels.reuses, url: '', relation: r.labels.relation as 'same' | 'part' | undefined } } : {}),
			})),
	};
}

const earlierOf = (runs: RunRec[], leaf: string, n: number) => [...new Set(runs.filter((r) => r.leaf === leaf && r.trial < n).map((r) => r.trial))].sort((a, b) => a - b);

/** A trial campaign's own list from the gateway's tasks. */
export function campaignDetail(c: Campaign, live: Records, runs: RunRec[]): TrialDetail {
	const doc = docFromCampaign(c, live.results);
	const sum = campaignSum(c, live.tasks.tasks, live.results);
	const q = trialQueue(live.tasks, doc);
	const leaves = liveLeaves(q, c.id, doc.leaves.map((l) => l.alias));
	const reused = reusedIn(live.results, c.id);
	const out: DetailLeaf[] = [];
	for (const item of c.items) {
		const alias = item.labels.alias ?? item.id;
		const l = leaves.find((x) => x.leaf === alias);
		if (!l) continue;
		const sub = live.results.find((r) => r.campaign === c.id && r.item === item.id && r.labels.kind === 'reuse');
		const reuse = reused.has(item.id) ? sub?.labels.reuses || 'a theorem already proved on prove2me' : undefined;
		out.push({
			id: item.id, alias, theorem: item.labels.theorem ?? '', title: item.title, url: item.url, role: item.role, earlier: earlierOf(runs, item.id, sum.number),
			summary: leafPlan(l.detail, [], reuse).summary, proved: l.proved, detail: l.detail, ...(reuse ? { reuse } : {}),
		});
	}
	const rs = live.results.filter((r) => r.campaign === c.id && r.state === 'accepted');
	return { sum, budget: c.labels.budget ?? '', text: c.text, leaves: out, accepted: rs.filter((r) => r.labels.kind !== 'reuse').length, reused: rs.filter((r) => r.labels.kind === 'reuse').length };
}

/** A finished trial of the build's data (trial 6 on, v5 tasks) as its own list. */
export function builtDetail(p: Prove2me, h: History, n: number): TrialDetail | null {
	const tr = p.trials.find((t) => t.trial === n);
	const sum = h.trials.find((t) => !t.probe && t.number === n);
	if (!tr || !sum) return null;
	const ids: Record<string, string> = Object.fromEntries(p.leaves.map((l) => [l.theorem, l.id]));
	const reuseOf = Object.fromEntries(p.submissions.filter((s) => s.kind === 'reuse' && s.reuses && s.trial === n).map((s) => [s.theorem, s.reuses!.theorem]));
	const leaves = tr.leaves.filter((r) => r.tasks?.length).map((r): DetailLeaf => {
		const detail = leafDetail((r.tasks as StaticTask[]).map(fromStatic), (r.outlines ?? []) as Outline[]);
		const id = ids[r.theorem] ?? idFromURL(r.url);
		return {
			id, alias: r.leaf, theorem: r.theorem, title: r.title, url: r.url, role: r.group, earlier: r.earlier,
			summary: leafPlan(detail, [], reuseOf[r.theorem]).summary, proved: r.outcome === 'proved', detail,
		};
	});
	const subs = p.submissions.filter((s) => s.trial === n && s.verdict === 'accepted');
	return { sum, budget: tr.caps, text: tr.summary, leaves, accepted: subs.filter((s) => s.kind !== 'reuse').length, reused: subs.filter((s) => s.kind === 'reuse').length };
}

/** Every trial's own list from trial 6 on: the gateway's where it has the campaign, else the build's. */
export function trialDetails(built: TrialDetail[], live: Records | null, h: History): TrialDetail[] {
	if (!live) return built;
	const runs = allRuns(h, live);
	const camps = live.campaigns.filter((c) => liveCampaign(c) && !trialOf(c.id)!.probe && (c.started_at || c.status !== 'preparing'));
	const have = new Set(camps.map((c) => trialOf(c.id)!.number));
	return [...built.filter((d) => !have.has(d.sum.number)), ...camps.map((c) => campaignDetail(c, live, runs))].sort((a, b) => a.sum.number - b.sum.number);
}


/** The build's rows of the table of every task with tokens computed (the export's `tokens` hold the cache re-reads):
 * from each run's or task's own counts; a probe task's are not in the build (its round's sums are), so only the
 * gateway's row of it has them. */
export function builtTaskRows(p: Prove2me): TaskTableRow[] {
	const computed = new Map<string, number>();
	for (const tr of p.trials)
		for (const r of tr.leaves) {
			if (r.tasks?.length) {
				for (const x of r.tasks) if (x.prompt_tokens !== null && x.output_tokens !== null) computed.set(x.id, x.prompt_tokens - (x.cached_tokens ?? 0) + x.output_tokens);
			} else computed.set(`t${tr.trial}-${r.leaf}`, computedOf(r));
		}
	return p.tasks.map((r) => ({ ...r, tokens: computed.get(r.id) ?? null, tokens_with_cache: r.tokens }));
}

/** "Working on now": every trial that has not finished (preparing or running), oldest first (2026-11-20: trial 9
 * dropped out of it when trial 10 was posted, with six of its tasks still open). Each is the gateway's trial campaign as
 * a document (the gateway is the record), or the project's data document instead when it names that
 * trial (it alone has each outline's holes) or a newer one the gateway has no campaign of yet. With no trial open, the
 * newest trial as before (it has ended; the next trial's outline probes show under it). Without the campaigns, the
 * document alone; empty with neither. */
export function nowDocs(doc: CurrentDoc | null, live: Records | null): CurrentDoc[] {
	const num = (c: Campaign) => trialOf(c.id)!.number;
	const camps = (live?.campaigns ?? []).filter((c) => liveCampaign(c) && !trialOf(c.id)!.probe).sort((a, b) => num(a) - num(b));
	const newest = camps[camps.length - 1];
	if (!newest) return doc ? [doc] : [];
	const asDoc = (c: Campaign) => (doc && doc.trial.id === c.id ? doc : docFromCampaign(c, live!.results));
	const open = camps.filter((c) => c.status !== 'finished').map(asDoc);
	if (doc && doc.trial.number > num(newest) && doc.trial.status !== 'finished') open.push(doc);
	if (open.length) return open;
	return [doc && doc.trial.number >= num(newest) ? doc : asDoc(newest)];
}

/** A URL's JSON in the browser, within `ms`; throws on an HTTP error, a timeout or a body that is not JSON. */
export async function getJSON(url: string, ms = 8000): Promise<unknown> {
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), ms);
	try {
		const res = await fetch(url, { signal: ctl.signal, headers: { accept: 'application/json' } });
		if (!res.ok) throw new Error(String(res.status));
		return await res.json();
	} finally {
		clearTimeout(timer);
	}
}

/** The home page's and /science/'s numbers, live: the build's history with the gateway's records (only the tasks that
 * decide a leaf); null when the gateway does not answer (the page keeps the build's). */
export async function liveSummary(h: History, get: (url: string) => Promise<unknown> = getJSON): Promise<ScienceView | null> {
	const live = await fetchRecords(get, DECIDING_TASKS_URL);
	return live ? scienceView(h, live) : null;
}

// ---- every leaf: the scorecard by role and the list of every leaf tried (owner, 2026-11-23: live like the rest)

export type LeafRole = 'calibration' | 'open' | 'comparison';
export type LeafOutcome = 'accepted' | 'reused' | 'proved' | 'not_proved' | 'stopped' | 'not_scored';
/** One leaf over every run of it. */
export interface LeafRow {
	id: string;
	/** The ids the trials used, in the order they first appear. */
	aliases: string[];
	theorem: string;
	title: string;
	url: string;
	roles: LeafRole[];
	trials: number[];
	/** The day it was first tried (its first run's trial start; a leaf closed by reuse, the day of that submission). */
	first: string | null;
	runs: number;
	provedRuns: number;
	/** The best of its runs: accepted by prove2me, closed by reuse, proved, not proved, stopped by us, not scored. */
	outcome: LeafOutcome;
	/** Null for a leaf with no run (closed by reuse). */
	minutes: number | null;
	computed: number | null;
	withCache: number | null;
	exact: boolean;
}
/** The scorecard's row of one role (a leaf counts under every role it had). */
export interface RoleRow {
	role: LeafRole;
	leaves: number;
	runs: number;
	proved: number;
	/** Proofs prove2me accepted, under the role of the run that proved the leaf. */
	accepted: number;
	reused: number;
	hours: number;
	computed: number;
	withCache: number;
	exact: boolean;
}
export interface LeavesView {
	leaves: LeafRow[];
	roles: RoleRow[];
	/** The outline probe rounds: their leaves, model hours and tokens (no runs); null with none. */
	probes: { leaves: number; hours: number; computed: number; withCache: number; exact: boolean } | null;
	all: { leaves: number; runs: number; proved: number; accepted: number; reused: number; hours: number; computed: number; withCache: number; exact: boolean };
	/** Runs cut short by a fault on our side, or stopped by us because of one. */
	notScored: number;
}

const ROLES: LeafRole[] = ['calibration', 'open', 'comparison'];

/** Every leaf tried, from the build's history alone (`live` null) or with the gateway's records: trials 1 to 5 (and any
 * later trial the gateway has no campaign of) from the build, trial 6 on from the gateway. The leaves keep the order
 * they were first tried in; leaves closed by reuse (no run) come last. */
export function leavesView(h: History, live: Records | null): LeavesView {
	const v = scienceView(h, live);
	const runs = live ? allRuns(h, live) : h.runs;
	const info: Record<string, LeafInfo> = { ...h.leafInfo };
	for (const c of live?.campaigns ?? []) for (const i of c.items) if (leafID.test(i.id)) info[i.id] ??= { theorem: i.labels.theorem ?? '', title: i.title, url: i.url };
	const accepted = v.submissions.filter((s) => s.verdict === 'accepted');
	const proofs = accepted.filter((s) => s.kind !== 'reuse');
	const reuses = accepted.filter((s) => s.kind === 'reuse');
	const ids = [...new Set([...runs.map((r) => r.leaf), ...reuses.map((s) => s.leaf)])];
	const leaves = ids.map((id): LeafRow => {
		const rs = runs.filter((r) => r.leaf === id);
		const reuse = reuses.find((s) => s.leaf === id);
		const i = info[id] ?? reuse ?? { theorem: id, title: '', url: '' };
		const outcome: LeafOutcome = proofs.some((s) => s.leaf === id)
			? 'accepted'
			: reuse
				? 'reused'
				: rs.some((r) => r.proved)
					? 'proved'
					: rs.some((r) => r.scored)
						? 'not_proved'
						: rs.some((r) => r.stopped)
							? 'stopped'
							: 'not_scored';
		const roles = new Set<LeafRole>(rs.map((r) => r.role));
		if (!rs.length) roles.add('open'); // closed by reuse: an open leaf
		const dates = rs.map((r) => r.date).filter((d): d is string => !!d);
		return {
			id, aliases: [...new Set([...rs.map((r) => r.alias), reuse?.alias ?? ''].filter(Boolean))], theorem: i.theorem, title: i.title, url: i.url,
			roles: ROLES.filter((r) => roles.has(r)), trials: [...new Set(rs.map((r) => r.trial))].sort((a, b) => a - b),
			first: dates.length ? dates.sort()[0] : reuse ? reuse.submitted_at.slice(0, 10) : null,
			runs: rs.length, provedRuns: rs.filter((r) => r.proved).length, outcome,
			minutes: rs.length ? sum(rs, (r) => r.model_min) : null, computed: rs.length ? sum(rs, (r) => r.computed) : null,
			withCache: rs.length ? sum(rs, (r) => r.withCache) : null, exact: rs.every((r) => r.exact),
		};
	});
	// a proof accepted counts under the role of the run that proved it (in the submission's trial)
	const roleOfProof = (s: SubRec): LeafRole =>
		(runs.find((r) => r.leaf === s.leaf && r.trial === s.trial && r.proved) ?? runs.find((r) => r.leaf === s.leaf && r.proved))?.role ?? 'open';
	const roles = ROLES.map((role): RoleRow => {
		const rs = runs.filter((r) => r.role === role);
		return {
			role, leaves: leaves.filter((l) => l.roles.includes(role)).length, runs: rs.length, proved: rs.filter((r) => r.proved).length,
			accepted: proofs.filter((s) => roleOfProof(s) === role).length, reused: role === 'open' ? reuses.length : 0,
			hours: sum(rs, (r) => r.model_min) / 60, computed: sum(rs, (r) => r.computed), withCache: sum(rs, (r) => r.withCache), exact: rs.every((r) => r.exact),
		};
	}).filter((r) => r.leaves > 0);
	const probeTrials = v.trials.filter((t) => t.probe);
	const keep = live ? keepBuilt(h, live) : () => true;
	const probeIds = new Set([
		...Object.entries(h.probeLeaves).filter(([label]) => keep(label, Number(label.slice(0, -1)))).flatMap(([, ids]) => ids),
		...(live?.campaigns ?? []).filter((c) => liveCampaign(c)?.probe).flatMap((c) => c.items.map((i) => i.id)),
	]);
	const probes = probeTrials.length
		? { leaves: probeIds.size, hours: sum(probeTrials, (t) => t.model_min) / 60, computed: sum(probeTrials, (t) => t.computed), withCache: sum(probeTrials, (t) => t.withCache), exact: probeTrials.every((t) => t.exact) }
		: null;
	return {
		leaves,
		roles,
		probes,
		all: {
			leaves: leaves.length, runs: runs.length, proved: runs.filter((r) => r.proved).length, accepted: proofs.length, reused: reuses.length,
			hours: (sum(runs, (r) => r.model_min) + sum(probeTrials, (t) => t.model_min)) / 60,
			computed: v.kpis.computed, withCache: v.kpis.withCache, exact: v.trials.every((t) => t.exact),
		},
		notScored: runs.filter((r) => !r.scored).length,
	};
}
