// The science data's shape and formatting (no data here, so the browser's bundle stays small):
// normalize() turns the committed JSON or the gateway's live JSON into what the page renders.
export type Size = 'easy' | 'medium' | 'hard';

export type Change = { rubric: string; verdict: string; reason: string; url: string };

/** One reviewed commit of a PR, with the rubrics that asked for changes at that commit. */
export type Round = { round: number; commit: string; at: Date; result: string; changes: Change[] };

type Cost = {
	/** Over every attempt at the task (failed ones and fix rounds included); null = never recorded. */
	attempts: number | null;
	qwenTokens: number | null;
	qwenOutputTokens: number | null;
	gpuMinutes: number | null;
	attemptsWithoutTokens: number;
	attemptsWithoutGpuTime: number;
};

export type PR = Cost & {
	number: number;
	url: string;
	title: string;
	type: string;
	size: Size | null;
	state: 'merged' | 'open' | 'closed';
	opened: Date;
	merged: Date | null;
	closed: Date | null;
	fixRounds: number;
	firstReviewApproved: boolean;
	reviewURL: string | null;
	rounds: Round[];
};

/** A task whose attempts have not led to a PR (yet): failures before a PR count too. */
export type Task = Cost & {
	target: string;
	type: string;
	size: Size | null;
	state: 'failed' | 'checks-passed';
	lastAttempt: Date | null;
};

/** What the runner works on now (live data only). */
export type Now = {
	reportedAt: Date;
	running: { target: string; type: string; size: Size | null; state: 'running' | 'interrupted' }[];
	queued: number;
};

/** PRs and tasks without a PR of one type, with subtotals; values unknown on every row stay null. */
export type Group = {
	type: string;
	prs: PR[];
	tasks: Task[];
	merged: number;
	qwenTokens: number | null;
	gpuMinutes: number | null;
	attemptsWithoutTokens: number;
	attemptsWithoutGpuTime: number;
};

const n = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const date = (v: unknown): Date | null => {
	if (typeof v !== 'string') return null;
	const d = new Date(v);
	return Number.isNaN(d.getTime()) ? null : d;
};
const size = (v: unknown): Size | null => (v === 'easy' || v === 'medium' || v === 'hard' ? v : null);
const gh = (v: unknown): string | null => (typeof v === 'string' && v.startsWith('https://github.com/') ? v : null);
const arr = (v: unknown): any[] => (Array.isArray(v) ? v : []);

const cost = (p: any): Cost => ({
	attempts: n(p.attempts),
	qwenTokens: n(p.qwen_tokens),
	qwenOutputTokens: n(p.qwen_output_tokens),
	gpuMinutes: n(p.gpu_minutes),
	attemptsWithoutTokens: n(p.attempts_without_tokens) ?? 0,
	attemptsWithoutGpuTime: n(p.attempts_without_gpu_time) ?? 0,
});

const rounds = (v: unknown): Round[] =>
	arr(v)
		.map((r): Round | null => {
			const at = date(r?.at);
			if (!at || !n(r.round) || !/^[0-9a-f]{7,40}$/.test(str(r.commit))) return null;
			const changes = arr(r.changes)
				.map((c): Change | null => {
					const url = gh(c?.url);
					return url && str(c.rubric) ? { rubric: c.rubric, verdict: str(c.verdict), reason: str(c.reason), url } : null;
				})
				.filter((c): c is Change => c !== null);
			return { round: r.round, commit: r.commit, at, result: str(r.result), changes };
		})
		.filter((r): r is Round => r !== null);

const sumOrNull = (xs: (number | null)[]) => (xs.some((x) => x !== null) ? xs.reduce<number>((a, x) => a + (x ?? 0), 0) : null);

