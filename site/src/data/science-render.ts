// The data part of /science/tauceti/ as HTML, from normalized data (science.ts). The build renders
// it from the committed JSON (the page works without JavaScript); in the browser the page renders
// it again from the gateway's daily data when that is newer, so both use this one function.
// Sections whose numbers are missing are left out rather than shown as dashes or zeros.
import { ago, compact, dayLabel, exact, isoDay, shortDay, type Group, type Now, type NowTask, type PR, type Science, type Task } from './science-model';

const esc = (s: unknown) =>
	String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
/** Tau Ceti's text, verbatim: escaped, with `code` spans as code. */
const verbatim = (s: string) => esc(s).replace(/`([^`]+)`/g, '<code>$1</code>');
const attrs = (a: Record<string, string | number>) =>
	Object.entries(a)
		.map(([k, v]) => (v === '' ? ` ${k}` : ` ${k}="${esc(v)}"`))
		.join('');
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const pct = (a: number, b: number) => `${Math.round((100 * a) / b)}%`;
const sizeLabel = { easy: 'easy', medium: 'medium', hard: 'hard' };

function tiles(s: Science) {
	if (!s.prs.length) return '';
	const list = [
		{ key: 'opened', value: String(s.prs.length), label: 'PRs opened' },
		{ key: 'merged', value: String(s.merged), label: 'Merged by Tau Ceti' },
		{ key: 'closed', value: String(s.closed), label: 'Closed without merging' },
		{ key: 'first-review', value: s.firstReviewed ? `${s.firstReviewApproved} of ${s.firstReviewed}` : null, label: 'Approved at the first review' },
		{ key: 'gpu-hours', value: s.gpuHours !== null ? s.gpuHours.toFixed(1) : null, label: 'GPU hours' },
		{
			key: 'tokens-per-merged',
			value: s.qwenTokens !== null && s.merged > 0 ? compact(s.qwenTokens / s.merged) : null,
			label: 'Qwen tokens per merged PR',
		},
	].filter((t) => t.value !== null);
	return `<dl class="sci-tiles not-content" data-science-totals>${list
		.map((t) => `<div data-total="${t.key}"><dt>${esc(t.label)}</dt><dd>${esc(t.value)}</dd></div>`)
		.join('')}</dl>`;
}

// Merged PRs over time: a cumulative step line from the first PR to the snapshot date.
function chart(s: Science) {
	const day = 86400000;
	const merges = s.prs.flatMap((p) => (p.merged ? [p] : [])).sort((a, b) => +a.merged! - +b.merged!);
	if (!merges.length) return '';
	const W = 640, H = 220, L = 40, R = 16, T = 16, B = 34;
	const start = Math.floor(Math.min(...s.prs.map((p) => +p.opened)) / day) * day;
	const last = Math.max(+merges[merges.length - 1].merged!, s.asOf ? +s.asOf + day : 0);
	const end = Math.max(Math.ceil(last / day) * day, start + day);
	const top = Math.max(merges.length, 2);
	const x = (t: number) => L + ((t - start) / (end - start)) * (W - L - R);
	const y = (v: number) => T + (1 - v / top) * (H - T - B);
	let d = `M${x(start).toFixed(1)},${y(0).toFixed(1)}`;
	const points = merges.map((p, i) => {
		const px = x(+p.merged!);
		d += ` H${px.toFixed(1)} V${y(i + 1).toFixed(1)}`;
		return { x: px, y: y(i + 1), label: `#${p.number} merged ${dayLabel(p.merged!)} (${i + 1} in total)` };
	});
	d += ` H${x(end).toFixed(1)}`;
	const step = Math.max(1, Math.ceil(top / 4));
	const yTicks = Array.from({ length: Math.floor(top / step) + 1 }, (_, i) => ({ v: i * step, y: y(i * step) }));
	const days = Math.round((end - start) / day);
	const every = Math.max(1, Math.ceil(days / 6));
	const count = Math.floor(days / every) + 1;
	const xTicks = Array.from({ length: count }, (_, i) => {
		const t = start + i * every * day;
		return { x: x(t), label: shortDay(new Date(t)), anchor: i === 0 ? 'start' : i === count - 1 ? 'end' : 'middle', minor: i % 2 === 1 };
	});
	const desc = `${plural(s.merged, 'pull request')} merged into Tau Ceti: ${merges.map((p) => `#${p.number} on ${dayLabel(p.merged!)}`).join(', ')}.`;
	return `<figure class="sci-chart not-content" data-science-chart>
<figcaption>Merged PRs over time (cumulative)</figcaption>
<svg viewBox="0 0 ${W} ${H}" role="img" aria-labelledby="sci-chart-desc"><desc id="sci-chart-desc">${esc(desc)}</desc>
<g class="grid">${yTicks.map((t) => `<line x1="${L}" x2="${W - R}" y1="${t.y}" y2="${t.y}"></line><text x="${L - 8}" y="${t.y + 4}" text-anchor="end">${t.v}</text>`).join('')}${xTicks
		.map((t) => `<text x="${t.x}" y="${H - 10}" text-anchor="${t.anchor}"${t.minor ? ' class="minor"' : ''}>${esc(t.label)}</text>`)
		.join('')}</g>
<path class="line" d="${d}"></path>
${points.map((p) => `<g class="pt" tabindex="0" data-merge-point><title>${esc(p.label)}</title><circle class="hit" cx="${p.x}" cy="${p.y}" r="14"></circle><circle class="dot" cx="${p.x}" cy="${p.y}" r="5"></circle></g>`).join('')}
</svg></figure>`;
}

