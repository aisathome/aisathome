// /llms.txt (https://llmstxt.org, src/data/seo.ts): what AIs@home is, for language models and AI answer engines.
import type { APIRoute } from 'astro';
import { llmsTxt } from '../data/seo';

export const GET: APIRoute = () => new Response(llmsTxt(), { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
