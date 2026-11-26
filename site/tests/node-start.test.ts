// The start of a node (src/data/node-start.ts), shared by the home page and /node/: the steps' links point at
// words that are there, and the pictures of the node's page have the sizes the pages reserve for them (a new
// picture from e2e/browser/node_page_image.py with another height needs the new size there).
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { linuxSteps, nodePageShots } from '../src/data/node-start';

/** A WebP's width and height (lossy VP8, lossless VP8L or extended VP8X). */
function webpSize(b: Buffer): { width: number; height: number } {
	assert.equal(b.toString('latin1', 0, 4), 'RIFF');
	assert.equal(b.toString('latin1', 8, 12), 'WEBP');
	const kind = b.toString('latin1', 12, 16);
	if (kind === 'VP8 ') return { width: b.readUInt16LE(26) & 0x3fff, height: b.readUInt16LE(28) & 0x3fff };
	if (kind === 'VP8L') {
		const v = b.readUInt32LE(21);
		return { width: (v & 0x3fff) + 1, height: ((v >> 14) & 0x3fff) + 1 };
	}
	if (kind === 'VP8X') return { width: b.readUIntLE(24, 3) + 1, height: b.readUIntLE(27, 3) + 1 };
	throw new Error(`unknown WebP chunk ${kind}`);
}

test('each linked step contains the words it links', () => {
	for (const s of linuxSteps) if (s.link) assert.ok(s.text.includes(s.link.words), s.text);
});

test('the pictures of the node page have the sizes the pages give them', () => {
	for (const [name, shot] of Object.entries(nodePageShots)) {
		for (const theme of ['light', 'dark'] as const) {
			const file = new URL(`../public${shot.src(theme)}`, import.meta.url);
			assert.deepEqual(webpSize(readFileSync(file)), { width: shot.width, height: shot.height }, `${name} ${theme}`);
		}
		assert.match(shot.alt, /example data/);
	}
});
