// The build's records (prove2me-build.ts): from the gateway, its committed copy when the gateway does not answer (and
// the build says so), or a file. No network here: the gateway is a stand-in.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import fixture from './fixtures/prove2me-records-2026-11-18.json';
import snapshot from '../src/data/science/prove2me-records.json';
import { loadRecords, recordsOf } from '../src/data/prove2me-build';

const fixturePath = fileURLToPath(new URL('./fixtures/prove2me-records-2026-11-18.json', import.meta.url));
/** A stand-in gateway answering from the fixture, page by page as the real one (one page each here). */
const gateway = async (url: string) => {
	if (url.startsWith('/v1/projects/prove2me/campaigns')) return fixture.campaigns;
	if (url.startsWith('/v1/projects/prove2me/results')) return fixture.results;
	if (url.startsWith('/v1/tasks')) return fixture.tasks;
	throw new Error(`unexpected ${url}`);
};
const down = async () => {
	throw new Error('ECONNREFUSED');
};

test('from the gateway: every list, no warning', async () => {
	const warned: string[] = [];
	const r = await loadRecords({}, gateway, (m) => warned.push(m));
	assert.equal(r.from, 'gateway');
	assert.equal(r.records.updated, fixture.tasks.updated);
	assert.equal(r.records.tasks.tasks.length, fixture.tasks.tasks.length);
	assert.equal(r.records.campaigns.length, fixture.campaigns.campaigns.length);
	assert.deepEqual(warned, []);
});

test('the gateway down or answering nonsense: the committed copy, and the build says so', async () => {
	for (const get of [down, async () => ({ nope: true })]) {
		const warned: string[] = [];
		const r = await loadRecords({}, get, (m) => warned.push(m));
		assert.equal(r.from, 'snapshot');
		assert.equal(r.records.updated, (snapshot as { tasks: { updated: string } }).tasks.updated);
		assert.equal(warned.length, 1);
		assert.match(warned[0], /did not answer the build; the page carries the committed copy of /);
	}
});

test('P2M_RECORDS: the committed copy without asking the gateway, or a file', async () => {
	const r = await loadRecords({ P2M_RECORDS: 'snapshot' }, async () => assert.fail('the gateway was asked'));
	assert.equal(r.from, 'snapshot');
	const f = await loadRecords({ P2M_RECORDS: fixturePath }, async () => assert.fail('the gateway was asked'));
	assert.equal(f.from, 'file');
	assert.equal(f.records.updated, fixture.tasks.updated);
	await assert.rejects(loadRecords({ P2M_RECORDS: fileURLToPath(new URL('../package.json', import.meta.url)) }), /is not a records file/);
});

test('the committed copy is a records file', () => {
	assert.ok(recordsOf(snapshot));
	assert.equal(recordsOf({ campaigns: {}, results: {}, tasks: {} }), null);
});
