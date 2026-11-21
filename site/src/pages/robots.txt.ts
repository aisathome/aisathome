// /robots.txt (src/data/seo.ts): every crawler may read the public site; the sitemap is
// /sitemap-index.xml (Starlight's, @astrojs/sitemap). Caddy redirects /sitemap.xml there.
import type { APIRoute } from 'astro';
import { robotsTxt } from '../data/seo';

export const GET: APIRoute = () => new Response(robotsTxt(), { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
