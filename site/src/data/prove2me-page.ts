// The build's data for the prove2me page (/science/prove2me/), its record page (/science/prove2me/record/) and the
// data the page's opened sections are drawn from (/science/prove2me/record.json): computed once per build, from the
// frozen history (science/prove2me.json) and the gateway's records the build fetched (prove2me-build.ts).
//
// 2026-11-28 (lazy sections): the page carried every trial's list, every leaf, every task and every leaf's outlines
// and tasks in its HTML, 1.9 MB of a 2.4 MB page, though all of them start closed. The page now draws them in the
// browser when they are opened, from the gateway's records it reads anyway, with the build's copy in record.json for
// the frozen trials, the build's task rows and when the gateway does not answer. The record page draws them in full,
// with the same renderers, for readers without JavaScript and for search engines.
//
// SPDX-License-Identifier: Apache-2.0
import { counts, outcomeLabel, p2m, plainTitle, sumTokens, theoremURL, tokTitle, tokensWithCache, computedTokens } from './prove2me';
import { FROZEN_THROUGH, taskTableRows, type TasksView } from './prove2me-current';
import { allRuns, builtDetail, builtTaskRows, historyOf, leavesView, scienceView, trialDetails, type TrialDetail } from './prove2me-records';
import { mergeRows, type TaskTableRow } from './task-table';
import { statusView, type StatusView } from './prove2me-status';
import { nowAll, type NowCtx } from './prove2me-now';
import { buildRecords } from './prove2me-build';
import type { FrozenTrial } from './prove2me-view';
import type { LeafInfo } from './prove2me-tasks';

const safe = (u: string) => (theoremURL.test(u) ? u : undefined);

