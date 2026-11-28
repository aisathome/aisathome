// A table of a science project's tasks (2026-11-15, first used by the prove2me page; simplified the same
// day): five columns (the group, usually a leaf, shown once per group; the task as a readable label; its result in
// one plain phrase; its work, steps · minutes · tokens; when it finished), rows grouped as the project orders them, two
// drop-down filters and a four-way "Show" switch (all, proved, not proved, running), and "Showing N of M". The row data
// has generic field names so another project's tasks fit the same table; what is project-specific (the group, the
// label, the result phrase, the order, the filters) comes in TaskTableOptions. The same code renders the build's HTML
// and, in the browser, the table again with the live rows merged in.
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
	/** What closed the task without a model attempt, when something did (prove2me: reuse:..., an outline task). */
	closed_by?: string | null;
	steps: number | null;
	model_min: number | null;
	/** Tokens computed: what the model read without the cache, plus what it wrote; null: unknown. */
	tokens: number | null;
	/** With the cache re-reads (shown on hover only); null or absent: unknown. */
	tokens_with_cache?: number | null;
	started_at: string | null;
	finished_at: string | null;
	/** The project's own fields, as text (prove2me: trial, leaf, alias, outline, hole, holes). */
	labels: Record<string, string>;
}

/** Which of the "Show" switch's sets a row is in. */
export type TaskShow = 'proved' | 'not_proved' | 'running';
export const taskShows: [TaskShow | '', string][] = [
	['', 'All'],
	['proved', 'Proved'],
	['not_proved', 'Not proved'],
	['running', 'Running'],
];

export interface TaskTableOptions {
	/** The table's name (its data attribute and its rows' anchors), e.g. the project. */
	name: string;
	/** The rows in the table's order (grouped: a group's rows next to each other). */
	sort: (rows: TaskTableRow[]) => TaskTableRow[];
	/** A row's group (its key) and the group cell's HTML, shown on the first row of a group that is in sight. */
	group: (r: TaskTableRow) => { key: string; html: string };
	/** The task in words ("Hole 4 (outline 2) · try 2") and, for a retry or a replacement only, a note as HTML. */
	label: (r: TaskTableRow, byId: Map<string, TaskTableRow>, link: (id: string, text: string) => string) => { text: string; noteHTML?: string };
	/** The result in one plain phrase, and which "Show" set it is in. */
	result: (r: TaskTableRow, byId: Map<string, TaskTableRow>) => { text: string; show: TaskShow };
	/** Drop-down filters on a label (`labels[key]`): the heading, the "all" entry, the values in order and the default. */
	selects: { key: string; title: string; all: string; values: (rows: TaskTableRow[]) => [string, string][]; initial?: (rows: TaskTableRow[]) => string }[];
}

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
const exact = (v: number) => new Intl.NumberFormat('en').format(v);
export const compactNum = (v: number) => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${Math.round(v / 1e3)}k` : String(Math.round(v)));
const mon = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
/** "2026-11-13T16:07:00Z" -> "13 Nov 16:07". */
export const shortTime = (iso: string) => {
	const d = new Date(iso);
	return `${d.getUTCDate()} ${mon[d.getUTCMonth()]} ${String(d.getUTCHours()).padStart(2, '0')}:${String(d.getUTCMinutes()).padStart(2, '0')}`;
};
/** The anchor of a task's row. */
export const taskAnchor = (name: string, id: string) => `${name}-task-${id}`;

/** A task's work in one line: "47 steps · 44 min · 2.4M"; "–" when there was none. */
export function workText(r: Pick<TaskTableRow, 'steps' | 'model_min' | 'tokens'>): string {
	const parts = [
		r.steps ? `${r.steps} step${r.steps === 1 ? '' : 's'}` : '',
		r.model_min ? `${r.model_min >= 10 ? Math.round(r.model_min) : r.model_min.toFixed(1)} min` : '',
		r.tokens ? compactNum(r.tokens) : '',
	].filter(Boolean);
	return parts.length ? parts.join(' · ') : '–';
}

/** One row's HTML. `first`: the first row of its group (the group cell shows). */
export function taskRowHTML(r: TaskTableRow, o: TaskTableOptions, byId: Map<string, TaskTableRow>, first: boolean, live = false): string {
	const link = (id: string, text: string) => (byId.has(id) ? `<a href="#${esc(taskAnchor(o.name, id))}" data-rel-to="${esc(id)}">${esc(text)}</a>` : esc(text));
	const g = o.group(r);
	const lab = o.label(r, byId, link);
	const res = o.result(r, byId);
	const attrs = [
		`id="${esc(taskAnchor(o.name, r.id))}"`,
		`data-task-id="${esc(r.id)}"`,
		`data-kind="${esc(r.kind)}"`,
		`data-state="${esc(r.state)}"`,
		`data-outcome="${esc(r.outcome ?? '')}"`,
		`data-show="${res.show}"`,
		`data-group="${esc(g.key)}"`,
		...Object.entries(r.labels).map(([k, v]) => `data-label-${esc(k)}="${esc(v)}"`),
		// the relations, for links and checks; the label names a retry or a replacement, the grouping the rest
		r.parent ? `data-parent="${esc(r.parent)}"` : '',
		r.needs.length ? `data-needs="${esc(r.needs.join(' '))}"` : '',
		r.retry_of ? `data-retry-of="${esc(r.retry_of)}"` : '',
		r.replaces ? `data-replaces="${esc(r.replaces)}"` : '',
		r.tokens !== null ? `data-tokens="${r.tokens}"` : '',
		first ? 'data-first' : '',
		live ? 'data-source="live"' : '',
	].filter(Boolean);
	const running = r.state === 'running' || r.state === 'queued';
	const fin = r.finished_at
		? `<time datetime="${esc(r.finished_at)}" data-finished${r.started_at ? ` title="started ${esc(shortTime(r.started_at))} UTC"` : ''}>${shortTime(r.finished_at)}</time>`
		: running && r.started_at
			? `running since <time datetime="${esc(r.started_at)}" data-started>${shortTime(r.started_at)}</time>`
			: '–';
	const work = workText(r);
	return `<tr ${attrs.join(' ')}>
