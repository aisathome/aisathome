// The download block's release (node-release.ts): the gateway's GET /v1/node/release, checked before the browser shows
// it, and the sizes and link made from it the same way as from the build's copy.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	bundleSizeOf,
	installerSize,
	installerSizeOf,
	installerURL,
	installerURLOf,
	liveInstallerURL,
	liveRelease,
	needs,
	nodeRelease,
	type NodeRelease,
} from '../src/data/node-release';

const build: NodeRelease = { version: 'v0.7.0', installerBytes: 390_380_154, bundleBytes: 478_190_808, versionFreeInstaller: true };
const answer = { version: 'v0.8.0', installerBytes: 391_000_000, bundleBytes: 480_000_000, versionFreeInstaller: true, fetched_at: '2026-11-26T12:00:00Z' };

test('a newer or equal release from the gateway is shown, with only its four fields', () => {
	assert.deepEqual(liveRelease(answer, build), { version: 'v0.8.0', installerBytes: 391_000_000, bundleBytes: 480_000_000, versionFreeInstaller: true });
	assert.equal(liveRelease({ ...answer, version: 'v0.7.0', installerBytes: 1 }, build)?.installerBytes, 1); // the same release, sizes corrected
	for (const version of ['v0.7.1', 'v0.10.0', 'v1.0.0', 'v0.7.10']) assert.equal(liveRelease({ ...answer, version }, build)?.version, version);
});

test('an older release than the build knows is not shown (a gateway that has not read GitHub since)', () => {
	for (const version of ['v0.6.9', 'v0.6.10', 'v0.0.1']) assert.equal(liveRelease({ ...answer, version }, build), null, version);
	// numbers, not strings: v0.10.0 is newer than v0.9.0
	assert.equal(liveRelease({ ...answer, version: 'v0.9.0' }, { ...build, version: 'v0.10.0' }), null);
	assert.equal(liveRelease({ ...answer, version: 'v0.7.2' }, { ...build, version: 'v0.7.3' }), null);
});

test('a malformed answer is not shown', () => {
	const bad: unknown[] = [
		null, undefined, 'v0.8.0', 42, [], {},
		...['0.8.0', 'v0.8', 'v0.8.0-rc1', 'v08.0.0', 'V0.8.0', ' v0.8.0', 'v0.8.0<b>', '', 8].map((version) => ({ ...answer, version })),
		...[0, -1, 1.5, '391000000', null, Number.MAX_SAFE_INTEGER + 2, Infinity].flatMap((n) => [{ ...answer, installerBytes: n }, { ...answer, bundleBytes: n }]),
		{ ...answer, versionFreeInstaller: 'true' },
		{ ...answer, versionFreeInstaller: undefined },
	];
	for (const d of bad) assert.equal(liveRelease(d, build), null, JSON.stringify(d));
});

test('sizes and the button link come from a release the same way at the build and live', () => {
	assert.equal(installerSizeOf(390_380_154), '372 MB');
	assert.equal(bundleSizeOf(478_190_808), 'about 0.5 GB');
	assert.equal(bundleSizeOf(640_000_000), 'about 0.6 GB');
	assert.equal(installerURLOf(build), 'https://github.com/aisathome/aisathome/releases/latest/download/AIsAtHome-node-setup.exe');
	assert.equal(installerURLOf({ ...build, version: 'v0.5.0', versionFreeInstaller: false }),
		'https://github.com/aisathome/aisathome/releases/download/v0.5.0/AIsAtHome-node-setup-v0.5.0.exe');
	assert.equal(installerURL, installerURLOf(nodeRelease));
	assert.equal(installerSize, installerSizeOf(nodeRelease.installerBytes));
	assert.equal(needs.bundle, bundleSizeOf(nodeRelease.bundleBytes));
	// the build's copy passes its own check
	assert.deepEqual(liveRelease(nodeRelease), nodeRelease);
});

test('a live release links its own file by tag, the build copy the moving latest link', () => {
	const r = liveRelease(answer, build)!;
	assert.equal(liveInstallerURL(r), 'https://github.com/aisathome/aisathome/releases/download/v0.8.0/AIsAtHome-node-setup.exe');
	assert.equal(liveInstallerURL({ ...r, versionFreeInstaller: false }),
		'https://github.com/aisathome/aisathome/releases/download/v0.8.0/AIsAtHome-node-setup-v0.8.0.exe');
	assert.notEqual(liveInstallerURL(nodeRelease), installerURL); // the static page keeps releases/latest/download
	assert.ok(installerURL.includes('/releases/latest/download/'));
});
