// A table of a science project's tasks (2026-11-15, first used by the prove2me page): one row per task, its relations
// to other tasks as links, filters (one value per filter), a sort by column (▲/▼ and aria-sort on that column only) and
// "Showing N of M". The row data has generic field names so another project's tasks fit the same table; what is
// project-specific (its label columns, how a task is named in a relation, the labels of kinds and outcomes) comes in
// TaskTableOptions. The same code renders the build's HTML and, in the browser, the live rows added later.
//
// SPDX-License-Identifier: Apache-2.0

export type TaskState = 'queued' | 'running' | 'done' | 'failed' | 'cancelled';

export interface TaskTableRow {
	project: string;
	id: string;
	kind: string;
	state: TaskState | string;
	outcome: string | null;
	try: number;
	/** The task this one works for (prove2me: a hole's outline). */
	parent: string | null;
	/** The tasks this one waits for (prove2me: an assembly's holes). */
	needs: string[];
	/** The try before this one. */
	retry_of: string | null;
	/** The task this one replaces (prove2me: the outline before). */
	replaces: string | null;
	steps: number | null;
	model_min: number | null;
	/** Everything the model read (cache re-reads included) and wrote; null: unknown. */
	tokens: number | null;
	started_at: string | null;
	finished_at: string | null;
	/** The project's own fields, as text (prove2me: trial, leaf, alias, outline, hole). */
	labels: Record<string, string>;
}

export interface TaskTableOptions {
	/** The table's name (its data attribute), e.g. the project. */
	name: string;
	/** Project columns after the task id: a label (`labels[key]`), its heading, and optionally its cell's HTML. */
	labelCols: { key: string; title: string; html?: (r: TaskTableRow) => string }[];
	/** Filters in order: a generic field (kind, state, outcome) or `label:<key>`, the "all" chip and the values. */
	filters: { key: string; all: string; values: [string, string][] }[];
	kindLabel: Record<string, string>;
	outcomeLabel: (o: string | null, r: TaskTableRow) => string;
	/** How another task is named in a relation ("outline 2", "hole 3"); its first word groups a list ("holes 1, 2"). */
	describe: (id: string, byId: Map<string, TaskTableRow>) => string;
}

export const taskStates: [TaskState, string][] = [
	['queued', 'Queued'],
	['running', 'Running'],
	['done', 'Done'],
	['failed', 'Failed'],
	['cancelled', 'Cancelled'],
];
const stateText = Object.fromEntries(taskStates) as Record<string, string>;

export type TaskSortKey = 'tokens' | 'minutes' | 'finished';
export type TaskSort = { key: TaskSortKey; dir: 'asc' | 'desc' };

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const exact = (v: number) => new Intl.NumberFormat('en').format(v);
const compactNum = (v: number) => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v)));
const mon = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
/** "2026-11-13T16:07:00Z" -> "13 Nov 16:07". */
export const shortTime = (iso: string) => {
	const d = new Date(iso);
	return `${d.getUTCDate()} ${mon[d.getUTCMonth()]} ${String(d.getUTCHours()).padStart(2, '0')}:${String(d.getUTCMinutes()).padStart(2, '0')}`;
};
/** The anchor of a task's row. */
export const taskAnchor = (name: string, id: string) => `${name}-task-${id}`;

/** The relations of a row as HTML: links to the rows they name (needs grouped by the first word: "needs holes 1, 2"). */
export function relationsHTML(r: TaskTableRow, o: TaskTableOptions, byId: Map<string, TaskTableRow>): string {
	const link = (id: string, text = o.describe(id, byId)) =>
		byId.has(id) ? `<a href="#${esc(taskAnchor(o.name, id))}" data-rel-to="${esc(id)}">${esc(text)}</a>` : esc(text);
	const out: string[] = [];
	if (r.parent) out.push(`<span data-rel="parent">from ${link(r.parent)}</span>`);
	if (r.retry_of) out.push(`<span data-rel="retry_of">retry of ${link(r.retry_of)}</span>`);
	if (r.replaces) out.push(`<span data-rel="replaces">replaces ${link(r.replaces)}</span>`);
	if (r.needs.length) {
		const groups = new Map<string, string[]>();
		for (const id of r.needs) {
			const d = o.describe(id, byId);
			const [noun, ...rest] = d.split(' ');
			const g = rest.length ? noun : '';
			groups.set(g, [...(groups.get(g) ?? []), rest.length ? link(id, rest.join(' ')) : link(id, d)]);
		}
		const parts = [...groups].map(([noun, ls]) => (noun ? `${noun}${ls.length > 1 ? 's' : ''} ${ls.join(', ')}` : ls.join(', ')));
		out.push(`<span data-rel="needs">needs ${parts.join('; ')}</span>`);
	}
	return out.length ? `<span class="tt-rel">${out.join('')}</span>` : '';
}

