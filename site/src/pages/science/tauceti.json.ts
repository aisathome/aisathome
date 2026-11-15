// The Tau Ceti page's final data (a finished project, 2026-11-15), published as it is committed in
// src/data/science/tauceti.json: public fields only (scripts/check-dist.mjs checks them). The old
// address, /v1/projects/tauceti/science, redirects here.
import type { APIRoute } from 'astro';
import raw from '../../data/science/tauceti.json';

export const GET: APIRoute = () =>
	new Response(JSON.stringify(raw, null, 1) + '\n', { headers: { 'Content-Type': 'application/json; charset=utf-8' } });
