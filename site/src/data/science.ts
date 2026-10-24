// Science results for /science/tauceti/ and the home page. The data is science/tauceti.json,
// exported from our own run records by an export script (public fields only) and committed here;
// it is also served as-is at /science/tauceti.json. Anything malformed is dropped, so the page
// shows only what the file really says.
import raw from './science/tauceti.json';

export type PR = {
	number: number;
	url: string;
	title: string;
	type: string;
	state: 'merged' | 'open' | 'closed';
	opened: Date;
	merged: Date | null;
	closed: Date | null;
	fixRounds: number;
	firstReviewApproved: boolean;
	/** Over every attempt at the PR's task (failed ones and fix rounds included); null = never recorded. */
	attempts: number | null;
	qwenTokens: number | null;
	qwenOutputTokens: number | null;
	gpuMinutes: number | null;
	attemptsWithoutTokens: number;
	attemptsWithoutGpuTime: number;
};

/** PRs of one type, with subtotals; values unknown on every PR stay null. */
export type Group = {
	type: string;
	prs: PR[];
	merged: number;
	qwenTokens: number | null;
	gpuMinutes: number | null;
	attemptsWithoutTokens: number;
	attemptsWithoutGpuTime: number;
};

const n = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);
const date = (v: unknown): Date | null => {
	if (typeof v !== 'string') return null;
	const d = new Date(v);
	return Number.isNaN(d.getTime()) ? null : d;
};

const prs: PR[] = (Array.isArray(raw.prs) ? raw.prs : [])
	.map((p): PR | null => {
		const opened = date(p.opened);
		const state = p.state === 'merged' || p.state === 'open' || p.state === 'closed' ? p.state : null;
		const url = typeof p.url === 'string' && p.url.startsWith('https://github.com/') ? p.url : null;
		if (!n(p.number) || !opened || !state || !url || n(p.fix_rounds) === null) return null;
		const merged = date(p.merged);
		if (state === 'merged' && !merged) return null;
		return {
			number: p.number,
			url,
			title: typeof p.title === 'string' ? p.title : '',
			type: typeof p.type === 'string' ? p.type : '',
			state,
			opened,
			merged: state === 'merged' ? merged : null,
			closed: state === 'closed' ? date(p.closed) : null,
			fixRounds: p.fix_rounds,
			firstReviewApproved: p.first_review_approved === true,
			attempts: n(p.attempts),
			qwenTokens: n(p.qwen_tokens),
			qwenOutputTokens: n(p.qwen_output_tokens),
			gpuMinutes: n(p.gpu_minutes),
			attemptsWithoutTokens: n(p.attempts_without_tokens) ?? 0,
			attemptsWithoutGpuTime: n(p.attempts_without_gpu_time) ?? 0,
		};
	})
	.filter((p): p is PR => p !== null);

// Grouped by type: most PRs first (ties alphabetical), oldest first inside a group.
const sumOrNull = (xs: (number | null)[]) => (xs.some((x) => x !== null) ? xs.reduce<number>((a, x) => a + (x ?? 0), 0) : null);
const byType = new Map<string, PR[]>();
for (const p of [...prs].sort((a, b) => +a.opened - +b.opened)) byType.set(p.type, [...(byType.get(p.type) ?? []), p]);
const groups: Group[] = [...byType.entries()]
	.map(([type, list]) => ({
		type,
		prs: list,
		merged: list.filter((p) => p.state === 'merged').length,
		qwenTokens: sumOrNull(list.map((p) => p.qwenTokens)),
		gpuMinutes: ((m) => (m === null ? null : Math.round(m * 10) / 10))(sumOrNull(list.map((p) => p.gpuMinutes))),
		attemptsWithoutTokens: list.reduce((a, p) => a + p.attemptsWithoutTokens, 0),
		attemptsWithoutGpuTime: list.reduce((a, p) => a + p.attemptsWithoutGpuTime, 0),
	}))
	.sort((a, b) => b.prs.length - a.prs.length || a.type.localeCompare(b.type));

const t = raw.totals ?? {};
const asOf = date(raw.states_as_of);

export const science = {
	model: typeof raw.model === 'string' ? raw.model : 'an open-weight model',
	project: 'Tau Ceti',
	projectURL: 'https://taucetiproject.org/',
	repository: 'https://github.com/TauCetiProject/TauCeti',
	prs,
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
	asOf,
	jsonPath: '/science/tauceti.json',
	pagePath: '/science/tauceti/',
};

export const rawScience = raw;

const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
export const dayLabel = (d: Date) => `${d.getUTCDate()} ${months[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
export const shortDay = (d: Date) => `${d.getUTCDate()} ${months[d.getUTCMonth()]}`;
export const isoDay = (d: Date) => d.toISOString().slice(0, 10);

export const exact = (v: number) => new Intl.NumberFormat('en').format(v);

/** 16968593 -> "17.0M", 8484296 -> "8.5M", 405114 -> "405k". */
export const compact = (v: number) =>
	v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v));
