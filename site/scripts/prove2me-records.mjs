// Refresh the committed copy of the gateway's public prove2me records (src/data/science/prove2me-records.json): the
// fallback a site build uses only when it cannot reach the gateway (src/data/prove2me-build.ts). Every build fetches the
// records itself, so this is never a step of a deploy. `npm run records` (PUBLIC_DOMAIN picks the gateway).
// SPDX-License-Identifier: Apache-2.0
import { writeFileSync } from 'node:fs';

const base = `https://${process.env.PUBLIC_DOMAIN || 'aisathome.org'}`;
const out = new URL('../src/data/science/prove2me-records.json', import.meta.url).pathname;

async function all(path, key) {
	const items = [];
	let first = null;
	let after = null;
	for (let i = 0; i < 20; i++) {
		const url = `${base}${path}${after ? `&after=${encodeURIComponent(after)}` : ''}`;
		const res = await fetch(url, { headers: { accept: 'application/json', 'user-agent': 'aisathome-site-build' } });
		if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`);
		const d = await res.json();
		first ??= d;
		items.push(...d[key]);
		if (!d.next) break;
		after = d.next;
	}
	return { ...first, [key]: items, next: null };
}

const [campaigns, results, tasks] = await Promise.all([
	all('/v1/projects/prove2me/campaigns?limit=200', 'campaigns'),
	all('/v1/projects/prove2me/results?limit=500', 'results'),
	all('/v1/tasks?project=prove2me&limit=500', 'tasks'),
]);
// who served a task (served_by) names volunteers by their public username: shown live only, never committed, so a
// username its owner clears is gone from the site with the gateway's next list (prove2me-build.ts withoutServed)
for (const t of tasks.tasks) delete t.served_by;
const source = `Public GET /v1/projects/prove2me/campaigns, /v1/projects/prove2me/results and /v1/tasks?project=prove2me from ${base}, ${tasks.updated}`;
writeFileSync(out, `${JSON.stringify({ source, campaigns, results, tasks }, null, '\t')}\n`);
console.log(`${out}: ${campaigns.campaigns.length} campaigns, ${results.results.length} results, ${tasks.tasks.length} tasks, as of ${tasks.updated}`);
