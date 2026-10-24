// The data behind /science/tauceti/, served unchanged (see src/data/science.ts).
import type { APIRoute } from 'astro';
import { rawScience } from '../../data/science';

export const GET: APIRoute = () =>
	new Response(JSON.stringify(rawScience, null, 1) + '\n', {
		headers: { 'content-type': 'application/json; charset=utf-8' },
	});