<td class="tt-group" data-cell="group">${g.html}</td><th scope="row" data-cell="task"><span class="tt-task" title="${esc(r.id)}">${esc(lab.text)}</span>${lab.noteHTML ? `<span class="tt-note">${lab.noteHTML}</span>` : ''}</th><td data-cell="result" class="tt-r tt-r-${res.show}">${esc(res.text)}</td><td data-cell="work" class="tt-work"${r.tokens !== null ? ` title="${exact(r.tokens)} tokens computed${r.tokens_with_cache ? `; ${exact(r.tokens_with_cache)} with cache re-reads` : ''}"` : ''}>${esc(work)}</td><td data-cell="finished" class="tt-time">${fin}</td>
</tr>`;
}

/** The body's rows: in the options' order, the first row of each group marked. */
function bodyHTML(rows: TaskTableRow[], o: TaskTableOptions, liveIds: Set<string> = new Set()): string {
	const sorted = o.sort(rows);
	const byId = new Map(rows.map((r) => [r.id, r]));
	let prev = '';
	return sorted
		.map((r) => {
			const k = o.group(r).key;
			const html = taskRowHTML(r, o, byId, k !== prev, liveIds.has(r.id));
			prev = k;
			return html;
		})
		.join('');
}

const selectHTML = (s: TaskTableOptions['selects'][number], rows: TaskTableRow[]) =>
	`<label class="tt-select"><span>${esc(s.title)}</span><select data-tt-filter="label:${esc(s.key)}"><option value="" selected>${esc(s.all)}</option>${s
		.values(rows)
		.map(([v, t]) => `<option value="${esc(v)}">${esc(t)}</option>`)
		.join('')}</select></label>`;

/** The whole block: filters, the count and the table. */
export function taskTableHTML(rows: TaskTableRow[], o: TaskTableOptions): string {
	const show = taskShows.map(([v, t]) => `<button type="button" data-tt-show="${v}" aria-pressed="${v === ''}">${esc(t)}</button>`).join('');
	return `<div class="tt-filters" role="group" aria-label="Filter the tasks" data-tt-filters>${o.selects.map((s) => selectHTML(s, rows)).join('')}<div class="tt-show" role="group" aria-label="Show" data-tt-shows>${show}</div></div>
