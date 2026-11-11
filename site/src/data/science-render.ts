// The rows, "Working on now" and "How to read this" of /science/tauceti/ as HTML, from normalized
// data (science.ts); science-insights.ts puts the page together. Sections whose numbers are missing
// are left out rather than shown as dashes or zeros.
import { ago, compact, dayLabel, exact, isoDay, shortDay, type Active, type Now, type NowTask, type PR, type Science, type Task } from './science-model';

export const esc = (s: unknown) =>
	String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
/** Tau Ceti's text, verbatim: escaped, with `code` spans as code. */
const verbatim = (s: string) => esc(s).replace(/`([^`]+)`/g, '<code>$1</code>');
const attrs = (a: Record<string, string | number>) =>
	Object.entries(a)
		.map(([k, v]) => (v === '' ? ` ${k}` : ` ${k}="${esc(v)}"`))
		.join('');
export const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const pct = (a: number, b: number) => `${Math.round((100 * a) / b)}%`;
const sizeLabel = { easy: 'easy', medium: 'medium', hard: 'hard' };

// A partly known sum shows the sum; the attempts it misses are in its hover text and in "How to read this".
const notRecorded = (n: number) => (n > 0 ? `; ${plural(n, 'attempt')} not recorded` : '');
function tokCell(v: number | null, out: number | null, missing: number) {
	const a = v === null ? { 'data-unknown': '' } : { 'data-tokens': v, title: `${exact(v)} Qwen tokens${out !== null ? `, ${exact(out)} of them written` : ''}${notRecorded(missing)}` };
	return `<td class="num tok" data-label="Qwen tokens"${attrs(a)}>${v === null ? 'not recorded' : compact(v)}</td>`;
}
function minCell(v: number | null, missing: number) {
	const a = v === null ? { 'data-unknown': '' } : { 'data-minutes': v, title: `${v.toFixed(1)} GPU minutes${notRecorded(missing)}` };
	return `<td class="num min" data-label="GPU minutes"${attrs(a)}>${v === null ? 'not recorded' : v.toFixed(1)}</td>`;
}

// Each review round: the commit Tau Ceti reviewed and, for every rubric that asked for changes,
// Tau Ceti's own one-line reason, verbatim, linked to its comment. The quotation marks are text, not
// <q>: a browser copies <q>'s generated marks from a closed <details>, so a pasted page had a stray
// "" for every closed row.
function rounds(p: PR) {
	if (!p.rounds.length) return p.state === 'open' ? '<span class="meta" data-no-review>No review yet</span>' : '';
	const items = p.rounds
		.map((r) => {
			const changes = r.changes
				.map(
					(c) =>
						`<li data-change data-rubric="${esc(c.rubric)}"><span class="rubric">${esc(c.rubric)}</span> ${
							c.reason ? `<span class="reason" data-reason>“${verbatim(c.reason)}”</span> ` : ''
						}<a href="${esc(c.url)}" data-change-link>${c.reason ? 'comment' : 'see the comment'}</a></li>`,
				)
				.join('');
			return `<li data-round="${r.round}" data-result="${esc(r.result)}"><span class="round-head">Round ${r.round} · <code>${esc(
				r.commit.slice(0, 7),
			)}</code> · <time datetime="${isoDay(r.at)}">${shortDay(r.at)}</time> · <strong>${esc(r.result)}</strong></span>${
				changes ? `<ul>${changes}</ul>` : ''
			}</li>`;
		})
		.join('');
	const link = p.reviewURL ? `<a class="review-link" href="${esc(p.reviewURL)}" data-review-link>Tau Ceti’s review</a>` : '';
	return `<details class="sci-rounds" data-rounds><summary>${plural(p.rounds.length, 'review round')}</summary><ol>${items}</ol>${link}</details>`;
}

/**
 * Tau Ceti's pause (since 2026-11-07): at its maintainers' request we stopped sending pull requests
 * and closed the ones still open. The data says only that a pull request was closed, not by whom, so
 * the ones we closed are listed here; while `paused`, a task that passed its local checks will not be
 * sent. The page's notice (pages/science/tauceti.astro) says the same.
 */
export const pause = { paused: true, closedByUs: [24465, 25595, 25788, 25838] };
const closedByUs = (p: PR) => p.state === 'closed' && pause.closedByUs.includes(p.number);

const status = (p: PR) =>
	p.state === 'merged'
		? `Merged ${shortDay(p.merged!)}`
		: p.state === 'closed'
			? closedByUs(p)
				? 'Closed by us: paused at Tau Ceti’s request'
				: 'Closed, not merged'
			: 'Open, in review';

// The first review: approved, changes asked, or not reviewed yet.
const firstReview = (p: PR) =>
	p.firstReviewApproved === null
		? ['pending', 'First review pending']
		: p.firstReviewApproved
			? ['approved', 'Approved at first review']
			: ['changes', 'Changes asked at first review'];

/** The task's id, as we talk about it (T5-NS-3). */
const taskId = (t: string) => (t ? `<span class="task-id" data-task="${esc(t)}">${esc(t)}</span>` : '');

/** Rounds answering our own review; nothing when there were none, or in data older than the field. */
const internalRounds = (p: PR) =>
	p.internalRounds ? `<span class="internal" data-internal-rounds="${p.internalRounds}">${plural(p.internalRounds, 'internal round')}</span>` : '';

/** A row's sort values; null sorts last in either direction. */
export type SortKeys = { number: number | null; title: string; date: number | null; status: number; tokens: number | null; minutes: number | null };
export type SortKey = keyof SortKeys;
export type Sort = { key: SortKey; dir: 'asc' | 'desc' };
/** Newest first. */
export const defaultSort: Sort = { key: 'date', dir: 'desc' };
/** The direction a column starts in: names and statuses from the top, numbers and dates from the largest. */
export const firstDir = (key: SortKey): Sort['dir'] => (key === 'title' || key === 'status' ? 'asc' : 'desc');
const statusOrder = ['merged', 'open', 'closed', 'running', 'queued', 'checks-passed', 'failed'];
export const statusRank = (state: string) => Math.max(0, statusOrder.indexOf(state));

/** The list's one order: the build's, and the browser's after every click and refresh. Ties go newest first. */
export function compareRows(a: SortKeys, b: SortKeys, { key, dir }: Sort) {
	const x = a[key], y = b[key];
	if (x === null || y === null) {
		if (x !== y) return x === null ? 1 : -1;
	} else {
		const c = typeof x === 'string' ? x.localeCompare(y as string) : x - (y as number);
		if (c) return dir === 'asc' ? c : -c;
	}
	return (b.date ?? 0) - (a.date ?? 0) || (b.number ?? 0) - (a.number ?? 0) || a.title.localeCompare(b.title);
}

const sortAttrs = (k: SortKeys) =>
	attrs(
		Object.fromEntries(
			Object.entries({ 'data-sort-number': k.number, 'data-sort-date': k.date, 'data-sort-tokens': k.tokens, 'data-sort-minutes': k.minutes }).filter(([, v]) => v !== null),
		) as Record<string, number>,
	);

function prRow(p: PR, k: SortKeys) {
	const [first, firstText] = firstReview(p);
	const meta = [esc(p.type)];
	if (p.size) meta.push(sizeLabel[p.size]);
	if (p.attempts !== null) meta.push(plural(p.attempts, 'attempt'));
	return `<tr data-pr="${p.number}" data-state="${p.state}" data-type="${esc(p.type)}"${sortAttrs(k)}>
