// The proved theorems page (prove2me-theorems.ts) on fixed data: the committed records copy (the gateway's public
// records with the reuse backfill applied) and the build's history. The expected numbers are counted from the raw
// records here, not from the page's own functions.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import snapshot from '../src/data/science/prove2me-records.json';
import type { Prove2me } from '../src/data/prove2me';
import { historyOf, type Records } from '../src/data/prove2me-records';
import { loadRecords, recordsOf } from '../src/data/prove2me-build';
import {
	MEDIAN_FROM, SELECTED_FROM, cardOf, costsHTML, listCardOf, quantile, selectionText, theoremFactsHTML, theoremKpisHTML, theoremRowsHTML, theoremsJSON,
	theoremsView, updatedText,
} from '../src/data/prove2me-theorems';
import { theoremCardPNG } from '../src/data/og-card';
import { pngSize } from '../scripts/seo-checks.mjs';

const h = historyOf(raw as unknown as Prove2me);
type RawResult = { item: string; campaign: string | null; state: string; labels: Record<string, string>; reuse?: { name: string } | null };
type RawTask = { labels: Record<string, string>; model_min?: number | null; started_at?: string | null; steps?: number | null };
const snap = snapshot as unknown as { results: { results: RawResult[] }; tasks: { tasks: RawTask[] } };
const live = () => recordsOf(snapshot)!;
const trialNo = (c: string | null) => Number(/^t(\d+)/.exec(c ?? '')?.[1] ?? 0);

test('a reuse is a reuse: listed and marked, never a proof by the model nor in the costs', () => {
	const v = theoremsView(h, live());
	const reuses = v.theorems.filter((t) => t.kind === 'reuse');
	assert.deepEqual(reuses.map((t) => t.alias).sort(), ['N02', 'N05', 'N18']);
	assert.ok(reuses.every((t) => t.hours === null && t.tokens === null));
	// counted from the raw records: accepted results without a reuse record
	const acceptedRaw = snap.results.results.filter((r) => r.state === 'accepted');
	const ownRaw = acceptedRaw.filter((r) => !r.reuse && r.labels.kind !== 'reuse');
	assert.equal(v.counts.reused, 3);
	assert.equal(v.counts.proofs + v.counts.reused, v.counts.accepted);
	// the build's history adds O04 (trial 3), which the gateway's records hold too or not
	assert.ok(v.counts.proofs >= ownRaw.length && v.counts.proofs <= ownRaw.length + 1, `${v.counts.proofs} vs ${ownRaw.length}`);
	const rows = theoremRowsHTML(v, new Set(['p2m-faf1e3']));
	assert.match(rows, /data-theorem-leaf="p2m-faf1e3" data-kind="reuse"/);
	assert.match(rows, /Closed by reuse: imports <a href="https:\/\/prove2\.me\/theorems\/896d696c[^"]*">WilliamsonShmoys\./);
	assert.match(rows, /<a href="\/science\/prove2me\/theorems\/p2m-faf1e3\/">p2m-faf1e3<\/a>/);
	// a leaf without a page of its own (accepted since the build) links to prove2me only
	assert.doesNotMatch(rows, /href="\/science\/prove2me\/theorems\/p2m-f181df\/"/);
	assert.match(theoremKpisHTML(v).html, /leaves closed by reuse \(not counted as proofs\)<\/dt><dd>3</);
	// the same result without its reuse record counts as a proof (the gateway's record decides, not a file of ours)
	const noRecord: Records = { ...live(), results: live().results.map((r) => (r.item === 'p2m-faf1e3' ? { ...r, reuse: null, labels: { alias: 'N18', kind: 'proof', trial: 't9' } } : r)) };
	assert.equal(theoremsView(h, noRecord).counts.reused, 2);
});

