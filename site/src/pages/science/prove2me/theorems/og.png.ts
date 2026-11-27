// The proved theorems' share card (/science/prove2me/theorems/og.png): the count and the three costs as the build
// computed them (og-card.ts draws it).
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute } from 'astro';
import { theoremCardPNG } from '../../../../data/og-card';
import { listCardOf } from '../../../../data/prove2me-theorems';
import { buildTheorems } from '../../../../data/prove2me-theorems-build';

export const GET: APIRoute = async () => {
	const { view } = await buildTheorems();
	return new Response(new Uint8Array(theoremCardPNG(listCardOf(view))), { headers: { 'Content-Type': 'image/png' } });
};
