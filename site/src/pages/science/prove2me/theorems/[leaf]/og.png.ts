// One proved theorem's share card (/science/prove2me/theorems/<leaf>/og.png): its title, Lean name, trial, GPU-hours
// and tokens as the build computed them (og-card.ts draws it).
// SPDX-License-Identifier: Apache-2.0
import type { APIRoute, GetStaticPaths } from 'astro';
import { theoremCardPNG } from '../../../../../data/og-card';
import { cardOf, type TheoremRow } from '../../../../../data/prove2me-theorems';
import { buildTheorems } from '../../../../../data/prove2me-theorems-build';

export const getStaticPaths = (async () => (await buildTheorems()).pages.map((t) => ({ params: { leaf: t.leaf }, props: { t } }))) satisfies GetStaticPaths;

export const GET: APIRoute = ({ props }) =>
	new Response(new Uint8Array(theoremCardPNG(cardOf((props as { t: TheoremRow }).t))), { headers: { 'Content-Type': 'image/png' } });
