// The Discord page (src/data/discord.ts): coming soon without an invite, a Discord invite link with one, and never
// anything else.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { discordPath, invite, inviteTarget } from '../src/data/discord';

test('no invite: the page says coming soon', () => {
	assert.equal(inviteTarget(''), null);
});

test('an invite link is the target', () => {
	for (const l of ['https://discord.gg/abcDEF12', 'https://discord.com/invite/aisathome', 'https://discord.gg/a-b']) {
		assert.equal(inviteTarget(l), l);
	}
});

test('anything else fails the build', () => {
	for (const l of ['discord.gg/abc', 'http://discord.gg/abc', 'https://discord.gg/', 'https://discord.gg/abc/x',
		'https://discord.gg.evil.test/abc', 'https://evil.test/?https://discord.gg/abc', 'https://discord.com/channels/1/2',
		' https://discord.gg/abc', 'https://discord.gg/abc"><script>']) {
		assert.throws(() => inviteTarget(l), /not a Discord invite link/, l);
	}
});

test('the committed invite is valid, and the page is a page the statistics count', () => {
	assert.doesNotThrow(() => inviteTarget(invite));
	// tools/sitestats counts a GET of a path ending in "/" or ".html" as a page view, per path.
	assert.match(discordPath, /^\/[a-z]+\/$/);
	// and leaves out the gateway's and the build's own paths (tools/sitestats/stats.go notSite)
	for (const x of ['/v1/', '/account', '/stats/', '/__login', '/_astro/', '/pagefind/']) assert.ok(!discordPath.startsWith(x), x);
});
