// The numbers on top of the science page, per type of task and over time (S3, the owner's order of
// 2026-10-29), computed from the same normalized data as the rest of the page, so nothing here can
// disagree with the rows below it. Pure: no rendering.
//
// What each number means is said on the page ("How to read this"); the rules behind them:
//  - merged means Tau Ceti merged it (`state`: the runner reads Bors's "[Merged by Bors]" title and
//    comment, because Bors closes a PR without GitHub's merged flag);
//  - approved at the first review counts only PRs that had one, with its n always beside it;
//  - "per merged PR" divides all the work of a type by what got merged: failed attempts, closed PRs,
//    tasks without a PR and internal reviews count; a review's stall does not (reviewsLeftOut).
import type { PR, Science, Task } from './science-model';

/** Below this, a rate is shown as a count, and a mean is marked: it says little yet. */
export const SMALL_SAMPLE = 5;

export type Rate = { x: number; n: number };

export type TypeStats = {
	type: string;
	/** Which series colour: follows the type, never its rank (0: none, for all types together). */
	slot: number;
	opened: number;
	merged: number;
	closed: number;
	open: number;
	withoutPR: number;
	firstReview: Rate;
	roundsFix: number;
	/** null when the data has no internal rounds (older data). */
	roundsInternal: number | null;
	/** Per merged PR; null with nothing merged. */
	roundsPerMerged: number | null;
	tokensPerMerged: number | null;
	minutesPerMerged: number | null;
	gpuMinutes: number;
	prs: PR[];
};

/** The series order of the stacks: adjacent colours validated in this order (dataviz palette check). */
const TYPE_ORDER = ['Namespace move', 'New lemma', 'Docstring cleanup', 'Generalisation', 'Lint fix'];
const day = 86400000;

/** The Monday (UTC) of a date's week. */
const weekOf = (d: Date) => {
	const t = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
	return t - ((d.getUTCDay() + 6) % 7) * day;
};

const firstReview = (prs: PR[]): Rate => {
	const reviewed = prs.filter((p) => p.firstReviewApproved !== null);
	return { x: reviewed.filter((p) => p.firstReviewApproved).length, n: reviewed.length };
};

const sum = (xs: number[]) => xs.reduce((a, x) => a + x, 0);

function typeStats(type: string, slot: number, prs: PR[], tasks: Task[]): TypeStats {
	const merged = prs.filter((p) => p.state === 'merged');
	const work = [...prs, ...tasks];
	const tokens = sum(work.map((r) => (r.qwenTokens ?? 0) + (r.reviewTokens ?? 0)));
	const gpuMinutes = sum(work.map((r) => (r.gpuMinutes ?? 0) + (r.reviewMinutes ?? 0)));
	const roundsFix = sum(merged.map((p) => p.fixRounds));
	const roundsInternal = merged.some((p) => p.internalRounds !== null) ? sum(merged.map((p) => p.internalRounds ?? 0)) : null;
	const per = (v: number) => (merged.length ? v / merged.length : null);
	return {
		type,
		slot,
		opened: prs.length,
		merged: merged.length,
		closed: prs.filter((p) => p.state === 'closed').length,
		open: prs.filter((p) => p.state === 'open').length,
		withoutPR: tasks.length,
		firstReview: firstReview(prs),
		roundsFix,
		roundsInternal,
		roundsPerMerged: per(roundsFix + (roundsInternal ?? 0)),
		tokensPerMerged: tokens ? per(tokens) : null,
		minutesPerMerged: gpuMinutes ? per(gpuMinutes) : null,
		gpuMinutes,
		prs: [...prs].sort((a, b) => +a.opened - +b.opened),
	};
}

export function insights(s: Science) {
	const end = Math.max(+(s.generatedAt ?? 0), ...s.prs.map((p) => +(p.merged ?? p.opened)));
	const first = Math.min(...s.prs.map((p) => +p.opened));
	const weeks: number[] = [];
	if (s.prs.length) for (let w = weekOf(new Date(first)); w <= end; w += 7 * day) weeks.push(w);

	const types = [...new Set([...TYPE_ORDER, ...s.groups.map((g) => g.type)])].filter((t) => s.groups.some((g) => g.type === t));
	const rows = types.map((t) => {
		const g = s.groups.find((x) => x.type === t)!;
		const i = TYPE_ORDER.indexOf(t);
		return typeStats(t, i < 0 ? 0 : i + 1, g.prs, g.tasks);
	});
	const all = typeStats('All types', 0, s.prs, s.tasks);
	return {
		rows,
		all,
		/** How many calendar weeks (Monday, UTC) the data spans. */
		weeks: weeks.length,
		reviewsLeftOut: sum([...s.prs, ...s.tasks].map((r) => r.reviewsLeftOut)),
		attemptsWithoutGpuTime: s.attemptsWithoutGpuTime,
	};
}

export type Insights = ReturnType<typeof insights>;