<th scope="row"><a href="${esc(p.url)}">#${p.number}</a>${taskId(p.task)}</th>
<td class="title" data-sort-title>${esc(p.title)}<span class="meta">${meta.join(' · ')}</span>${rounds(p)}</td>
<td class="date" data-label="Date"><time datetime="${isoDay(p.opened)}">${shortDay(p.opened)}</time></td>
<td class="status"><span class="state state-${p.state}"${closedByUs(p) ? ' data-closed-by="us"' : ''}>${status(p)}</span><span class="meta"><span class="first" data-first-review="${first}">${firstText}</span><span class="fix" data-fix-rounds="${p.fixRounds}">${plural(
		p.fixRounds,
		'fix round',
	)}</span>${internalRounds(p)}</span></td>
${tokCell(p.qwenTokens, p.qwenOutputTokens, p.attemptsWithoutTokens)}${minCell(p.gpuMinutes, p.attemptsWithoutGpuTime)}
</tr>`;
}

/** What each kind of run is called on the page, without the words we use internally. */
const runLabels: Record<string, string> = {
	new: 'New task',
	rerun: 'Rerun',
	'rerun-more-time': 'Rerun with more time',
	'fix-review': 'Fix after Tau Ceti’s review',
	'fix-ci': 'Fix after Tau Ceti’s CI',
	'fix-internal': 'Fix after internal review',
	review: 'Internal review',
	followup: 'Follow-up round',
};
/** "Fix after internal review, round 3", "Rerun with more time, try 2"; '' when older data does not say. */
function runLabel(run: string, round?: number | null) {
	const label = runLabels[run];
	if (!label) return '';
	return round ? `${label}, ${run.startsWith('fix') ? 'round' : 'try'} ${round}` : label;
}

/**
 * Where a task without a PR stands: from the live "now" when it is running or queued again, else what
 * its last attempt left. Rendered on its own so the minute's refresh can update it.
 */
export function noPRStatus(t: Pick<Task, 'task' | 'state'>, now: Now | null | undefined) {
	const [cls, state, sub] = noPRStand(t, now);
	return `<span class="state state-${cls}" data-no-pr-status="${cls}">${state}</span><span class="meta"><span>${esc(sub)}</span></span>`;
}

function noPRStand(t: Pick<Task, 'task' | 'state'>, now: Now | null | undefined) {
	const on = now?.active.filter((a) => t.task && a.task === t.task) ?? [];
	const act = on.find((a) => a.state !== 'queued') ?? on[0];
	return act
		? act.state === 'running'
			? ['running', 'Running now', runLabel(act.run)]
			: act.state === 'interrupted'
				? ['running', 'Interrupted, resumes automatically', runLabel(act.run)]
				: ['queued', 'Queued again', runLabel(act.run)]
		: t.state === 'failed'
			? ['failed', 'Failed local checks', 'No pull request sent, not queued']
			: ['checks-passed', 'Passed local checks', pause.paused ? 'Not sent: paused' : 'Waiting for internal review or to be sent'];
}

function taskRow(t: Task, now: Now | null | undefined, k: SortKeys) {
	const meta = [esc(t.type)];
	if (t.size) meta.push(sizeLabel[t.size]);
	if (t.attempts !== null) meta.push(plural(t.attempts, 'attempt'));
	if (t.internalRounds) meta.push(plural(t.internalRounds, 'internal round'));
	return `<tr data-no-pr data-state="${t.state}" data-type="${esc(t.type)}"${t.task ? ` data-task-id="${esc(t.task)}"` : ''}${sortAttrs(k)}>
