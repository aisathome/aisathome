// The project's Discord server. The site links to /discord/ (the header's Discord icon, the home page), never to
// Discord itself, so the visitor statistics (tools/sitestats: page views per path) count those clicks as views of
// /discord/. While there is no server the page says it is coming soon; once there is one, set `invite` to its invite
// link (one that never expires) and the page sends visitors on to it, still counted the same way.
// SPDX-License-Identifier: Apache-2.0

export const invite = '';

export const discordPath = '/discord/';

const inviteLink = /^https:\/\/discord\.(gg|com\/invite)\/[A-Za-z0-9-]{2,64}$/;

/** The invite to send visitors on to, or null while there is none. Anything that is not a Discord invite link fails
 * the build rather than sending visitors somewhere else. */
export function inviteTarget(link: string): string | null {
	if (link === '') return null;
	if (!inviteLink.test(link)) throw new Error(`not a Discord invite link: ${JSON.stringify(link)}`);
	return link;
}
