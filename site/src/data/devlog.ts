// The dev log's posts, newest first. Drafts (`publish: false`) are built only with
// DEVLOG_DRAFTS=1, which the staging deploy sets so the maintainer can review them there;
// production builds never contain them (scripts/check-dist.mjs checks).
import { getCollection, type CollectionEntry } from 'astro:content';

export type Post = CollectionEntry<'devlog'>;

export const showDrafts = process.env.DEVLOG_DRAFTS === '1';

export async function posts(): Promise<Post[]> {
	const all = await getCollection('devlog', (p) => p.data.publish || showDrafts);
	return all.sort((a, b) => b.data.date.getTime() - a.data.date.getTime() || b.id.localeCompare(a.id));
}

export const postURL = (p: Post) => `/devlog/${p.id}/`;

// "12 October 2026"; dates are calendar days (UTC), not times.
export const formatDate = (d: Date) =>
	d.toLocaleDateString('en-GB', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' });

export const isoDate = (d: Date) => d.toISOString().slice(0, 10);

export const feed = { title: 'AIs@home dev log', path: '/devlog/rss.xml' };
