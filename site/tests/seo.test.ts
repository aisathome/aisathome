// What search engines and AI answer engines read (seo.ts): the home page's JSON-LD, /robots.txt and
// /llms.txt, against the checks the built site gets too (scripts/seo-checks.mjs).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { homeJsonLd, homeTitle, jsonLdScript, llmsTxt, origin, robotsTxt } from '../src/data/seo';
import { jsonLdProblems, llmsProblems, robotsProblems } from '../scripts/seo-checks.mjs';

test('the JSON-LD is an Organization, a WebSite and the node software, on the given domain', () => {
	assert.deepEqual(jsonLdProblems(homeJsonLd(), origin), []);
	const staging = 'https://staging.aisathome.org';
	assert.deepEqual(jsonLdProblems(homeJsonLd(staging), staging), []);
	assert.deepEqual(JSON.parse(jsonLdScript(homeJsonLd())), homeJsonLd());
});

test('the JSON-LD checks catch a wrong shape', () => {
	const bad = homeJsonLd();
	bad['@graph'] = bad['@graph'].filter((n) => n['@type'] !== 'WebSite');
	assert.ok(jsonLdProblems(bad, origin).includes('not exactly one WebSite'));
	const person = homeJsonLd();
	(person['@graph'][0] as Record<string, unknown>).founder = { '@type': 'Person', name: 'Someone' };
	assert.ok(jsonLdProblems(person, origin).includes('names a person or contact'));
	const paid = homeJsonLd();
	(paid['@graph'][2] as { offers: { price: string } }).offers.price = '5';
	assert.ok(jsonLdProblems(paid, origin).some((p) => p.includes('free Offer')));
});

test('a "</script>" in the JSON-LD cannot close its script element', () => {
	assert.equal(jsonLdScript({ a: '</script><b>' }), '{"a":"\\u003c/script>\\u003cb>"}');
});

test('robots.txt lets every crawler in, AI crawlers too, and names the sitemap', () => {
	assert.deepEqual(robotsProblems(robotsTxt(), origin), []);
	assert.ok(robotsProblems('User-agent: GPTBot\nDisallow: /\n', origin).length > 0);
	assert.ok(robotsProblems(robotsTxt().replace('Disallow: /stats/', 'Disallow: /science/'), origin).includes('disallows /science/'));
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
