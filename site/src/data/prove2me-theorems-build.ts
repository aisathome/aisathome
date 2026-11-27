// The proved theorems as the build sees them (pages/science/prove2me/theorems/): the same records the prove2me page
// builds from (prove2me-build.ts: the gateway's, or the committed copy when the build cannot reach it), so the list,
// each theorem's page, its share card and the JSON export are made from one read. The browser reads the records again.
// SPDX-License-Identifier: Apache-2.0
import { buildRecords, type RecordsFrom } from './prove2me-build';
import { p2m } from './prove2me';
import { historyOf, type History } from './prove2me-records';
import { theoremsView, type TheoremRow, type TheoremsView } from './prove2me-theorems';

/** A leaf id that may name a page (p2m- and 6 or 8 hex digits). */
export const pageLeaf = /^p2m-[0-9a-f]{6}([0-9a-f]{2})?$/;

export interface BuildTheorems {
	history: History;
	view: TheoremsView;
	from: RecordsFrom;
	/** The accepted leaves that get a page of their own. */
	pages: TheoremRow[];
}

let once: Promise<BuildTheorems> | null = null;
/** The build's view of the proved theorems, made once per build. */
export const buildTheorems = () =>
	(once ??= buildRecords().then(({ records, from }) => {
		const history = historyOf(p2m);
		const view = theoremsView(history, records);
		return { history, view, from, pages: view.theorems.filter((t) => pageLeaf.test(t.leaf)) };
	}));
