// The proved theorems as JSON (/science/prove2me/theorems.json): the counts, the three costs with what they cover, and
// every accepted leaf, as the build computed them from the gateway's records (prove2me-theorems.ts). The records
// themselves are live at the gateway's public endpoints, which the JSON names.
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute } from 'astro';
import { p2m } from '../../../data/prove2me';
import { theoremsJSON } from '../../../data/prove2me-theorems';
import { buildTheorems } from '../../../data/prove2me-theorems-build';

export const GET: APIRoute = async ({ site }) => {
	const { view } = await buildTheorems();
	const origin = (site?.origin ?? 'https://aisathome.org').replace(/\/$/, '');
	return new Response(JSON.stringify(theoremsJSON(view, origin, p2m.model), null, '\t') + '\n', { headers: { 'Content-Type': 'application/json; charset=utf-8' } });
};