const notRecorded = (n: number) => (n > 0 ? `; ${plural(n, 'attempt')} not recorded` : '');
function tokCell(v: number | null, out: number | null, missing: number) {
	const a = v === null ? { 'data-unknown': '' } : { 'data-tokens': v, title: `${exact(v)} Qwen tokens${out !== null ? `, ${exact(out)} of them written` : ''}${notRecorded(missing)}` };
	const more = v !== null && missing > 0 ? `<span class="meta">+${missing} not recorded</span>` : '';
	return `<td class="num tok" data-label="Qwen tokens"${attrs(a)}>${v === null ? 'not recorded' : compact(v)}${more}</td>`;
}
function minCell(v: number | null, missing: number) {
	const a = v === null ? { 'data-unknown': '' } : { 'data-minutes': v, title: `${v.toFixed(1)} GPU minutes${notRecorded(missing)}` };
	const more = v !== null && missing > 0 ? `<span class="meta">+${missing} not recorded</span>` : '';
	return `<td class="num min" data-label="GPU minutes"${attrs(a)}>${v === null ? 'not recorded' : v.toFixed(1)}${more}</td>`;
}

// Each review round: the commit Tau Ceti reviewed and, for every rubric that asked for changes,
// Tau Ceti's own one-line reason, verbatim, linked to its comment.
function rounds(p: PR) {
	if (!p.rounds.length) return p.state === 'open' ? '<span class="meta" data-no-review>No review yet</span>' : '';
	const items = p.rounds
		.map((r) => {
			const changes = r.changes
				.map(
					(c) =>
						`<li data-change data-rubric="${esc(c.rubric)}"><span class="rubric">${esc(c.rubric)}</span> ${
							c.reason ? `<q data-reason>${verbatim(c.reason)}</q> ` : ''
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

const status = (p: PR) => (p.state === 'merged' ? `Merged ${shortDay(p.merged!)}` : p.state === 'closed' ? 'Closed, not merged' : 'Open, in review');

// The first review: approved, changes asked, or not reviewed yet.
const firstReview = (p: PR) =>
	p.firstReviewApproved === null
		? ['pending', 'First review pending']
		: p.firstReviewApproved
			? ['approved', 'Approved at first review']
			: ['changes', 'Changes asked at first review'];

function prRow(p: PR) {
	const [first, firstText] = firstReview(p);
	const meta = [`opened <time datetime="${isoDay(p.opened)}">${shortDay(p.opened)}</time>`];
	if (p.size) meta.push(sizeLabel[p.size]);
	if (p.attempts !== null) meta.push(plural(p.attempts, 'attempt'));
	return `<tr data-pr="${p.number}" data-state="${p.state}">
<th scope="row"><a href="${esc(p.url)}">#${p.number}</a></th>
<td class="title">${esc(p.title)}<span class="meta">${meta.join(' · ')}</span>${rounds(p)}</td>
<td class="status"><span class="state state-${p.state}">${status(p)}</span><span class="meta"><span class="first" data-first-review="${first}">${firstText}</span><span class="fix" data-fix-rounds="${p.fixRounds}">${plural(
		p.fixRounds,
		'fix round',
	)}</span></span></td>
${tokCell(p.qwenTokens, p.qwenOutputTokens, p.attemptsWithoutTokens)}${minCell(p.gpuMinutes, p.attemptsWithoutGpuTime)}
</tr>`;
}

function taskRow(t: Task) {
	const meta = [];
	if (t.size) meta.push(sizeLabel[t.size]);
	if (t.attempts !== null) meta.push(plural(t.attempts, 'attempt'));
	if (t.lastAttempt) meta.push(`last <time datetime="${isoDay(t.lastAttempt)}">${shortDay(t.lastAttempt)}</time>`);
	const [state, sub] = t.state === 'failed' ? ['Failed local checks', 'No pull request sent'] : ['Passed local checks', 'Not sent yet'];
	return `<tr data-no-pr data-state="${t.state}">
<th scope="row"><span class="no-pr">No PR</span></th>
<td class="title" data-target>${esc(t.target)}<span class="meta">${meta.join(' · ')}</span></td>
<td class="status"><span class="state state-${t.state}">${state}</span><span class="meta"><span>${sub}</span></span></td>
${tokCell(t.qwenTokens, t.qwenOutputTokens, t.attemptsWithoutTokens)}${minCell(t.gpuMinutes, t.attemptsWithoutGpuTime)}
</tr>`;
}

function group(g: Group) {
	const sub = [plural(g.prs.length, 'PR'), `${g.merged} merged`];
	if (g.tasks.length) sub.push(`${g.tasks.length} without a PR`);
	return `<tbody data-group="${esc(g.type)}">
<tr class="group-head" data-group-head><th scope="rowgroup" colspan="3">${esc(g.type)}<span class="group-sub" data-group-sub>${sub.join(' · ')}</span></th>
${tokCell(g.qwenTokens, null, g.attemptsWithoutTokens)}${minCell(g.gpuMinutes, g.attemptsWithoutGpuTime)}</tr>
${g.prs.map(prRow).join('')}${g.tasks.map(taskRow).join('')}
</tbody>`;
}

function table(s: Science) {
	if (!s.prs.length && !s.tasks.length) return '';
	return `<h2 id="every-pr">Every pull request</h2>
<p>Successes and failures alike, grouped by type of task, oldest first within each group. Each links to the pull request on
GitHub, where Tau Ceti’s review is public; open a pull request’s review rounds to see what the review asked for. Tasks whose
attempts have not led to a pull request are listed too, marked “No PR”. A fix round is a revision pushed after review asked
for changes.</p>
<div class="sci-wrap not-content"><table class="sci-table" data-science-prs>
<thead><tr><th scope="col">PR</th><th scope="col">What it does</th><th scope="col">Status</th><th scope="col" class="num">Qwen tokens</th><th scope="col" class="num">GPU minutes</th></tr></thead>
${s.groups.map(group).join('')}
</table></div>
<p class="sci-note">Tokens and minutes cover every attempt at a task, failed attempts and fix rounds included. Most prompt
tokens are the same files re-read from the cache; hover a token count for the exact number and how many tokens the model wrote.</p>`;
}

/** A running or queued task's title: a fix round names its pull request, linked ("Fix round 4 for #19781 — title"). */
function nowTitle(t: NowTask) {
	if (!t.pr) return esc(t.target || t.type);
	const link = `<a href="${esc(t.pr.url)}" data-now-pr="${t.pr.number}">#${t.pr.number}</a>`;
	const title = esc(t.pr.title || t.target);
	return `${t.fixRound ? `Fix round ${t.fixRound} for ` : ''}${link}${title ? ` — ${title}` : ''}`;
}

/** "Working on now" (live data only): the running tasks, the first queued ones and how many are queued. */
export function nowSection(now: Now | null | undefined, at: Date) {
	if (now === undefined) return '';
	let body: string;
	if (now === null) body = '<p class="meta" data-now-none>No report from the runner in the last hour.</p>';
	else {
		const items = now.running
			.map((r) => {
				const meta = [esc(r.type), r.size ? sizeLabel[r.size] : '', r.state === 'running' ? 'running' : 'interrupted, resumes automatically'].filter(Boolean);
				return `<li data-now-task data-state="${r.state}"><span class="target">${nowTitle(r)}</span><span class="meta">${meta.join(' · ')}</span></li>`;
			})
			.join('');
		const queue = now.queue
			.map((q) => {
				const meta = [q.target || q.pr ? esc(q.type) : '', q.size ? sizeLabel[q.size] : ''].filter(Boolean);
				return `<li data-queued${q.pr ? ` data-pr="${q.pr.number}"` : ''}><span class="target">${nowTitle(q)}</span>${
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
			queue ? `<h3>Queued next</h3><ol class="sci-queue" data-now-queue>${queue}</ol>` : ''
		}<p class="meta" data-now-queued="${now.queued}">${queued}${queued ? ' ' : ''}Reported <span data-ago="${now.reportedAt.toISOString()}">${ago(
			now.reportedAt,
			at,
		)}</span>.</p>`;
	}
	return `<section class="sci-now not-content" data-science-now><h2 id="working-on-now">Working on now</h2>${body}</section>`;
}

function howToRead(s: Science) {
	const attempts =
		s.attempts !== null && s.attemptsPassing !== null && s.attempts > 0
			? ` <span data-science-attempts>So far ${s.attempts} attempts, of which ${s.attemptsPassing} (${pct(s.attemptsPassing, s.attempts)})
passed every local check (build and lints) before anything was sent.</span>`
			: '';
	const gpu = s.attemptsWithoutGpuTime > 0 ? ` (the time of ${plural(s.attemptsWithoutGpuTime, 'early attempt')} ${s.attemptsWithoutGpuTime === 1 ? 'was' : 'were'} not recorded)` : '';
	const tok = `${s.qwenTokens !== null ? `: ${compact(s.qwenTokens)} in all` : ''}${s.qwenOutputTokens !== null ? `, of which ${compact(s.qwenOutputTokens)} written` : ''}${
		s.attemptsWithoutTokens > 0 ? ` (the tokens of ${plural(s.attemptsWithoutTokens, 'early attempt')} were not recorded)` : ''
	}`;
	const json = s.now === undefined ? s.jsonPath : s.livePath; // the live data once the browser has it
	return `<h2 id="how-to-read-this">How to read this</h2>
<ul>
<li><strong>Tau Ceti’s review is the judge.</strong> A pull request counts as merged only when Tau Ceti merged it. We do not grade our own work.</li>
<li><strong>Everything counts.</strong> Closed pull requests stay in the table, failed attempts count in their task’s row and in the
totals, and tasks whose attempts all failed before a pull request have rows of their own.${attempts}</li>
<li><strong>Review rounds</strong> are Tau Ceti’s review of each commit: the review criteria (rubrics) that asked for changes, each
with Tau Ceti’s own one-line reason, quoted verbatim and linked to its comment. <strong>Fix rounds</strong> are revisions pushed
after review asked for changes. <strong>Approved at the first review</strong> means every one of Tau Ceti’s review criteria passed on the first try; pull requests
not reviewed yet are left out of that count.</li>
<li><strong>GPU hours</strong> are the time the model was working, summed over every attempt, failures included${gpu}.
<strong>Qwen tokens</strong> are everything the model read and wrote${tok}. Most of what it reads is the same files again, served from a cache.</li>
<li><strong>These are our own measurements,</strong> from the records of each run, not estimates. The model runs on the AIs@home
network, which today is mostly the maintainer’s own GPUs, some of them rented.</li>
<li>${s.asOf ? `Pull request states as of <time datetime="${isoDay(s.asOf)}" data-science-asof>${dayLabel(s.asOf)}</time>. ` : ''}The data is
rebuilt once a day and published as JSON: <a href="${json}" data-science-json><code>${json}</code></a>.</li>
</ul>`;
}

/** The "Updated N h ago" line: the build shows the date; the browser turns it into "N h ago". */
function updated(s: Science) {
	if (!s.generatedAt) return '';
	return `<p class="sci-updated" data-science-updated>Updated <time datetime="${s.generatedAt.toISOString()}" data-ago="${s.generatedAt.toISOString()}">on ${dayLabel(
		s.generatedAt,
	)}</time></p>`;
}

/** Everything after the introduction. */
export function renderScience(s: Science, at: Date) {
	return [updated(s), tiles(s), chart(s), nowSection(s.now, at), table(s), howToRead(s)].filter(Boolean).join('\n');
}
