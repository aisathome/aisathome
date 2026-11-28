// The prove2me page's data as JSON (/science/prove2me.json): the same committed file the page renders, so the two
// never disagree, with its licence (licence.ts). The network's share is not in it; that is the gateway's /v1/stats.
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute } from 'astro';
import { recordsLicense } from '../../data/licence';
import { p2m } from '../../data/prove2me';

export const GET: APIRoute = () =>
	new Response(JSON.stringify({ project: p2m.project, license: recordsLicense, ...p2m }, null, '\t') + '\n', { headers: { 'Content-Type': 'application/json; charset=utf-8' } });
