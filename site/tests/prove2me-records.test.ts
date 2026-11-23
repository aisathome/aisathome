// The prove2me page's trials, submissions and headline numbers from the gateway's records (step D6), on fixed data:
// the build's committed history (science/prove2me.json) and the public records as the gateway served them on
// 2026-11-18 (tests/fixtures: campaigns t4 to t8 with the probe rounds, 8 accepted results, 154 tasks). The expected
// numbers were counted from the same files by hand (a run is a leaf of a trial with a task that started; a leaf
// closed by reuse is no run; trials 1 to 5 are the build's).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import { parseCurrent, type CurrentDoc, type PublicTask } from '../src/data/prove2me-current';
import {
	builtDetail, builtTaskRows, docFromCampaign, fetchRecords, historyOf, liveSummary, nowDocs, parseCampaigns, parseResults, scienceView, trialDetails,
	type Campaign, type Records,
} from '../src/data/prove2me-records';
import { cardNumbers, homeLine, kpisHTML, newestFirst, plainHTML, smallNumbersHTML, submissionRowHTML, submissionRowsHTML, trialDetailHTML, trialFacts, trialRowHTML, trialsFootHTML } from '../src/data/prove2me-view';

const p2m = raw as unknown as Prove2me;
const h = historyOf(p2m);
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: fixture.tasks.tasks as unknown as PublicTask[] },
});
const row = (v: ReturnType<typeof scienceView>, label: string) => v.trials.find((t) => t.label === label)!;

test('without the gateway the numbers are the build’s', () => {
	const v = scienceView(h, null);
	const t = p2m.totals;
	assert.equal(v.source, 'build');
	assert.deepEqual(
		[v.kpis.runs, v.kpis.proved, v.kpis.openTried, v.kpis.openProved, v.kpis.accepted, v.kpis.submitted, v.kpis.reused, v.kpis.calibrationRuns, v.kpis.calibrationProved],
		[t.runs, t.proved, t.open_leaves, t.open_leaves_proved, t.accepted, t.submitted, t.reused, t.calibration_runs, t.calibration_proved],
	);
	assert.equal(v.kpis.hours.toFixed(1), t.model_hours.toFixed(1));
	assert.deepEqual(v.trials.map((x) => x.label), ['1', '2', '3', '4', '5', '6p', '6', '7p', '7']);
	assert.ok(v.trials.every((x) => x.source === 'build'));
	assert.deepEqual(v.current, { number: 8, status: 'running', start: '2026-11-16', scaffold: 'v9' });
});

