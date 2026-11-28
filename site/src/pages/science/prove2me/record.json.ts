// The build's data for the prove2me page's sections that open (/science/prove2me/record.json): every task's row, the
// trials' lists, every leaf and "Working on now" as the build had them (prove2me-page.ts). The page draws an opened
// section from the gateway's records it reads, and from this for what only the build has (trials 1 to 5, their tasks)
// or while the gateway does not answer. Public fields only: the same as the page and the gateway's public records.
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute } from 'astro';
import { prove2mePage } from '../../../data/prove2me-page';

export const GET: APIRoute = async () =>
	new Response(JSON.stringify((await prove2mePage()).record), { headers: { 'Content-Type': 'application/json; charset=utf-8' } });
