// The dev log's posts, newest first. Drafts (`publish: false`) are built only with
// DEVLOG_DRAFTS=1, which the staging deploy sets so the maintainer can review them there;
// production builds never contain them (scripts/check-dist.mjs checks).
import { getCollection, type CollectionEntry } from 'astro:content';

export type Post = CollectionEntry<'devlog'>;

export const showDrafts = process.env.DEVLOG_DRAFTS === '1';

// Every post, drafts included, must be named "<number>-..." and numbers must be unique: the
// build fails otherwise, so a number is never used twice.
export async function posts(): Promise<Post[]> {
	const every = await getCollection('devlog');
	const seen = new Map<number, string>();
	for (const p of every) {
		if (!p.id.startsWith(`${p.data.number}-`)) {
			throw new Error(`devlog: ${p.id} has number ${p.data.number}; name the file ${p.data.number}-<slug>.md`);
		}
		if (seen.has(p.data.number)) throw new Error(`devlog: #${p.data.number} is used by ${seen.get(p.data.number)} and ${p.id}`);
		seen.set(p.data.number, p.id);
	}
	return every
		.filter((p) => p.data.publish || showDrafts)
		.sort((a, b) => b.data.number - a.data.number);
}

// "Dev log #1": the post's label, above its title (never part of the title).
export const label = (p: Post) => `Dev log #${p.data.number}`;

export const postURL = (p: Post) => `/devlog/${p.id}/`;

// "12 October 2026"; dates are calendar days (UTC), not times.
export const formatDate = (d: Date) =>
	d.toLocaleDateString('en-GB', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' });

// "18 Oct 2026"
export const shortDate = (d: Date) =>
	d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });

export const isoDate = (d: Date) => d.toISOString().slice(0, 10);

export const feed = { title: 'AIs@home dev log', path: '/devlog/rss.xml' };
