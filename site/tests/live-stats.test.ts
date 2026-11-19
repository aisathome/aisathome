// The live network strip's number format (live-stats.ts): plain under 1000, three digits from there.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { liveNumber } from '../src/data/live-stats';

test('under 1000 a number stays plain; from 1000 on it is short with three digits', () => {
	const want: [number, string][] = [
		[0, '0'], [2, '2'], [999, '999'], [1000, '1.00K'], [19_700, '19.7K'], [999_999, '1.00M'],
		[1_000_000_000, '1.00B'], [1_250_000_000, '1.25B'], [12.5e9, '12.5B'], [125e9, '125B'],
	];
	for (const [n, s] of want) assert.equal(liveNumber(n), s, String(n));
});