<th scope="row"><span class="no-pr">No PR</span>${taskId(t.task)}</th>
<td class="title" data-target data-sort-title>${esc(t.target)}<span class="meta">${meta.join(' · ')}</span></td>
<td class="date" data-label="Date">${t.lastAttempt ? `<time datetime="${isoDay(t.lastAttempt)}">${shortDay(t.lastAttempt)}</time>` : ''}</td>
<td class="status">${noPRStatus(t, now)}</td>
${tokCell(t.qwenTokens, t.qwenOutputTokens, t.attemptsWithoutTokens)}${minCell(t.gpuMinutes, t.attemptsWithoutGpuTime)}
</tr>`;
}

const sortHeads: [SortKey, string, string][] = [
	['number', 'PR', ''],
	['title', 'What it does', ''],
	['date', 'Date', ''],
	['status', 'Status', ''],
	['tokens', 'Qwen tokens', 'num'],
	['minutes', 'GPU minutes', 'num'],
];
const sortMark = (dir: Sort['dir']) => (dir === 'asc' ? '▲' : '▼');
/** The header's arrow: `sort` is the active order. Also what the browser sets after a click. */
export const sortHead = ([key, label, cls]: [SortKey, string, string], sort: Sort) => {
	const on = sort.key === key;
	return `<th scope="col"${cls ? ` class="${cls}"` : ''} data-sort-col="${key}"${on ? ` aria-sort="${sort.dir === 'asc' ? 'ascending' : 'descending'}"` : ''}><button type="button" data-sort="${key}">${label}<span class="sort-mark" aria-hidden="true">${on ? sortMark(sort.dir) : ''}</span></button></th>`;
};

/** Every pull request and task without one, one flat list, newest first until a header says otherwise. */
export function rowsTable(s: Science) {
	const rows = [
		...s.prs.map((p) => {
			const k: SortKeys = { number: p.number, title: p.title, date: +p.opened, status: statusRank(p.state), tokens: p.qwenTokens, minutes: p.gpuMinutes };
			return { k, html: prRow(p, k) };
		}),
		...s.tasks.map((t) => {
			const k: SortKeys = {
				number: null,
				title: t.target,
				date: t.lastAttempt ? +t.lastAttempt : null,
				status: statusRank(noPRStand(t, s.now)[0]),
				tokens: t.qwenTokens,
				minutes: t.gpuMinutes,
			};
			return { k, html: taskRow(t, s.now, k) };
		}),
	].sort((a, b) => compareRows(a.k, b.k, defaultSort));
	return `<div class="sci-wrap not-content"><table class="sci-table" data-science-prs>
