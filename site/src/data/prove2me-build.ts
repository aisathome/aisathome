// The gateway's public prove2me records for the site's build (since 2026-11-23,
// no manual data pushes): every build fetches the campaigns, results and tasks (prove2me-records.ts fetchRecords, every page)
// from the gateway of the domain it builds for, so each deploy's page without JavaScript, and the browser's page before
// the gateway answers, carry the records as they were at the build. The browser then reads the same records live.
//
//   P2M_RECORDS unset or "gateway"   fetch from https://<PUBLIC_DOMAIN>; if that fails, the committed copy (below)
//   P2M_RECORDS=snapshot             the committed copy, no fetch (a build that must not depend on the network)
//   P2M_RECORDS=<path>               a records file of the same shape (the browser tests build from their fixture)
//
// The committed copy, science/prove2me-records.json, is only the fallback for a build that cannot reach the gateway;
// such a build says so on stderr and on the page (the status block's time line). `npm run records` refreshes it.
//
// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from 'node:fs';
import snapshot from './science/prove2me-records.json';
import { parseTasks } from './prove2me-current';
import { fetchRecords, parseCampaigns, parseResults, type Records } from './prove2me-records';
import { domain } from './site';

/** Where the build's records came from: the gateway, the committed copy (the gateway did not answer, or by choice), or
 * a file named by P2M_RECORDS. */
export type RecordsFrom = 'gateway' | 'snapshot' | 'file';
export interface BuildRecords {
	records: Records;
	from: RecordsFrom;
}

/** The records without who served each task (`served_by`): a GPU's owner's public username is shown only live, from the
 * gateway's current list, never in the build's HTML, so a username cleared on the gateway leaves the site at once
 * instead of staying in pages built while it was set (prove2me-proof.ts). */
export function withoutServed(r: Records): Records {
	return { ...r, tasks: { ...r.tasks, tasks: r.tasks.tasks.map(({ served_by: _, ...t }) => t) } };
}

/** A records file (the committed copy, a test fixture): each list as one page of the gateway's answer. Null when it is
 * not one. */
export function recordsOf(d: unknown): Records | null {
	const v = d as { campaigns?: unknown; results?: unknown; tasks?: unknown };
	const c = parseCampaigns(v?.campaigns);
	const r = parseResults(v?.results);
	const t = parseTasks(v?.tasks);
	return c && r && t ? { updated: t.updated, campaigns: c.items, results: r.items, tasks: t } : null;
}

/** A URL's JSON from the gateway at `base`, within `ms`; throws on an HTTP error, a timeout or a body that is not JSON. */
const getFrom = (base: string, ms: number) => async (url: string) => {
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), ms);
	try {
		const res = await fetch(base + url, { signal: ctl.signal, headers: { accept: 'application/json', 'user-agent': 'aisathome-site-build' } });
		if (!res.ok) throw new Error(`HTTP ${res.status}`);
		return await res.json();
	} finally {
		clearTimeout(timer);
	}
};

/** The build's records as `env` asks (see above); `get` fetches from the gateway (tests pass their own). */
export async function loadRecords(
	env: Record<string, string | undefined> = process.env,
	get: (url: string) => Promise<unknown> = getFrom(`https://${env.PUBLIC_DOMAIN || domain}`, 20000),
	warn: (msg: string) => void = (m) => console.warn(m),
): Promise<BuildRecords> {
	const want = env.P2M_RECORDS ?? '';
	const saved = recordsOf(snapshot);
	if (!saved) throw new Error('prove2me: the committed records copy (science/prove2me-records.json) is not a records file');
	if (want === 'snapshot') return { records: saved, from: 'snapshot' };
	if (want && want !== 'gateway') {
		const r = recordsOf(JSON.parse(readFileSync(want, 'utf8')));
		if (!r) throw new Error(`prove2me: P2M_RECORDS=${want} is not a records file`);
		return { records: r, from: 'file' };
	}
	const live = await fetchRecords(get).catch(() => null);
	if (live) return { records: live, from: 'gateway' };
	warn(`prove2me: the gateway's records did not answer the build; the page carries the committed copy of ${saved.updated} (science/prove2me-records.json)`);
	return { records: saved, from: 'snapshot' };
}

let once: Promise<BuildRecords> | null = null;
/** The build's records, fetched once per build (every page that shows them reads the same), without who served each task
 * (withoutServed). */
export const buildRecords = () => (once ??= loadRecords().then((b) => ({ ...b, records: withoutServed(b.records) })));