<p class="tt-count" data-tt-count>Showing ${rows.length} of ${rows.length}</p>
<div class="tt-wrap"><table class="tt-table" data-tt-table="${esc(o.name)}">
<thead><tr><th scope="col">Leaf</th><th scope="col">Task</th><th scope="col">Result</th><th scope="col">Work</th><th scope="col">Finished</th></tr></thead>
<tbody>${bodyHTML(rows, o)}</tbody>
</table></div>`;
}

/** The build's rows with the live rows merged in: the build's keep their places, a live row takes the place of the
 * build's row with its id, and a task only the live rows have comes after them; one row per id (the first). */
export function mergeRows(rows: TaskTableRow[], live: TaskTableRow[]): TaskTableRow[] {
	const byLive = new Map<string, TaskTableRow>();
	for (const r of live) if (!byLive.has(r.id)) byLive.set(r.id, r);
	const out: TaskTableRow[] = [];
	const ids = new Set<string>();
	for (const r of [...rows.map((r) => byLive.get(r.id) ?? r), ...byLive.values()]) {
		if (ids.has(r.id)) continue;
		ids.add(r.id);
		out.push(r);
	}
	return out;
}

/** In the browser: wire a block made by taskTableHTML (filters, the switch, the count) and return how to add live rows. */
export function attachTaskTable(box: HTMLElement, o: TaskTableOptions, rows: TaskTableRow[]) {
	let all = rows;
	const filter: Record<string, string> = Object.fromEntries(o.selects.map((s) => [`label:${s.key}`, '']));
	// a drop-down the reader has not touched follows its default as live rows arrive (a new trial)
	const touched = new Set<string>();
	let show = '';
	const body = () => box.querySelector<HTMLElement>('[data-tt-table] tbody');
	const fillSelects = () => {
		for (const s of o.selects) {
			const key = `label:${s.key}`;
			if (!touched.has(key) && s.initial) filter[key] = s.initial(all);
			const el = box.querySelector<HTMLSelectElement>(`select[data-tt-filter="${CSS.escape(key)}"]`);
			const values = s.values(all);
			if (filter[key] && !values.some(([v]) => v === filter[key])) filter[key] = '';
			if (!el) continue;
			el.replaceChildren(
				...[['', s.all] as [string, string], ...values].map(([v, t]) => {
					const opt = document.createElement('option');
					opt.value = v;
					opt.textContent = t;
					return opt;
				}),
			);
			el.value = filter[key];
		}
	};
	const apply = () => {
		const b = body();
		if (!b) return;
		box.querySelectorAll<HTMLButtonElement>('[data-tt-show]').forEach((btn) => btn.setAttribute('aria-pressed', String((btn.dataset.ttShow ?? '') === show)));
		const trs = [...b.querySelectorAll<HTMLElement>(':scope > tr')];
		let shown = 0;
		let prev: string | null = null;
		for (const tr of trs) {
			tr.hidden = (!!show && tr.dataset.show !== show) || Object.entries(filter).some(([k, v]) => !!v && tr.getAttribute(`data-label-${k.slice(6)}`) !== v);
			if (tr.hidden) continue;
			shown++;
			// the group cell shows on the first row in sight of its group
			tr.toggleAttribute('data-first', tr.dataset.group !== prev);
			prev = tr.dataset.group ?? '';
		}
		const c = box.querySelector('[data-tt-count]');
		if (c) c.textContent = `Showing ${shown} of ${trs.length}`;
	};
	box.addEventListener('change', (e) => {
		const t = e.target as HTMLSelectElement;
		if (!t.matches?.('select[data-tt-filter]')) return;
		filter[t.dataset.ttFilter!] = t.value;
		touched.add(t.dataset.ttFilter!);
		apply();
	});
	box.addEventListener('click', (e) => {
		const t = e.target as HTMLElement;
		const s = t.closest<HTMLElement>('[data-tt-show]');
		if (s) {
			show = s.dataset.ttShow ?? '';
			apply();
			return;
		}
		// a relation's link shows its row even when a filter hides it
		const rel = t.closest<HTMLElement>('[data-rel-to]');
		const target = rel && box.querySelector<HTMLElement>(`tr[data-task-id="${CSS.escape(rel.dataset.relTo!)}"]`);
		if (target?.hidden) {
			for (const k of Object.keys(filter)) {
				filter[k] = '';
				touched.add(k);
			}
			show = '';
			box.querySelectorAll<HTMLSelectElement>('select[data-tt-filter]').forEach((el) => (el.value = ''));
			apply();
		}
	});
	fillSelects();
	apply();
	return {
		/** The row of task `anchor` (its element id), shown even when a filter hides it: the filters are cleared then, as
		 * a relation's link clears them. Null when the table has no such row. */
		reveal(anchor: string): HTMLElement | null {
			const row = box.querySelector<HTMLElement>(`tr[id="${CSS.escape(anchor)}"]`);
			if (!row) return null;
			if (row.hidden) {
				for (const k of Object.keys(filter)) {
					filter[k] = '';
					touched.add(k);
				}
				show = '';
				box.querySelectorAll<HTMLSelectElement>('select[data-tt-filter]').forEach((el) => (el.value = ''));
				apply();
			}
			return row;
		},
		/** Merge the live rows in (a build's row with the same id gives way to its live row) and draw the table again. */
		setLive(live: TaskTableRow[]) {
			const b = body();
			if (!b) return;
			all = mergeRows(rows, live);
			b.innerHTML = bodyHTML(all, o, new Set(live.map((r) => r.id)));
			fillSelects();
			apply();
		},
	};
}