<thead><tr>${sortHeads.map((h) => sortHead(h, defaultSort)).join('')}</tr></thead>
<tbody>${rows.map((r) => r.html).join('')}</tbody>
</table></div>
<p class="sci-note">The date is the day a pull request opened or, for a task without one, its last attempt. Tokens and minutes cover every attempt at a task, failed
attempts and fix rounds included. Most prompt tokens are the same files re-read from the cache; hover a token count for the exact number and how many tokens the model wrote.</p>`;
}

/** A running or queued task's title: its pull request, linked, when it has one ("#19781 — title"). */
function nowTitle(t: NowTask) {
	if (!t.pr) return esc(t.target || t.type);
	const link = `<a href="${esc(t.pr.url)}" data-now-pr="${t.pr.number}">#${t.pr.number}</a>`;
	const title = esc(t.pr.title || t.target);
	return `${t.fixRound && !t.run ? `Fix round ${t.fixRound} for ` : ''}${link}${title ? ` — ${title}` : ''}`;
}

/** What kind of run this is, in words, on its own line. */
const nowRun = (t: NowTask) => {
	const label = runLabel(t.run, t.runRound ?? t.fixRound);
	return label ? `<span class="run" data-run="${esc(t.run)}">${esc(label)}</span>` : '';
};

const nowMeta = (t: NowTask, extra: string[] = []) => [t.task ? esc(t.task) : '', t.target || t.pr ? esc(t.type) : '', t.size ? sizeLabel[t.size] : '', ...extra].filter(Boolean);

/** What the queue is a mix of: "8 new tasks, 4 reruns, 1 internal review". */
function queueMix(active: Active[]) {
	const kinds: [string, (run: string) => boolean, string, string][] = [
		['new', (r) => r === 'new', 'new task', 'new tasks'],
		['rerun', (r) => r.startsWith('rerun'), 'rerun', 'reruns'],
		['fix', (r) => r.startsWith('fix'), 'fix round', 'fix rounds'],
		['review', (r) => r === 'review', 'internal review', 'internal reviews'],
		['followup', (r) => r === 'followup', 'follow-up round', 'follow-up rounds'],
	];
	const queued = active.filter((a) => a.state === 'queued');
	const parts = kinds.flatMap(([, is, one, many]) => {
		const n = queued.filter((a) => is(a.run)).length;
		return n ? [plural(n, one, many)] : [];
	});
	return parts.length ? `<p class="meta" data-now-mix>In the queue: ${parts.join(', ')}.</p>` : '';
}

/** "Working on now" (live data only): the running tasks, the first queued ones and how many are queued. */
export function nowSection(now: Now | null | undefined, at: Date) {
	if (now === undefined) return '';
	let body: string;
	if (now === null) body = '<p class="meta" data-now-none>No report from the runner in the last hour.</p>';
	else {
		const items = now.running
			.map((r) => {
				const meta = nowMeta(r, [r.state === 'running' ? 'running' : 'interrupted, resumes automatically']);
				return `<li data-now-task data-state="${r.state}"><span class="target">${nowTitle(r)}</span>${nowRun(r)}<span class="meta">${meta.join(' · ')}</span></li>`;
			})
			.join('');
		const queue = now.queue
			.map((q) => {
				const meta = nowMeta(q);
				return `<li data-queued${q.pr ? ` data-pr="${q.pr.number}"` : ''}><span class="target">${nowTitle(q)}</span>${nowRun(q)}${
					meta.length ? `<span class="meta">${meta.join(' · ')}</span>` : ''
				}</li>`;
			})
			.join('');
		const more = now.queued - now.queue.length;
		const queued = queue
			? more > 0
				? `+${plural(more, 'more task', 'more tasks')} queued.`
				: ''
			: now.queued > 0
				? `${plural(now.queued, 'more task', 'more tasks')} queued.`
				: now.running.length
					? 'Nothing else queued.'
					: '';
		body = `${items ? `<ul>${items}</ul>` : '<p data-now-idle>Nothing is running right now.</p>'}${
			queue ? `<h3>Queued next</h3>${queueMix(now.active)}<ol class="sci-queue" data-now-queue>${queue}</ol>` : ''
		}<p class="meta" data-now-queued="${now.queued}">${queued}${queued ? ' ' : ''}Reported <span data-ago="${now.reportedAt.toISOString()}">${ago(
			now.reportedAt,
			at,
		)}</span>.</p>`;
	}
	return `<section class="sci-now not-content" data-science-now><h2 id="working-on-now">Working on now</h2>${body}</section>`;
}