const num = (v: number | null, f: (v: number) => string = String) => (v === null ? '–' : f(v));

/** One row's HTML. `order` is its place before any sort. */
export function taskRowHTML(r: TaskTableRow, o: TaskTableOptions, byId: Map<string, TaskTableRow>, order: number, live = false): string {
	const attrs = [
		`id="${esc(taskAnchor(o.name, r.id))}"`,
		`data-task-id="${esc(r.id)}"`,
		`data-kind="${esc(r.kind)}"`,
		`data-state="${esc(r.state)}"`,
		`data-outcome="${esc(r.outcome ?? '')}"`,
		`data-order="${order}"`,
		...Object.entries(r.labels).map(([k, v]) => `data-label-${esc(k)}="${esc(v)}"`),
		r.tokens !== null ? `data-sort-tokens="${r.tokens}"` : '',
		r.model_min !== null ? `data-sort-minutes="${r.model_min}"` : '',
		r.finished_at ? `data-sort-finished="${Date.parse(r.finished_at)}"` : '',
		live ? 'data-source="live"' : '',
	].filter(Boolean);
	const labelCells = o.labelCols
		.map((c) => `<td data-label="${esc(c.title)}" data-cell="${esc(c.key)}">${c.html ? c.html(r) : esc(r.labels[c.key] ?? '–')}</td>`)
		.join('');
	const times = [r.started_at ? `<time datetime="${esc(r.started_at)}" data-started>${shortTime(r.started_at)}</time>` : '', r.finished_at ? `<time datetime="${esc(r.finished_at)}" data-finished>${shortTime(r.finished_at)}</time>` : ''];
	return `<tr ${attrs.join(' ')}>
<th scope="row"><span class="tt-id">${esc(r.id)}</span>${relationsHTML(r, o, byId)}</th>${labelCells}<td data-label="Kind" data-cell="kind">${esc(o.kindLabel[r.kind] ?? r.kind)}</td><td data-label="Try" class="num" data-cell="try">${r.try}</td><td data-label="State" data-cell="state" class="tt-state tt-${esc(r.state)}">${esc(stateText[r.state] ?? r.state)}</td><td data-label="Outcome" data-cell="outcome">${esc(o.outcomeLabel(r.outcome, r))}</td><td data-label="Steps" class="num" data-cell="steps">${num(r.steps)}</td><td data-label="Model min" class="num" data-cell="minutes">${num(r.model_min, (v) => v.toFixed(1))}</td><td data-label="Qwen tokens" class="num" data-cell="tokens"${r.tokens !== null ? ` title="${exact(r.tokens)} Qwen tokens"` : ''}>${num(r.tokens, compactNum)}</td><td data-label="Started / finished" class="tt-times" data-cell="times">${times[0] || '–'}${times[1] ? `<span class="tt-sub">${times[1]}</span>` : ''}</td>
</tr>`;
}

const sortable: [TaskSortKey, string][] = [
	['minutes', 'Model min'],
	['tokens', 'Qwen tokens'],
	['finished', 'Started / finished'],
];

/** The whole block: filters, the count and the table (rows in the given order). */
export function taskTableHTML(rows: TaskTableRow[], o: TaskTableOptions): string {
	const byId = new Map(rows.map((r) => [r.id, r]));
	const chip = (key: string, value: string, text: string, on: boolean) =>
		`<button type="button" data-tt-filter="${esc(key)}" data-value="${esc(value)}" aria-pressed="${on}">${esc(text)}</button>`;
	const filters = o.filters
		.map((f) => [chip(f.key, '', f.all, true), ...f.values.map(([v, t]) => chip(f.key, v, t, false))].join(''))
		.join('<span class="sep"></span>');
	const head = (title: string, cls = '') => {
		const s = sortable.find(([, t]) => t === title);
		return s
			? `<th scope="col"${cls ? ` class="${cls}"` : ''} data-sort-col="${s[0]}"><button type="button" data-tt-sort="${s[0]}">${esc(title)}<span class="sort-mark" aria-hidden="true"></span></button></th>`
			: `<th scope="col"${cls ? ` class="${cls}"` : ''}>${esc(title)}</th>`;
	};
	return `<div class="tt-filters" role="group" aria-label="Filter the tasks" data-tt-filters>${filters}</div>
<p class="tt-count" data-tt-count>Showing ${rows.length} of ${rows.length}</p>
<div class="tt-wrap"><table class="tt-table" data-tt-table="${esc(o.name)}">
<thead><tr>${head('Task')}${o.labelCols.map((c) => head(c.title)).join('')}${head('Kind')}${head('Try', 'num')}${head('State')}${head('Outcome')}${head('Steps', 'num')}${head('Model min', 'num')}${head('Qwen tokens', 'num')}${head('Started / finished')}</tr></thead>
<tbody>${rows.map((r, i) => taskRowHTML(r, o, byId, i)).join('')}</tbody>
</table></div>`;
}