test('with the gateway: trials 6 on from its records, 1 to 5 the build’s, every submission', () => {
	const v = scienceView(h, records());
	assert.equal(v.source, 'live');
	// trials 1-5: 42 runs, 10 proved (the build's); t6 2 of 2, t7 3 of 4, t8 2 of 6 so far
	assert.deepEqual([v.kpis.runs, v.kpis.proved], [54, 17]);
	assert.deepEqual([v.kpis.calibrationRuns, v.kpis.calibrationProved], [16, 11]);
	assert.deepEqual([v.kpis.openTried, v.kpis.openProved], [26, 6]);
	assert.deepEqual(v.kpis.openProvedLeaves.map((l) => l.leaf).sort(), ['p2m-527634', 'p2m-7f145a', 'p2m-a146ce', 'p2m-aec6a2', 'p2m-d00cfc', 'p2m-dbd69f']);
	assert.deepEqual([v.kpis.accepted, v.kpis.submitted, v.kpis.reused], [8, 8, 2]);
	assert.equal(v.kpis.lastTrial, 8);
	assert.deepEqual(v.trials.map((x) => `${x.label}:${x.source}`), ['1:build', '2:build', '3:build', '4:build', '5:build', '6p:live', '6:live', '7p:live', '7:live', '8:live']);
	const t7 = row(v, '7');
	assert.deepEqual([t7.all, t7.calibration, t7.open], [{ proved: 3, runs: 4 }, { proved: 1, runs: 1 }, { proved: 2, runs: 3 }]);
	assert.deepEqual([t7.start, t7.end, t7.scaffold, t7.status], ['2026-11-15', '2026-11-17', 'v8', 'finished']);
	// tokens computed lead (the gateway's tokens_computed, summed); t6 and t8 have a started task without its counts
	assert.equal(t7.computed, 201793 + 138170 + 270465 + 5776280);
	assert.ok(t7.withCache > 10 * t7.computed);
	assert.equal(t7.exact, true);
	assert.equal(row(v, '6').exact, false);
	assert.equal(Math.round(row(v, '6').model_min * 10) / 10, 8.3 + 165.6);
	const t8 = row(v, '8');
	assert.deepEqual([t8.status, t8.end, t8.all.proved, t8.all.runs], ['running', null, 2, 6]);
	assert.equal(row(v, '7p').probe, true);
	assert.equal(Math.round(row(v, '7p').model_min * 10) / 10, 59.4);
	assert.equal(row(v, '7p').computed, 251366);
	assert.deepEqual(v.current, { number: 8, status: 'running', start: '2026-11-16', scaffold: 'v9' });
	// the submissions: the build's seven and trial 8's from the gateway, in order; O04 (trial 3, no campaign) keeps its
	// theorem and title from the build, a reuse its author
	assert.deepEqual(v.submissions.map((s) => s.alias), ['O04', 'F01', 'N02', 'N05', 'N03', 'N10', 'N07', 'N12']);
	const o04 = v.submissions[0];
	assert.deepEqual([o04.leaf, o04.trial, o04.kind, o04.theorem], ['p2m-7f145a', 3, 'proof', 'SeqPostedPricing.MatroidSPM.accept_prob_eq_quantile']);
	assert.match(o04.title, /accepts with probability/);
	const n05 = v.submissions.find((s) => s.alias === 'N05')!;
	assert.deepEqual([n05.kind, n05.found, n05.trial, n05.reuses?.theorem, n05.reuses?.author, n05.reuses?.relation], ['reuse', 'trial', 6, 'NestedSeatAlloc.IntPolicy.eq27_er1_clbi', 'mikedeng1', 'part']);
	const n02 = v.submissions.find((s) => s.alias === 'N02')!;
	assert.deepEqual([n02.found, n02.trial], ['probes', 6]);
	const n12 = v.submissions.at(-1)!;
	assert.deepEqual([n12.leaf, n12.trial, n12.theorem, n12.verdict], ['p2m-527634', 8, 'DistLTLSynth.Char.lemma_2_3', 'accepted']);
	assert.match(n12.title, /prefix/);
});

test('a later trial the gateway has no campaign of keeps the build’s row; a frozen trial never takes the gateway’s', () => {
	const r = records();
	r.campaigns = r.campaigns.filter((c) => c.id !== 't7');
	const v = scienceView(h, r);
	assert.equal(row(v, '7').source, 'build');
	assert.equal(row(v, '6').source, 'live');
	assert.equal(row(v, '5').source, 'build');
	assert.equal(v.kpis.runs, 54, 'trial 7 counted once, from the build');
	// no campaign at all: the build's numbers, the gateway's submissions
	const none = scienceView(h, { ...r, campaigns: [] });
	assert.equal(none.kpis.runs, p2m.totals.runs);
	assert.equal(none.kpis.accepted, 8);
	assert.deepEqual(none.current, h.current);
});

test('the headline numbers, the submissions and the trials table as HTML: escaped, tokens computed first', () => {
	const v = scienceView(h, records());
	const k = kpisHTML(v.kpis);
	assert.equal(k.n, 4);
	assert.match(k.html, /<div data-kpi="accepted"><dt>proofs accepted by prove2me, of 8 submitted<\/dt><dd>8<\/dd><\/div>/);
	assert.match(k.html, /<dt>open leaves proved, of 26 tried<\/dt><dd>6<\/dd>/);
	assert.match(k.html, /<dt>runs proved, of 54 in trials 1 to 8<\/dt><dd>17<\/dd>/);
	const small = smallNumbersHTML(v, 21, (a) => ({ O04: 'p2m-7f145a', F01: 'p2m-a146ce' })[a] ?? a);
	assert.match(small, /^Small numbers: 6 open leaves proved so far\. That shows the model can do it, not yet how often\./);
	assert.match(small, /11 of 16 runs succeeded/);
	const t8row = trialRowHTML(row(v, '8'));
	assert.match(t8row, /^<tr data-trial="8" data-source="live" data-status="running">/);
	assert.match(t8row, /<a href="#trial-8">Trial 8<\/a><span class="sub">running<\/span>/);
	assert.match(t8row, /data-cell="proved">2 of 6</);
	assert.match(t8row, /data-label="Tokens computed" class="num tok" data-tokens="\d+" data-with-cache="\d+" title="At least: [\d,]+ tokens computed; [\d,]+ with the cache re-reads">≥ /);
	assert.match(trialRowHTML(row(v, '7p')), /<span>Trial 7 probes<\/span>.*data-cell="proved">–</s);
	const foot = trialsFootHTML(v);
	assert.match(foot, /data-cell="proved">17 of 54</);
	assert.match(foot, /data-cell="calibration">11 of 16</);
	const sub = submissionRowHTML({ ...v.submissions.at(-1)!, title: 'A <b>title</b> & $x$' });
	assert.match(sub, /^<tr data-submission="N12" data-leaf-id="p2m-527634" data-verdict="accepted" data-kind="proof">/);
	assert.match(sub, /<span class="sub">A &lt;b&gt;title&lt;\/b&gt; &amp; x<\/span>/);
	assert.match(sub, /<td data-label="From">Trial 8<\/td>/);
	assert.match(submissionRowHTML(v.submissions.find((s) => s.alias === 'N05')!), /part of <a href="https:\/\/prove2\.me\/theorems\/21543cdb[^"]*">NestedSeatAlloc\.IntPolicy\.eq27_er1_clbi<\/a> by mikedeng1<\/span><\/td><td data-label="From">Closed by reuse<span class="sub">found during trial 6<\/span>/);
});