export function normalize(d: any) {
	const prs: PR[] = arr(d?.prs)
		.map((p): PR | null => {
			const opened = date(p?.opened);
			const state = p.state === 'merged' || p.state === 'open' || p.state === 'closed' ? p.state : null;
			const url = gh(p.url);
			if (!n(p.number) || !opened || !state || !url || n(p.fix_rounds) === null) return null;
			const merged = date(p.merged);
			if (state === 'merged' && !merged) return null;
			return {
				number: p.number,
				url,
				title: str(p.title),
				type: str(p.type),
				size: size(p.size),
				state,
				opened,
				merged: state === 'merged' ? merged : null,
				closed: state === 'closed' ? date(p.closed) : null,
				fixRounds: p.fix_rounds,
				firstReviewApproved: p.first_review_approved === true,
				reviewURL: gh(p.review_url),
				rounds: rounds(p.rounds),
				...cost(p),
			};
		})
		.filter((p): p is PR => p !== null);

	const tasks: Task[] = arr(d?.tasks_without_pr)
		.map((t): Task | null => {
			const state = t?.state === 'failed' || t?.state === 'checks-passed' ? t.state : null;
			if (!state || !str(t.target)) return null;
			return { target: t.target, type: str(t.type), size: size(t.size), state, lastAttempt: date(t.last_attempt), ...cost(t) };
		})
		.filter((t): t is Task => t !== null);

	// Grouped by type: most PRs first, then most tasks without a PR (ties alphabetical); oldest
	// first inside a group, PRs before tasks without one.
	const byType = new Map<string, { prs: PR[]; tasks: Task[] }>();
	const get = (t: string) => {
		if (!byType.has(t)) byType.set(t, { prs: [], tasks: [] });
		return byType.get(t)!;
	};
	for (const p of [...prs].sort((a, b) => +a.opened - +b.opened)) get(p.type).prs.push(p);
	for (const t of [...tasks].sort((a, b) => +(a.lastAttempt ?? 0) - +(b.lastAttempt ?? 0))) get(t.type).tasks.push(t);
	const groups: Group[] = [...byType.entries()]
		.map(([type, g]) => {
			const rows: Cost[] = [...g.prs, ...g.tasks];
			const minutes = sumOrNull(rows.map((r) => r.gpuMinutes));
			return {
				type,
				prs: g.prs,
				tasks: g.tasks,
				merged: g.prs.filter((p) => p.state === 'merged').length,
				qwenTokens: sumOrNull(rows.map((r) => r.qwenTokens)),
				gpuMinutes: minutes === null ? null : Math.round(minutes * 10) / 10,
				attemptsWithoutTokens: rows.reduce((a, r) => a + r.attemptsWithoutTokens, 0),
				attemptsWithoutGpuTime: rows.reduce((a, r) => a + r.attemptsWithoutGpuTime, 0),
			};
		})
		.sort((a, b) => b.prs.length - a.prs.length || b.tasks.length - a.tasks.length || a.type.localeCompare(b.type));

	let now: Now | null | undefined = undefined; // undefined: not live data; null: live, no recent report
	if (d && typeof d === 'object' && 'now' in d) {
		const reportedAt = date(d.now?.reported_at);
		now = reportedAt
			? {
					reportedAt,
					running: arr(d.now.running)
						.filter((r) => str(r?.target) && (r.state === 'running' || r.state === 'interrupted'))
						.map((r) => ({ target: r.target, type: str(r.type), size: size(r.size), state: r.state })),
					queued: n(d.now.queued) ?? 0,
				}
			: null;
	}

	const t = d?.totals ?? {};
	return {
		model: typeof d?.model === 'string' ? d.model : 'an open-weight model',
		project: 'Tau Ceti',
		projectURL: 'https://taucetiproject.org/',
		repository: 'https://github.com/TauCetiProject/TauCeti',
		prs,
		tasks,
		groups,
		merged: prs.filter((p) => p.state === 'merged').length,
		closed: prs.filter((p) => p.state === 'closed').length,
		open: prs.filter((p) => p.state === 'open').length,
		firstReviewApproved: prs.filter((p) => p.firstReviewApproved).length,
		attempts: n(t.attempts),
		attemptsPassing: n(t.attempts_passing_local_checks),
		gpuHours: n(t.gpu_hours),
		attemptsWithoutGpuTime: n(t.attempts_without_gpu_time) ?? 0,
		attemptsWithoutTokens: n(t.attempts_without_token_count) ?? 0,
		qwenTokens: n(t.qwen_tokens),
		qwenOutputTokens: n(t.qwen_output_tokens),
		asOf: date(d?.states_as_of),
		generatedAt: date(d?.generated_at),
		now,
		jsonPath: '/science/tauceti.json',
		livePath: '/v1/projects/tauceti/science',
		pagePath: '/science/tauceti/',
	};
}

export type Science = ReturnType<typeof normalize>;

const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
export const dayLabel = (d: Date) => `${d.getUTCDate()} ${months[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
export const shortDay = (d: Date) => `${d.getUTCDate()} ${months[d.getUTCMonth()]}`;
export const isoDay = (d: Date) => d.toISOString().slice(0, 10);

export const exact = (v: number) => new Intl.NumberFormat('en').format(v);

/** 16968593 -> "17.0M", 8484296 -> "8.5M", 405114 -> "405k". */
export const compact = (v: number) =>
	v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v));

/** "5 h ago" for a moment before now (minutes under an hour, days from two days on). */
export const ago = (then: Date, now: Date) => {
	const min = Math.max(0, Math.round((+now - +then) / 60000));
	if (min < 60) return min <= 1 ? 'just now' : `${min} min ago`;
	const h = Math.floor(min / 60);
	return h < 48 ? `${h} h ago` : `${Math.floor(h / 24)} days ago`;
};
