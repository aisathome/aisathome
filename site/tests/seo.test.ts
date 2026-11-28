// What search engines and AI answer engines read (seo.ts): the JSON-LD of the home page and /node/, /robots.txt and
// /llms.txt, against the checks the built site gets too (scripts/seo-checks.mjs).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { homeJsonLd, homeTitle, jsonLdScript, llmsTxt, nodeJsonLd, origin, robotsTxt, searchCrawlers, trainingCrawlers } from '../src/data/seo';
import { nodeRelease } from '../src/data/node-release';
import { jsonLdProblems, llmsProblems, nodeJsonLdProblems, robotsGroups, robotsProblems } from '../scripts/seo-checks.mjs';

test('the home JSON-LD is an Organization and a WebSite, on the given domain', () => {
	assert.deepEqual(jsonLdProblems(homeJsonLd(), origin), []);
	const staging = 'https://staging.aisathome.org';
	assert.deepEqual(jsonLdProblems(homeJsonLd(staging), staging), []);
	assert.deepEqual(JSON.parse(jsonLdScript(homeJsonLd())), homeJsonLd());
});

test('/node/ describes the node software once: the release, the GitHub download, its publisher by @id', () => {
	assert.deepEqual(nodeJsonLdProblems(nodeJsonLd(), origin), []);
	const staging = 'https://staging.aisathome.org';
	assert.deepEqual(nodeJsonLdProblems(nodeJsonLd(staging), staging), []);
	const app = nodeJsonLd()['@graph'].find((n) => n['@type'] === 'SoftwareApplication') as Record<string, string>;
	assert.equal(app.softwareVersion, nodeRelease.version.replace(/^v/, ''));
	assert.equal(app.releaseNotes, `https://github.com/aisathome/aisathome/releases/tag/${nodeRelease.version}`);
	assert.ok(!jsonLdProblems(homeJsonLd(), origin).length && !homeJsonLd()['@graph'].some((n) => n['@type'] === 'SoftwareApplication'));
	const moved = nodeJsonLd();
	(moved['@graph'][1] as Record<string, unknown>).downloadUrl = `${origin}/node/`;
	assert.ok(nodeJsonLdProblems(moved, origin).some((p) => p.includes('downloadUrl')));
	const paid = nodeJsonLd();
	(paid['@graph'][1] as { offers: { price: string } }).offers.price = '5';
	assert.ok(nodeJsonLdProblems(paid, origin).some((p) => p.includes('free Offer')));
});

test('the JSON-LD checks catch a wrong shape', () => {
	const bad = homeJsonLd();
	bad['@graph'] = bad['@graph'].filter((n) => n['@type'] !== 'WebSite');
	assert.ok(jsonLdProblems(bad, origin).includes('not exactly one WebSite'));
	const person = homeJsonLd();
	(person['@graph'][0] as Record<string, unknown>).founder = { '@type': 'Person', name: 'Someone' };
	assert.ok(jsonLdProblems(person, origin).includes('names a person or contact'));
	const both = homeJsonLd();
	both['@graph'].push(nodeJsonLd()['@graph'][1] as never);
	assert.ok(jsonLdProblems(both, origin).some((p) => p.includes('SoftwareApplication')));
});

test('a "</script>" in the JSON-LD cannot close its script element', () => {
	assert.equal(jsonLdScript({ a: '</script><b>' }), '{"a":"\\u003c/script>\\u003cb>"}');
});

test('robots.txt lets every crawler in, AI crawlers too, and names the sitemap', () => {
	assert.deepEqual(robotsProblems(robotsTxt(), origin), []);
	assert.ok(robotsProblems('User-agent: GPTBot\nDisallow: /\n', origin).length > 0);
	assert.ok(robotsProblems(robotsTxt().replace('Disallow: /stats/', 'Disallow: /science/'), origin).includes('disallows /science/ for ' + searchCrawlers.join(', ')));
});

test('robots.txt names the search and AI answer crawlers, with the same rules as every other crawler', () => {
	const groups = robotsGroups(robotsTxt());
	assert.deepEqual(groups.map((g) => g.agents), [searchCrawlers, ['*']]);
	assert.deepEqual(groups[0].rules, groups[1].rules);
	for (const a of ['OAI-SearchBot', 'ChatGPT-User', 'PerplexityBot', 'Perplexity-User', 'Claude-SearchBot', 'Claude-User', 'ClaudeBot', 'Googlebot', 'Bingbot']) {
		assert.ok(searchCrawlers.includes(a), a);
		assert.ok(robotsProblems(robotsTxt().replace(`User-agent: ${a}\n`, ''), origin).includes(`${a} is not named in exactly one group`), a);
	}
	// a named group that forgot the private paths would let those crawlers into the account pages
	const loose = robotsTxt().replace(/^(User-agent: ClaudeBot\n)Disallow: \/account\n/m, '$1');
	assert.ok(robotsProblems(loose, origin).some((p) => p.startsWith('does not disallow /account for Googlebot')));
});

test('robots.txt leaves the training crawlers under "*": their policy is the owner\'s call', () => {
	for (const a of trainingCrawlers) {
		assert.ok(!robotsGroups(robotsTxt()).some((g) => g.agents.includes(a)), a);
		assert.ok(robotsProblems(`${robotsTxt()}\nUser-agent: ${a}\nDisallow: /\n`, origin).some((p) => p.startsWith(`names ${a}`)), a);
	}
});

test('llms.txt has the llmstxt.org shape, the main links and only the site’s own claims', () => {
	assert.deepEqual(llmsProblems(llmsTxt(), origin), []);
	assert.ok(llmsProblems(llmsTxt().replace('hardened, not confidential', 'confidential'), origin).includes('claims confidentiality'));
	assert.ok(llmsProblems(llmsTxt() + '\nThe Windows installer is signed.\n', origin).includes('claims a signed installer'));
	assert.ok(llmsProblems(llmsTxt().replace(`${origin}/use/api/`, `${origin}/x/`), origin).includes('no link to /use/api/'));
});

test('the home page title names the brand once', () => {
	assert.equal(homeTitle.match(/AIs@home/g)?.length, 1);
});