test('a trial’s list: generated facts, the campaign’s own text as plain text, each leaf’s outcome', () => {
	const r = records();
	const t7c = r.campaigns.find((c) => c.id === 't7')!;
	r.campaigns = r.campaigns.map((c) => (c.id === 't7' ? { ...c, text: `${t7c.text}\n\nClosing: <script>x</script> & [a link](https://example.org) *not em*` } : c));
	const ds = trialDetails([], r, h);
	assert.deepEqual(ds.map((d) => d.sum.number), [6, 7, 8]);
	const d7 = ds[1];
	assert.equal(trialFacts(d7), 'Trial 7 ran from 15 to 17 November 2026 on scaffold v8: 4 leaves (1 calibration, 3 open), 3 of 4 runs proved; prove2me accepted 2 proofs from it.');
	assert.equal(trialFacts(ds[2]), 'Trial 8 has run since 16 November 2026 on scaffold v9: 6 leaves (1 calibration, 5 open), so far 2 of 6 runs proved; prove2me accepted 1 proof from it.');
	assert.match(trialFacts(ds[0]), /^Trial 6 ran on 15 November 2026 on scaffold v7: 2 leaves \(1 calibration, 1 open\), 2 of 2 runs proved; prove2me accepted 1 proof from it and 1 leaf closed by reuse\.$/);
	const html = trialDetailHTML(d7);
	assert.match(html, /^<details class="p2m-trial" id="trial-7" data-p2m-trial="7" data-source="live"><summary>Trial 7 · 15 to 17 November 2026 · 3 of 4 proved<\/summary>/);
	assert.match(html, /<div data-trial-text><p>Scaffold v8: v7 with what trial 6 taught\./);
	assert.match(html, /<p>Closing: &lt;script&gt;x&lt;\/script&gt; &amp; \[a link\]\(https:\/\/example\.org\) \*not em\*<\/p><\/div>/);
	assert.doesNotMatch(html, /<script>|<a href="https:\/\/example/);
	assert.match(html, /<p class="caps">Budget per leaf: one round of 110 steps/);
	assert.match(html, /<tr data-leaf="N10" data-leaf-id="p2m-aec6a2" data-outcome="proved">/);
	assert.match(html, /<tr data-leaf="N08" data-leaf-id="p2m-115f6c" data-outcome="not_proved">/);
	assert.match(html, /<tr data-leaf="N07" [^>]*>.*?data-cell="result">Proved with outline 3<span class="sub" data-leaf-line>Proved: outline 3 assembled<\/span>/s);
	assert.match(html, /data-trial-leaf="C07".*data-leaf-plan="C07"/s);
	assert.match(plainHTML('a\nb\n\n<i>c</i>'), /^<p>a b<\/p><p>&lt;i&gt;c&lt;\/i&gt;<\/p>$/);
	// trial 6's N05 (closed by reuse in the trial) is no run and not in the trial's list
	assert.ok(!ds[0].leaves.some((l) => l.id === 'p2m-f181df' && !l.reuse));
	// trial 8's p2m-03d4b4: its outline 1's cancelled tries say why (the wording fix, through the trial's list)
	const n13 = ds[2].leaves.find((l) => l.id === 'p2m-03d4b4')!;
	assert.match(trialDetailHTML(ds[2]), /try 1: a sorry left; try 2 cancelled: outline abandoned after hole 1 failed/);
	assert.doesNotMatch(trialDetailHTML(ds[2]), /closed by another task/);
	assert.ok(n13.detail.rows.length > 5);
	// the build's list of a trial it has (trial 7) and of one it lacks (none)
	const b7 = builtDetail(p2m, h, 7)!;
	assert.equal(b7.sum.source, 'build');
	assert.equal(b7.leaves.length, 4);
	assert.equal(builtDetail(p2m, h, 99), null);
	assert.deepEqual(trialDetails([b7], null, h), [b7]);
});

test('"Working on now": every trial not finished, oldest first; the data document for its own trial or a newer one', () => {
	const r = records(); // t8 runs, every other trial campaign has finished
	const doc = (n: number, status = 'running') =>
		parseCurrent({
			schema: 'prove2me-current/2', project: 'prove2me', updated_at: '2026-11-17T15:47:29Z',
			trial: { id: `t${n}`, number: n, scaffold: 'v9', start: '2026-11-17', status, description: '', budget: '' }, leaves: [], submissions: [],
		});
	const ids = (ds: CurrentDoc[]) => ds.map((d) => `${d.trial.id}:${d.schema}`);
	assert.deepEqual(ids(nowDocs(doc(8), r)), ['t8:prove2me-current/2'], 'the document stands for its own trial');
	assert.deepEqual(ids(nowDocs(doc(9), r)), ['t8:campaign', 't9:prove2me-current/2'], 'a newer document joins the open campaign');
	assert.deepEqual(ids(nowDocs(doc(9, 'finished'), r)), ['t8:campaign']);
	const [fromCampaign, ...rest] = nowDocs(doc(7), r);
	assert.equal(rest.length, 0, 'an older document is left out');
	assert.equal(fromCampaign.schema, 'campaign');
	assert.deepEqual(fromCampaign.trial, { id: 't8', number: 8, scaffold: 'v9', start: '2026-11-16', status: 'running', description: '', budget: r.campaigns[0].labels.budget });
	assert.equal(fromCampaign.leaves.length, 6);
	assert.deepEqual(fromCampaign.leaves[0], { id: 'p2m-3ad60f', alias: 'C07', role: 'calibration', theorem: 'WilliamsonShmoys.planar_drawing_maximal_supergraph__p2m_97829890_428d_4657_a899_f7e200a68a7c_a4', url: 'https://prove2.me/theorems/3ad60fc3-1bed-4725-aaf6-264d650d407f' });
	assert.deepEqual(fromCampaign.submissions.map((s) => [s.leaf, s.verdict]), [['p2m-527634', 'accepted']]);
	assert.deepEqual(ids(nowDocs(null, r)), ['t8:campaign']);
	assert.deepEqual(nowDocs(null, null), []);
	assert.deepEqual(ids(nowDocs(doc(8), null)), ['t8:prove2me-current/2'], 'without the campaigns, the document alone');

	// Bug (owner, 2026-11-20): trial 10 was posted while trial 9 still ran; only trial 10 was shown. Both now, oldest first,
	// each with its own text and budget; the stale document of trial 8 (no longer written) stays out.
	const t8 = r.campaigns.find((c) => c.id === 't8')!;
	const later = (n: number, status: Campaign['status'], text: string) => ({ ...t8, id: `t${n}`, title: `Trial ${n}`, status, text, labels: { ...t8.labels, budget: `budget ${n}` } });
	const two = { ...r, campaigns: [later(10, 'running', 'Trial ten.'), later(9, 'running', 'Trial nine.'), { ...t8, status: 'finished' as const }, ...r.campaigns.filter((c) => c.id !== 't8')] };
	const both = nowDocs(doc(8), two);
	assert.deepEqual(ids(both), ['t9:campaign', 't10:campaign']);
	assert.deepEqual(both.map((d) => [d.trial.description, d.trial.budget]), [['Trial nine.', 'budget 9'], ['Trial ten.', 'budget 10']]);
	// trial 9 ends: it leaves "Working on now" (its list is under Trials)
	two.campaigns[1] = { ...two.campaigns[1], status: 'finished' };
	assert.deepEqual(ids(nowDocs(doc(8), two)), ['t10:campaign']);
	// a trial being prepared counts as not finished; the probe rounds never do
	two.campaigns.unshift({ ...later(11, 'preparing', ''), started_at: null }, { ...later(11, 'running', ''), id: 't11p', labels: { ...t8.labels, kind: 'probes' } });
	assert.deepEqual(ids(nowDocs(null, two)), ['t10:campaign', 't11:campaign']);
	// every trial ended: the newest, ended, as before (the next trial's outline probes show under it)
	two.campaigns = two.campaigns.map((c) => ({ ...c, status: 'finished' as const }));
	const ended = nowDocs(null, two);
	assert.deepEqual(ids(ended), ['t11:campaign']);
	assert.equal(ended[0].trial.status, 'finished');

	const t6 = docFromCampaign(r.campaigns.find((c) => c.id === 't6')!, r.results);
	assert.deepEqual(t6.submissions.find((s) => s.kind === 'reuse')!.reuses, { theorem: 'NestedSeatAlloc.IntPolicy.eq27_er1_clbi', url: '', relation: 'part' });
});

test('campaigns and results: every page, a bad entry left out, a failed source keeps the page as it is', async () => {
	assert.equal(parseCampaigns({ campaigns: {} }), null);
	assert.equal(parseResults(null), null);
	const bad = parseCampaigns({ campaigns: [{ id: 'x y', status: 'running', labels: {}, items: [] }, { id: 't9', status: 'odd', labels: {}, items: [] }, { ...fixture.campaigns.campaigns[0], items: [{ id: 'p2m-aaaaaa', url: 'https://evil.example/x', labels: {} }] }] })!;
	assert.deepEqual(bad.items.map((c: Campaign) => c.id), ['t8']);
	assert.equal(bad.items[0].items[0].url, '', 'a link outside prove2me’s theorem pages is dropped');
	assert.equal(parseResults({ results: [{ id: 'r-1', state: 'won', submitted_at: '2026-11-18T00:00:00Z', labels: {} }] })!.items.length, 0);
	const pages: Record<string, unknown> = {
		'/v1/projects/prove2me/campaigns?limit=200': { campaigns: fixture.campaigns.campaigns.slice(0, 3), next: 'c1' },
		'/v1/projects/prove2me/campaigns?limit=200&after=c1': { campaigns: fixture.campaigns.campaigns.slice(3), next: null },
		'/v1/projects/prove2me/results?limit=500': fixture.results,
		'/v1/tasks?project=prove2me&limit=500': fixture.tasks,
	};
	const urls: string[] = [];
	const get = async (u: string) => {
		urls.push(u);
		if (!(u in pages)) throw new Error(`404 ${u}`);
		return pages[u];
	};
	const got = await fetchRecords(get);
	assert.ok(got);
	assert.equal(got.campaigns.length, 7);
	assert.equal(got.results.length, 8);
	assert.equal(got.tasks.tasks.length, 154);
	assert.ok(urls.includes('/v1/projects/prove2me/campaigns?limit=200&after=c1'));
	for (const broken of Object.keys(pages)) {
		const fail = async (u: string) => (u === broken ? Promise.reject(new Error('500')) : get(u));
		assert.equal(await fetchRecords(fail), null, broken);
	}
	// the home page and /science/: only the tasks that decide a leaf
	const seen: string[] = [];
	const v = await liveSummary(h, async (u) => {
		seen.push(u);
		return u.startsWith('/v1/tasks') ? fixture.tasks : get(u);
	});
	assert.ok(seen.includes('/v1/tasks?project=prove2me&role=outline,assembly&limit=500'));
	assert.equal(v!.kpis.accepted, 8);
	assert.equal(await liveSummary(h, async () => Promise.reject(new Error('down'))), null);
});

test('the home page’s line and the /science/ card', () => {
	const v = scienceView(h, records());
	assert.equal(
		homeLine(v, 'Qwen3.8 27B'),
		'proofs by Qwen3.8 27B accepted on prove2me so far (6 of 26 open leaves tried; 2 more closed by reusing a proof already there), where theorems from research papers are stated in Lean 4 and every proof is checked by machine. Trial 8 is running.',
	);
	assert.deepEqual(cardNumbers(v), [
		{ key: 'accepted', value: '8', label: 'proofs accepted by prove2me, of 8 submitted' },
		{ key: 'open', value: '6 of 26', label: 'open leaves proved; 2 more closed by reuse' },
		{ key: 'trial', value: 'Trial 8', label: 'running since 16 November 2026' },
	]);
	const done = scienceView(h, { ...records(), campaigns: records().campaigns.map((c) => (c.id === 't8' ? { ...c, status: 'finished' as const, ended_at: '2026-11-19T00:00:00Z' } : c)) });
	assert.equal(done.current, null);
	assert.doesNotMatch(homeLine(done, 'M'), /Trial 8/);
});

test('the build’s task rows carry tokens computed, the cache re-reads beside them', () => {
	const rows = builtTaskRows(p2m);
	const r = rows.find((x) => x.id === 't1-A01')!;
	const run = p2m.trials[0].leaves.find((x) => x.leaf === 'A01')!;
	assert.equal(r.tokens, run.prompt_tokens - run.cached_tokens + run.output_tokens);
	assert.equal(r.tokens_with_cache, p2m.tasks.find((x) => x.id === 't1-A01')!.tokens);
	const t5 = rows.find((x) => x.id.startsWith('t5-') && x.tokens !== null)!;
	assert.ok(t5.tokens! < t5.tokens_with_cache!);
	// a probe task's own counts are not in the build: unknown, not the cache-inclusive figure
	assert.equal(rows.find((x) => x.id === 't6p-N02')!.tokens, null);
});

test('the submissions table: newest trial first, the newest submission first within a trial', () => {
	const v = scienceView(h, records());
	assert.deepEqual(newestFirst(v.submissions).map((s) => s.alias), ['N12', 'N07', 'N10', 'N03', 'N05', 'N02', 'F01', 'O04']);
	const rows = submissionRowsHTML(v);
	assert.equal((rows.match(/<tr /g) ?? []).length, 8);
	assert.ok(rows.indexOf('data-submission="N12"') < rows.indexOf('data-submission="O04"'));
	// the trial leads, not the day: a later trial's proof submitted before an earlier trial's comes first
	const s0 = v.submissions[0];
	const mixed = [{ ...s0, alias: 'A', trial: 9, submitted_at: '2026-11-20T08:00:00Z' }, { ...s0, alias: 'B', trial: 10, submitted_at: '2026-11-19T08:00:00Z' }];
	assert.deepEqual(newestFirst(mixed).map((s) => s.alias), ['B', 'A']);
});

test('a curated reuse: N18 (trial 9) imports a theorem another prover had proved, so it is a reuse, not a proof by the model', async () => {
	const { recordsOf } = await import('../src/data/prove2me-build');
	const snap = (await import('../src/data/science/prove2me-records.json')).default;
	// the records say "proof"; the correction makes it a reuse with the theorem it imports
	const raw18 = (snap as { results: { results: { item: string }[] } }).results.results.find((r) => r.item === 'p2m-faf1e3')!;
	const [r18] = parseResults({ results: [raw18] })!.items;
	assert.deepEqual([r18.labels.kind, r18.labels.relation, r18.labels.reuses_author], ['reuse', 'import', 'Eyal1990']);
	assert.match(r18.labels.reuses, /^WilliamsonShmoys\.WilliamsonShmoys_maximal_planar_supergraph_edge_count_upper__/);
	// only for its own trial: the same leaf's result from another trial stays as it is
	assert.equal(parseResults({ results: [{ ...raw18, campaign: 't10', labels: { alias: 'N18', kind: 'proof', trial: 't10' } }] })!.items[0].labels.kind, 'proof');
	// the page's numbers from the records of 2026-11-23: 11 accepted, 8 open leaves proved by the model, 3 closed by reuse
	const live = recordsOf(snap)!;
	const v = scienceView(h, live);
	assert.deepEqual([v.kpis.accepted, v.kpis.openProved, v.kpis.reused], [11, 8, 3]);
	assert.ok(!v.kpis.openProvedLeaves.some((l) => l.leaf === 'p2m-faf1e3'));
	const s18 = v.submissions.find((s) => s.leaf === 'p2m-faf1e3')!;
	assert.equal(s18.kind, 'reuse');
	assert.equal(s18.reuses!.url, 'https://prove2.me/theorems/896d696c-1e18-4de1-b5bd-ab3d52a97871');
	assert.match(submissionRowHTML(s18), /imports <a href="https:\/\/prove2\.me\/theorems\/896d696c/);
	assert.equal(s18.reuses!.author, 'Eyal1990');
	// trial 9: its run of N18 is no run (as trial 6's N05), 1 of 6 open runs proved
	const t9 = v.trials.find((t) => t.label === '9')!;
	assert.deepEqual([t9.open.proved, t9.open.runs], [1, 5]);
});