test('the three costs come from the records: the tasks’ model minutes over the accepted proofs', () => {
	const v = theoremsView(h, live());
	const r = live();
	// independently: each accepted proof's own tasks in its trial (trial 6 on), from the raw task list
	const own = snap.results.results
		.filter((x) => x.state === 'accepted' && !x.reuse && trialNo(x.campaign) >= MEDIAN_FROM)
		.map((x) => snap.tasks.tasks.filter((t) => t.labels.trial === x.campaign && t.labels.leaf === x.item).reduce((a, t) => a + (t.model_min ?? 0), 0) / 60)
		.sort((a, b) => a - b);
	assert.ok(v.costs.median);
	assert.equal(v.costs.median.n, own.length);
	assert.ok(Math.abs(v.costs.median.value - quantile(own, 0.5)) < 0.01, `${v.costs.median.value} vs ${quantile(own, 0.5)}`);
	// all-in from trial 11 on: the proofs from those trials
	const recentProofs = snap.results.results.filter((x) => x.state === 'accepted' && !x.reuse && trialNo(x.campaign) >= SELECTED_FROM).length;
	assert.equal(v.costs.recent.proofs, recentProofs);
	assert.equal(v.costs.recent.from, SELECTED_FROM);
	assert.ok(Math.abs(v.costs.recent.perProof! - v.costs.recent.hours / recentProofs) < 1e-9);
	assert.equal(v.costs.overall.proofs, v.counts.proofs);
	assert.ok(v.costs.overall.hours > v.costs.recent.hours);
	// the records move the numbers: twice the model time on every gateway task is twice the median and the recent all-in
	const twice: Records = { ...r, tasks: { ...r.tasks, tasks: r.tasks.tasks.map((t) => ({ ...t, model_min: t.model_min === null || t.model_min === undefined ? t.model_min : t.model_min * 2 })) } };
	const v2 = theoremsView(h, twice);
	assert.ok(Math.abs(v2.costs.median!.value - 2 * v.costs.median.value) < 1e-9);
	assert.ok(Math.abs(v2.costs.recent.perProof! - 2 * v.costs.recent.perProof!) < 1e-9);
	// and the page shows what was computed, never a number of its own
	const html = costsHTML(v2);
	assert.match(html, new RegExp(`<dd>${(2 * v.costs.median.value).toFixed(2)}</dd>`));
	assert.match(selectionText(v), new RegExp(`of the ${v.counts.selected.tried} open leaves tried`));
});

test('without the gateway: the build falls back on the committed copy and the page still has every number', async () => {
	const warned: string[] = [];
	const b = await loadRecords({}, async () => { throw new Error('ECONNREFUSED'); }, (m) => warned.push(m));
	assert.equal(b.from, 'snapshot');
	assert.equal(warned.length, 1);
	const v = theoremsView(h, b.records);
	assert.ok(v.theorems.length > 50);
	assert.ok(v.costs.median && v.costs.recent.perProof !== null && v.costs.overall.perProof !== null);
	assert.match(updatedText(v, 'saved'), /^From a saved copy of the gateway’s records of /);
	// the history alone (no records at all) still renders
	const bare = theoremsView(h, null);
	assert.equal(bare.source, 'build');
	assert.ok(theoremRowsHTML(bare, new Set()).length > 0);
});

test('every value is escaped; a theorem’s facts, card and JSON', () => {
	const v = theoremsView(h, live());
	const t = { ...v.theorems.find((x) => x.kind === 'proof')!, title: 'A <script>alert(1)</script> & "x"' };
	const rows = theoremRowsHTML({ ...v, theorems: [t] }, new Set([t.leaf]));
	assert.doesNotMatch(rows, /<script>/);
	assert.match(rows, /A &lt;script&gt;alert\(1\)&lt;\/script&gt; &amp; &quot;x&quot;/);
	assert.match(theoremFactsHTML(t), /data-fact="hours"><dt>GPU-hours<\/dt><dd>\d+\.\d+</);
	const png = theoremCardPNG(cardOf(t));
	assert.deepEqual(pngSize(png), { width: 1200, height: 630 });
	assert.deepEqual(pngSize(theoremCardPNG(listCardOf(v))), { width: 1200, height: 630 });
	const j = theoremsJSON(v, 'https://aisathome.org', 'Qwen3.8 27B');
	assert.equal(j.theorems.length, v.theorems.length);
	const n18 = j.theorems.find((x) => x.leaf === 'p2m-faf1e3')!;
	assert.equal(n18.kind, 'reuse');
	assert.equal(n18.gpu_hours, null);
	assert.equal(n18.page, 'https://aisathome.org/science/prove2me/theorems/p2m-faf1e3/');
});
