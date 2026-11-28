// The prove2me page's sections drawn when they open (2026-11-28): each trial's list, each leaf's outlines and tasks in
// "Working on now" and the list of every leaf. Closed, the build draws a place that links the record page; opened, the
// browser draws the same HTML the full renderers draw (the record page's), on the records of 2026-11-18.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import raw from '../src/data/science/prove2me.json';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import type { Prove2me } from '../src/data/prove2me';
import type { PublicTask } from '../src/data/prove2me-current';
import { builtDetail, historyOf, leavesView, parseCampaigns, parseResults, trialDetails, type Records, type TrialDetail } from '../src/data/prove2me-records';
import { nowAll, nowBlocksHTML, nowLeafPartsHTML } from '../src/data/prove2me-now';
import { RECORD_PATH, leafListHTML, leafRowsHTML, trialDetailHTML, trialListHTML } from '../src/data/prove2me-view';

const p2m = raw as unknown as Prove2me;
const records = (): Records => ({
	updated: fixture.tasks.updated,
	campaigns: parseCampaigns(fixture.campaigns)!.items,
	results: parseResults(fixture.results)!.items,
	tasks: { updated: fixture.tasks.updated, tasks: structuredClone(fixture.tasks.tasks) as unknown as PublicTask[] },
});
const h = historyOf(p2m);
const details = (live: Records) =>
	trialDetails(p2m.trials.filter((t) => t.trial > 5).map((t) => builtDetail(p2m, h, t.trial)).filter((d): d is TrialDetail => !!d), live, h);

test('a closed trial has its row, facts and counts, and a link to its list on the record page; opened, the full list', () => {
	const live = records();
	const ds = details(live);
	assert.ok(ds.length >= 3);
	for (const d of ds) {
		const closed = trialDetailHTML(d, false);
		const full = trialDetailHTML(d, true);
		const n = d.sum.number;
		assert.ok(!closed.includes('<table') && !closed.includes('data-trial-leaf'), `trial ${n}: closed, no list`);
		assert.ok(closed.includes(`<a href="${RECORD_PATH}#trial-${n}" data-p2m-record-link="trial">`), `trial ${n}: the record page's link`);
		assert.ok(closed.includes('data-trial-facts') && closed.includes('data-trial-numbers'));
		// the list drawn when it opens is the full one, in its place
		const list = trialListHTML(d);
		assert.ok(list.includes('<table') && full.includes(list));
		assert.equal(full.replace(list, ''), closed.replace(/<div class="p2m-lazy"[\s\S]*?<\/div><\/details>$/, '</details>'));
		assert.ok(!/\sopen[\s>]/.test(full.slice(0, full.indexOf('>'))), 'the list drawn does not open the trial');
	}
});

test('"Working on now" closed: each row without its outlines and tasks, a link to them; opened, the full row\'s', () => {
	const live = records();
	const docs = nowAll(null, live);
	const full = nowBlocksHTML(docs, live.tasks);
	const lazy = nowBlocksHTML(docs, live.tasks, { full: () => false });
	assert.ok(full.includes('data-leaf-plan') && !lazy.includes('data-leaf-plan') && !lazy.includes('data-leaf-tasks'));
	const rows = [...lazy.matchAll(/data-current-leaf="([^"]+)"/g)].map((m) => m[1]);
	assert.equal(rows.length, 6);
	for (const alias of rows) {
		const parts = nowLeafPartsHTML(docs, live.tasks, {}, 't8', alias);
		assert.ok(parts && full.includes(parts), `${alias}: its parts are the full row's`);
	}
	// the anchors stay in the closed rows: #leaf-<id> and #leaf-<alias>
	for (const m of full.matchAll(/\sid="(leaf-[^"]+)"/g)) assert.ok(lazy.includes(` id="${m[1]}"`), m[1]);
	assert.ok(lazy.includes(`href="${RECORD_PATH}#leaf-`));
	// a leaf or a block that is not there any more draws nothing
	assert.equal(nowLeafPartsHTML(docs, live.tasks, {}, 't8', 'Z99'), null);
	assert.equal(nowLeafPartsHTML(docs, live.tasks, {}, 't99', rows[0]), null);
	// an open row is drawn in full with its block
	const one = nowBlocksHTML(docs, live.tasks, { full: (t, a) => t === 't8' && a === rows[0] });
	assert.equal((one.match(/data-leaf-plan=/g) ?? []).length, 1);
});

test('every leaf, opened: the filters, the count and every row', () => {
	const v = leavesView(h, records());
	const html = leafListHTML(v);
	assert.ok(html.includes(leafRowsHTML(v)));
	assert.ok(html.includes(`Showing ${v.leaves.length} of ${v.leaves.length}`));
	assert.equal((html.match(/data-leaf-sort=/g) ?? []).length, 6);
});