async function compute() {
	// The gateway's records as the build fetched them (or its committed copy when the gateway did not answer the build).
	const { records: rec, from: recFrom } = await buildRecords();
	// A leaf closed by reuse: the proved theorem its one-line proof imports (the leaf's line says so).
	const reuseOf: Record<string, string> = Object.fromEntries(p2m.submissions.filter((s) => s.kind === 'reuse' && s.reuses).map((s) => [s.theorem, s.reuses!.theorem]));
	// For "Working on now": the build's theorem pages (a leaf a document lists without one) and its reuses.
	const known = {
		reuses: reuseOf,
		theorems: Object.fromEntries(p2m.trials.flatMap((tr) => tr.leaves).filter((r) => safe(r.url)).map((r) => [r.theorem, r.url])),
		// each leaf's prove2me title by id, for its row's short title (the campaigns' items over the build's leaves)
		titles: Object.fromEntries([
			...p2m.leaves.map((l) => [l.id, l.title]),
			...rec.campaigns.flatMap((c) => c.items).filter((i) => i.title).map((i) => [i.id, i.title]),
		]) as Record<string, string>,
	};
	const trials = p2m.trials.map((tr) => ({ ...tr, c: counts(tr.leaves), hours: tr.leaves.reduce((a, r) => a + r.model_minutes, 0) / 60, tok: sumTokens(tr.leaves), v5: tr.leaves.some((r) => r.tasks) }));
	// The page's numbers, submissions, trials and leaves (prove2me-records.ts): the build's history with the records the
	// build fetched; the browser redoes them with the records it reads and the same renderers (prove2me-view.ts).
	const hist = historyOf(p2m);
	const view = scienceView(hist, rec);
	const lv = leavesView(hist, rec);
	// The status block's build copy: the queue, the open benchmarks and trials and the submissions from the records; the
	// browser adds the network and refreshes the rest.
	const st: StatusView = { ...statusView(rec, null)!, source: 'build', saved: recFrom === 'snapshot' };
	const frozenTrials = trials.filter((tr) => tr.trial <= FROZEN_THROUGH);
	const builtDetails: TrialDetail[] = trialDetails(
		p2m.trials.filter((tr) => tr.trial > FROZEN_THROUGH).map((tr) => builtDetail(p2m, hist, tr.trial)).filter((d): d is TrialDetail => !!d),
		rec,
		hist,
	);
	// "Working on now": every open benchmark and every trial not finished, from the records (prove2me-now.ts).
	const runs = allRuns(hist, rec);
	const earlier = (leaf: string, n: number) => [...new Set(runs.filter((r) => r.leaf === leaf && r.trial < n).map((r) => r.trial))].sort((a, b) => a - b);
	const nowDocs = nowAll(null, rec);
	const nowCtx: NowCtx = { theorems: known.theorems, reuses: reuseOf, titles: known.titles, earlier };
	// The texts name F01's trial-5 run; its task count comes from the data.
	const f01 = p2m.trials.flatMap((tr) => tr.leaves).find((r) => r.leaf === 'F01' && r.outcome === 'proved' && r.tasks);
	// A leaf's id (p2m- and the start of prove2me's theorem id) leads everywhere; the trials' alias (O04, F01) is shown small.
	const probeLeaves = (p2m.probes ?? []).flatMap((r) => r.leaves);
	const idOfTheorem = (theorem: string) => p2m.leaves.find((l) => l.theorem === theorem)?.id ?? probeLeaves.find((l) => l.theorem === theorem)?.id;
	const idOfAlias = (alias: string) => p2m.leaves.find((l) => l.aliases.includes(alias))?.id ?? alias;
	// Alias -> leaf id for the history the "Small numbers" text names (O04, F01); the browser's script gets it too.
	const aliasIds: Record<string, string> = Object.fromEntries(['O04', 'F01'].map((a) => [a, idOfAlias(a)]));
	// Trials 1 to 5 as their lists show them (prove2me-view.ts frozenListHTML).
	const frozen: FrozenTrial[] = frozenTrials.map((tr) => ({
		trial: tr.trial,
		summary: tr.summary,
		caps: tr.caps,
		v5: tr.v5,
		leaves: tr.leaves.map((r) => ({
			leaf: r.leaf,
			id: idOfTheorem(r.theorem) ?? null,
			theorem: r.theorem,
			url: safe(r.url) ?? '',
			title: plainTitle(r.title),
			group: r.group,
			earlier: r.earlier,
			outcome: r.outcome,
			scored: r.scored,
			result: outcomeLabel(r),
			why: r.not_scored_why ?? '',
			rounds: r.rounds,
			minutes: r.model_minutes,
			computed: computedTokens(r),
			withCache: tokensWithCache(r),
			tokTitle: tokTitle(r),
			tasks: r.tasks ?? null,
			outlines: r.outlines ?? [],
			...(reuseOf[r.theorem] ? { reuse: reuseOf[r.theorem] } : {}),
		})),
	}));
	// Every task: the generic task table with prove2me's options; every task of trial 6 on (and of the outline probes) from
	// the records, the build's and then the browser's. Each leaf's theorem and page for its group cell (the
	// trials', the campaigns' and the probe rounds' leaves; the browser adds the data document's), and a written outline's
	// hole count by task id for the live rows.
	const items = rec.campaigns.flatMap((c) => c.items).filter((i) => i.labels.theorem);
	const leafIds: Record<string, string> = Object.fromEntries([
		...items.map((i) => [i.labels.theorem, i.id]),
		...probeLeaves.map((l) => [l.theorem, l.id]),
		...p2m.leaves.map((l) => [l.theorem, l.id]),
	]);
	const taskLeaves: LeafInfo = Object.fromEntries([
		...items.map((i) => [i.id, { theorem: i.labels.theorem, url: safe(i.url) ?? '' }]),
		...probeLeaves.map((l) => [l.id, { theorem: l.theorem, url: safe(l.url) ?? '' }]),
		...p2m.leaves.map((l) => [l.id, { theorem: l.theorem, url: safe(l.url) ?? '' }]),
	]);
	const taskHoles: Record<string, string> = Object.fromEntries(p2m.tasks.filter((r) => r.labels.holes).map((r) => [r.id, r.labels.holes]));
	// the build's rows with tokens computed (the export's hold the cache re-reads), the records' rows over them
	const builtRows: TaskTableRow[] = mergeRows(builtTaskRows(p2m), taskTableRows(rec.tasks, null, leafIds, taskHoles));
	// What the browser's script needs to redo the numbers, submissions, trials, leaves and the status block with the
	// gateway's records; the build's status stands in while the gateway does not answer.
	const recordsKnown = { history: hist, model: p2m.model, f01Tasks: f01?.tasks?.length ?? null, aliasIds, status: st };
	// "Working on now"'s tasks as the build had them: those of the blocks it shows (a leaf's row draws its outlines and
	// tasks from them when it opens before the gateway answers, or when it does not).
	const nowTrials = new Set(nowDocs.map((d) => d.trial.id));
	const nowTasks: TasksView = { updated: rec.tasks.updated, tasks: rec.tasks.tasks.filter((t) => nowTrials.has(t.labels.trial)) };
	/** record.json: what the page's sections that open draw from, as the build had it. */
	const record = {
		updated: rec.updated,
		/** Every task: the build's rows (the frozen trials' and the records'), and what the gateway's rows need: each leaf's
		 * theorem and page, leaf ids by theorem, a written outline's hole count. */
		tasks: { rows: builtRows, leafIds, leaves: taskLeaves, holes: taskHoles },
		/** The trials' lists: trials 1 to 5, and the build's from trial 6 on. */
		frozen,
		details: builtDetails,
		/** Every leaf, as the build counted them. */
		leaves: lv,
		/** "Working on now" as the build had it. */
		now: { docs: nowDocs, tasks: nowTasks },
	};
	// What the page's script needs inline for "Working on now" every minute: the theorem pages and reuses, and the titles
	// the gateway's campaigns do not give it (the frozen trials' leaves) or the build's blocks show.
	const campaignItems = new Set(rec.campaigns.flatMap((c) => c.items.map((i) => i.id)));
	const shown = new Set(nowDocs.flatMap((d) => d.leaves.map((l) => l.id)));
	const knownInline = { ...known, titles: Object.fromEntries(Object.entries(known.titles).filter(([id]) => !campaignItems.has(id) || shown.has(id))) };
	return {
		knownInline, rec, recFrom, reuseOf, known, trials, hist, view, lv, st, frozenTrials, frozen, builtDetails, runs, earlier, nowDocs, nowCtx, f01,
		idOfTheorem, idOfAlias, aliasIds, leafIds, taskLeaves, taskHoles, builtRows, recordsKnown, record,
	};
}

let once: ReturnType<typeof compute> | null = null;
/** The build's prove2me data, computed once per build (the page, the record page and record.json read the same). */
export const prove2mePage = () => (once ??= compute());
export type Prove2mePage = Awaited<ReturnType<typeof compute>>;
export type Prove2meRecord = Prove2mePage['record'];
