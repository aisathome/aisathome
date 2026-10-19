// The dev log as an RSS 2.0 feed (/devlog/rss.xml): title, link, date and summary per post.
// Written by hand: no dependency for a few lines of XML.
import type { APIRoute } from 'astro';
import { posts, postURL, feed } from '../../data/devlog';

const esc = (s: string) =>
	s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&apos;');

export const GET: APIRoute = async ({ site }) => {
	const base = (site?.href ?? 'https://aisathome.org/').replace(/\/$/, '');
	const list = await posts();
	const items = list.map((p) => {
		const link = base + postURL(p);
		return `    <item>
      <title>${esc(p.data.title)}</title>
      <link>${esc(link)}</link>
      <guid isPermaLink="true">${esc(link)}</guid>
      <pubDate>${p.data.date.toUTCString()}</pubDate>
      <description>${esc(p.data.summary)}</description>
    </item>`;
	});
	const updated = list[0]?.data.date ?? new Date(Date.UTC(2026, 9, 12));
	const xml = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom">
  <channel>
    <title>${esc(feed.title)}</title>
    <link>${esc(base)}/devlog/</link>
    <atom:link href="${esc(base + feed.path)}" rel="self" type="application/rss+xml"/>
    <description>Notes from building AIs@home: open AI models on volunteers’ GPUs.</description>
    <language>en</language>
    <lastBuildDate>${updated.toUTCString()}</lastBuildDate>
${items.join('\n')}
  </channel>
</rss>
`;
	return new Response(xml, { headers: { 'Content-Type': 'application/rss+xml; charset=utf-8' } });
};