export function howToRead(s: Science) {
	const attempts =
		s.attempts !== null && s.attemptsPassing !== null && s.attempts > 0
			? ` <span data-science-attempts>So far ${s.attempts} attempts, of which ${s.attemptsPassing} (${pct(s.attemptsPassing, s.attempts)})
passed every local check (build and lints) before anything was sent.</span>`
			: '';
	const gpu = s.attemptsWithoutGpuTime > 0 ? ` (the time of ${plural(s.attemptsWithoutGpuTime, 'early attempt')} ${s.attemptsWithoutGpuTime === 1 ? 'was' : 'were'} not recorded)` : '';
	const tok = `${s.qwenTokens !== null ? `: ${compact(s.qwenTokens)} in all` : ''}${s.qwenOutputTokens !== null ? `, of which ${compact(s.qwenOutputTokens)} written` : ''}${
		s.attemptsWithoutTokens > 0 ? ` (the tokens of ${plural(s.attemptsWithoutTokens, 'early attempt')} were not recorded)` : ''
	}`;
	const json = s.livePath;
	return `<h2 id="how-to-read-this">How to read this</h2>
<ul>
<li><strong>Tau Ceti’s review is the judge.</strong> A pull request counts as merged only when Tau Ceti merged it (its merge bot closes a
pull request rather than merging it on GitHub, so we read the bot’s own title and comment). We do not grade our own work.</li>
<li><strong>Everything counts.</strong> Closed pull requests stay in the table, failed attempts count in their task’s row and in the
totals, and tasks whose attempts all failed before a pull request have rows of their own.${attempts}</li>
<li><strong>Review rounds</strong> are Tau Ceti’s review of each commit: the review criteria (rubrics) that asked for changes, each
with Tau Ceti’s own one-line reason, quoted verbatim and linked to its comment. <strong>Fix rounds</strong> answer Tau Ceti’s
review or its CI, <strong>internal rounds</strong> our own review, before or after a pull request is opened; a round’s retries count
once. <strong>Approved at the first review</strong> means every one of Tau Ceti’s review criteria passed on the first try; pull requests
not reviewed yet are left out of that count.</li>
<li><strong>GPU hours</strong> are the time the model was working, summed over every attempt, failures included${gpu}.
<strong>Qwen tokens</strong> are everything the model read and wrote${tok}. Most of what it reads is the same files again, served from a cache.</li>
<li><strong>These are our own measurements,</strong> from the records of each run, not estimates. The model runs on the AIs@home
network, which today is mostly the maintainer’s own GPUs, some of them rented.</li>
<li>${s.asOf ? `Pull request states as of <time datetime="${isoDay(s.asOf)}" data-science-asof>${dayLabel(s.asOf)}</time>. ` : ''}The data is
rebuilt once a day and published as JSON: <a href="${json}" data-science-json><code>${json}</code></a>.</li>
</ul>`;
}

/**
 * Tau Ceti's own progress as a whole, in one line with its date (the runner reads it daily from
 * Tau Ceti's public GitHub data; science-model.ts Progress). Counts only, no judgement; nothing
 * when the data has none.
 */
export function progress(s: Science) {
	const p = s.progress;
	if (!p) return '';
	const count = (n: number, one: string, many = `${one}s`) => `${exact(n)} ${n === 1 ? one : many}`;
	const parts: string[] = [];
	if (p.mergedPRs !== null && p.mergedPRAuthors !== null)
		parts.push(
			`<span data-progress-merged="${p.mergedPRs}">${count(p.mergedPRs, 'pull request')} merged in the last ${count(p.windowDays, 'day')}</span>, by <span data-progress-authors="${p.mergedPRAuthors}">${count(p.mergedPRAuthors, 'GitHub account')}</span>`,
		);
	if (p.roadmapsActive !== null && p.roadmapsCompleted !== null)
		parts.push(
			`<span data-progress-roadmaps="${p.roadmapsActive}" data-progress-completed="${p.roadmapsCompleted}">${count(p.roadmapsActive, 'active roadmap')}, and ${exact(p.roadmapsCompleted)} declared complete by its maintainers</span>`,
		);
	return `<p class="sci-progress" data-science-progress>Tau Ceti as a whole, as of <time datetime="${isoDay(p.asOf)}" data-progress-asof>${dayLabel(
		p.asOf,
	)}</time>: ${parts.join('; ')}.</p>`;
}

/** The "Updated N h ago" line: the build shows the date; the browser turns it into "N h ago". */
export function updated(s: Science) {
	if (!s.generatedAt) return '';
	return `<p class="sci-updated" data-science-updated>Updated <time datetime="${s.generatedAt.toISOString()}" data-ago="${s.generatedAt.toISOString()}">on ${dayLabel(
		s.generatedAt,
	)}</time></p>`;
}
