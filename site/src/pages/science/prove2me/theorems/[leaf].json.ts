// One proved theorem as JSON (/science/prove2me/theorems/<leaf>.json): its row of theorems.json.
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute, GetStaticPaths } from 'astro';
import { recordsLicense } from '../../../../data/licence';
import { p2m } from '../../../../data/prove2me';
import { theoremsJSON } from '../../../../data/prove2me-theorems';
import { buildTheorems } from '../../../../data/prove2me-theorems-build';

export const getStaticPaths = (async () => (await buildTheorems()).pages.map((t) => ({ params: { leaf: t.leaf } }))) satisfies GetStaticPaths;

export const GET: APIRoute = async ({ params, site }) => {
	const { view } = await buildTheorems();
	const origin = (site?.origin ?? 'https://aisathome.org').replace(/\/$/, '');
	const all = theoremsJSON(view, origin, p2m.model);
	const one = all.theorems.find((t) => t.leaf === params.leaf);
	const body = { about: all.about, license: recordsLicense, updated: all.updated, source: all.source, definitions: all.definitions, theorem: one };
	return new Response(JSON.stringify(body, null, '\t') + '\n', { headers: { 'Content-Type': 'application/json; charset=utf-8' } });
};
