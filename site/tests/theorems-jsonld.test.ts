// The proved theorems' JSON-LD (src/data/theorems-jsonld.ts): the list's Dataset and a proof page's CreativeWork, in the
// shape the built site is checked against (scripts/seo-checks.mjs), with titles from prove2me's users that cannot
// close the script they go in.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { jsonLdScript, origin } from '../src/data/seo';
import { RECORDS_LICENSE_URL, recordsLicense, recordsLicenceText } from '../src/data/licence';
import { theoremJsonLd, theoremsDatasetJsonLd, theoremsJsonLdForRoute } from '../src/data/theorems-jsonld';
import { datasetJsonLdProblems, jsonLdScripts, proofJsonLdProblems } from '../scripts/seo-checks.mjs';

const model = 'Qwen3.8 27B';
const description = 'prove2me leaf p2m-77fca9 (Eq. 46): Qwen3.8 27B, an open model on the AIs@home network, proved it in trial 16.';
const statement = 'https://prove2.me/theorems/77fca99d-839b-4710-937d-62c0c5ca93fd';
const row = {
	leaf: 'p2m-77fca9', kind: 'proof' as const, theorem: 'LemonsPoA.TwoSided.eq_46', title: '§5.2, Eq. (46)', url: statement,
	submitted_at: '2026-11-27T19:08:37Z',
};
const staging = 'https://staging.aisathome.org';

test('the list is a Dataset: its JSON download, AIs@home as creator, the Apache License 2.0', () => {
	const d = theoremsDatasetJsonLd({ updated: '2026-11-28T10:40:24Z' }, model, `Every leaf of prove2me that ${model} proved, and what a proof costs in GPU-hours.`);
	assert.deepEqual(datasetJsonLdProblems(d, origin), []);
	assert.equal(d.distribution[0].contentUrl, `${origin}/science/prove2me/theorems.json`);
	assert.equal(d.name, `Lean theorems ${model} proved on prove2me`);
	assert.equal(d.license, 'https://www.apache.org/licenses/LICENSE-2.0');
	const s = theoremsDatasetJsonLd({ updated: '2026-11-28T10:40:24Z' }, model, 'x'.repeat(60), staging);
	assert.deepEqual(datasetJsonLdProblems(s, staging), []);
	assert.ok(datasetJsonLdProblems(s, origin).some((p) => p.startsWith('url is')));
});

test('the Dataset checks catch a wrong shape', () => {
	const ok = () => theoremsDatasetJsonLd({ updated: '2026-11-28T10:40:24Z' }, model, 'y'.repeat(80));
	assert.ok(datasetJsonLdProblems({ ...ok(), description: 'short' }, origin).includes('description is not 50 to 5000 characters'));
	assert.ok(datasetJsonLdProblems({ ...ok(), license: 'https://creativecommons.org/licenses/by/4.0/' }, origin).some((p) => p.startsWith('license is')));
	const { license: _, ...unlicensed } = ok();
	assert.ok(datasetJsonLdProblems(unlicensed, origin).some((p) => p.startsWith('license is')));
	assert.ok(datasetJsonLdProblems({ ...ok(), distribution: [] }, origin).includes('no DataDownload of theorems.json'));
	assert.ok(datasetJsonLdProblems({ ...ok(), dateModified: 'yesterday' }, origin).some((p) => p.startsWith('dateModified')));
	assert.ok(datasetJsonLdProblems({ ...ok(), creator: { '@type': 'Person', name: 'Someone' } }, origin).length >= 2);
});

test('a proof is a CreativeWork about the prove2me theorem, based on its statement, dated when submitted', () => {
	const d = theoremJsonLd(row, model, description);
	assert.deepEqual(proofJsonLdProblems(d, origin, row.leaf), []);
	assert.equal(d.url, `${origin}/science/prove2me/theorems/p2m-77fca9/`);
	assert.deepEqual(d.about, { '@type': 'Thing', name: '§5.2, Eq. (46)', alternateName: row.theorem, identifier: row.leaf, url: statement });
	assert.equal(d.isBasedOn, statement);
	assert.equal(d.dateCreated, row.submitted_at);
	assert.equal(d.license, RECORDS_LICENSE_URL);
	assert.ok(proofJsonLdProblems({ ...d, license: undefined }, origin, row.leaf).some((p) => p.startsWith('license is')));
	assert.ok(d.keywords.includes(model) && d.name.startsWith('Lean 4 proof of '));
	assert.deepEqual(d.isPartOf, { '@id': `${origin}/science/prove2me/theorems/#dataset` });
	assert.deepEqual(proofJsonLdProblems(theoremJsonLd(row, model, description, staging), staging, row.leaf), []);
});

test('a leaf closed by reuse is not credited to the model, and is based on the reused theorem too', () => {
	const reused = 'https://prove2.me/theorems/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
	const d = theoremJsonLd({ ...row, kind: 'reuse', reuses: { theorem: 'Other.thm', url: reused, author: 'someone' } }, model, description);
	assert.deepEqual(proofJsonLdProblems(d, origin, row.leaf), []);
	assert.deepEqual(d.isBasedOn, [statement, reused]);
	assert.ok(!d.keywords.includes(model) && !JSON.stringify(d).includes('someone'));
});

test('a statement link that is not a prove2me theorem page is left out', () => {
	const d = theoremJsonLd({ ...row, url: 'javascript:alert(1)' }, model, description);
	assert.ok(!('isBasedOn' in d) && !('url' in d.about));
	assert.deepEqual(proofJsonLdProblems(d, origin, row.leaf), []);
	assert.ok(proofJsonLdProblems({ ...d, isBasedOn: 'https://example.com/' }, origin, row.leaf).some((p) => p.includes('example.com')));
});

test('a title with "</script>" stays inside its script: escaped, and the page reads it back whole', () => {
	const evil = '</script><script>alert(1)</script><!-- "quoted" &  ';
	const d = theoremJsonLd({ ...row, title: evil, theorem: evil }, model, `${description} ${evil}`);
	const script = jsonLdScript(d);
	assert.ok(!script.includes('<'), script);
	const page = `<head><script type="application/ld+json">${script}</script><title>x</title></head>`;
	const [back] = jsonLdScripts(page);
	assert.deepEqual(back, JSON.parse(JSON.stringify(d)));
	assert.equal(back.about.alternateName, evil);
});

test('only the list and its proof pages get theorems JSON-LD', async () => {
	assert.equal(await theoremsJsonLdForRoute('node', 'x'), null);
	assert.equal(await theoremsJsonLdForRoute('science/prove2me', 'x'), null);
	assert.equal(await theoremsJsonLdForRoute('science/prove2me/theoremsx', 'x'), null);
});

test('the records\' licence: Apache-2.0 as on prove2me, and statements stay their authors\'', () => {
	assert.equal(RECORDS_LICENSE_URL, 'https://www.apache.org/licenses/LICENSE-2.0');
	assert.deepEqual(recordsLicense, { spdx: 'Apache-2.0', url: RECORDS_LICENSE_URL, note: recordsLicenceText });
	assert.match(recordsLicenceText, /Apache License 2\.0/);
	assert.match(recordsLicenceText, /theorem statements belong to their prove2me authors/);
});