/** Tokens, minutes and finished go most or newest first; a row with none stays last; ties keep their order. */
export function compareTasks(a: HTMLElement, b: HTMLElement, s: TaskSort): number {
	const key = `sort${s.key[0].toUpperCase()}${s.key.slice(1)}`;
	const v = (r: HTMLElement) => (r.dataset[key] === undefined ? null : Number(r.dataset[key]));
	const x = v(a), y = v(b);
	const order = Number(a.dataset.order) - Number(b.dataset.order);
	if (x === null || y === null) return x === y ? order : x === null ? 1 : -1;
	return (s.dir === 'asc' ? x - y : y - x) || order;
}

/** In the browser: wire a block made by taskTableHTML (filters, sort, count) and return how to add live rows. */
export function attachTaskTable(box: HTMLElement, o: TaskTableOptions, rows: TaskTableRow[]) {
	const filter: Record<string, string> = Object.fromEntries(o.filters.map((f) => [f.key, '']));
	let sort: TaskSort | null = null;
	const body = () => box.querySelector<HTMLElement>('[data-tt-table] tbody');
	const apply = () => {
		const b = body();
		if (!b) return;
		box.querySelectorAll<HTMLElement>('th[data-sort-col]').forEach((th) => {
			const on = !!sort && th.dataset.sortCol === sort.key;
			if (on) th.setAttribute('aria-sort', sort!.dir === 'asc' ? 'ascending' : 'descending');
			else th.removeAttribute('aria-sort');
			const m = th.querySelector('.sort-mark');
			if (m) m.textContent = on ? (sort!.dir === 'asc' ? '▲' : '▼') : '';
		});
		// a build's row a live row replaces stays out of sight and out of the count
		b.querySelectorAll<HTMLElement>(':scope > tr[data-replaced]').forEach((tr) => (tr.hidden = true));
		const trs = [...b.querySelectorAll<HTMLElement>(':scope > tr:not([data-replaced])')];
		const sorted = [...trs].sort((x, y) => (sort ? compareTasks(x, y, sort) : Number(x.dataset.order) - Number(y.dataset.order)));
		if (sorted.some((r, i) => r !== trs[i])) sorted.forEach((r) => b.appendChild(r));
		box.querySelectorAll<HTMLButtonElement>('[data-tt-filter]').forEach((btn) => btn.setAttribute('aria-pressed', String(filter[btn.dataset.ttFilter!] === (btn.dataset.value ?? ''))));
		let shown = 0;
		for (const tr of trs) {
			tr.hidden = Object.entries(filter).some(([k, v]) => {
				if (!v) return false;
				const have = k.startsWith('label:') ? tr.getAttribute(`data-label-${k.slice(6)}`) : tr.getAttribute(`data-${k}`);
				return have !== v;
			});
			if (!tr.hidden) shown++;
		}
		const c = box.querySelector('[data-tt-count]');
		if (c) c.textContent = `Showing ${shown} of ${trs.length}`;
	};
	box.addEventListener('click', (e) => {
		const t = e.target as HTMLElement;
		const s = t.closest<HTMLElement>('[data-tt-sort]');
		if (s) {
			const key = s.dataset.ttSort as TaskSortKey;
			sort = { key, dir: sort?.key === key ? (sort.dir === 'asc' ? 'desc' : 'asc') : 'desc' };
			apply();
			return;
		}
		const f = t.closest<HTMLElement>('[data-tt-filter]');
		if (f) {
			filter[f.dataset.ttFilter!] = f.dataset.value ?? '';
			apply();
			return;
		}
		// a relation's link shows its row even when a filter hides it
		const rel = t.closest<HTMLElement>('[data-rel-to]');
		const target = rel && box.querySelector<HTMLElement>(`tr[data-task-id="${CSS.escape(rel.dataset.relTo!)}"]`);
		if (target?.hidden) {
			for (const k of Object.keys(filter)) filter[k] = '';
			apply();
		}
	});
	apply();
	return {
		/** Replace the live rows (the build's rows with the same id give way to them); they go after the build's rows. */
		setLive(live: TaskTableRow[]) {
			const b = body();
			if (!b) return;
			b.querySelectorAll('tr[data-source="live"]').forEach((tr) => tr.remove());
			const ids = new Set(live.map((r) => r.id));
			// the build's rows and the live ones, for the relations' names
			const byId = new Map([...rows.filter((r) => !ids.has(r.id)), ...live].map((r) => [r.id, r]));
			b.querySelectorAll<HTMLElement>('tr[data-task-id]').forEach((tr) => tr.toggleAttribute('data-replaced', ids.has(tr.dataset.taskId!)));
			b.insertAdjacentHTML('beforeend', live.map((r, i) => taskRowHTML(r, o, byId, rows.length + i, true)).join(''));
			apply();
		},
	};
}
