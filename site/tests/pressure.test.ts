// The network page's pressure section (pressure.ts) on fixed answers of GET /v1/stats/pressure (gateway pressure.go):
// what it accepts, the gap-free grid, and the line on what limits the network now.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fmtDuration, limitLine, onGrid, parseBucket, parsePressure, type Bucket } from '../src/data/pressure';

const g = (avg: number, peak = Math.ceil(avg)) => ({ avg, peak });
/** One bucket as the gateway serves it. */
function raw(start: string, more: Record<string, unknown> = {}) {
	return {
		start, samples: 360, slots: g(4), busy: g(2), utilisation: 0.5, queue: { people: g(0), science: g(1, 3) }, held: g(0), holds: 0,
		science_slots: g(2), open_tasks: g(5), demand: g(2), wait_s: {}, first_token_s: {}, outcomes: { ok: 10 }, ...more,
	};
}
const answer = (buckets: unknown[], last: unknown = raw('')) => ({
	updated: '2026-11-18T14:05:00Z', range: '7d', bucket: 'hour', sample_seconds: 10, buckets, total: last, last_24h: last,
	now: { slots: 4, busy: 2 },
});
const sum = (more: Record<string, unknown>) => parseBucket(raw('', more)) as Bucket;

test('an answer is checked; bad buckets are left out, the rest sorted', () => {
	const v = parsePressure(answer([raw('2026-11-18T14:00Z'), raw('2026-11-18T13:00Z'), raw('2026-11-18'), { start: '2026-11-18T12:00Z' },
		raw('2026-11-18T11:00Z', { samples: 0 }), raw('2026-11-18T10:00Z', { busy: { avg: -1, peak: 1 } })]));
	assert.ok(v);
	assert.deepEqual(v.buckets.map((b) => b.start), ['2026-11-18T13:00Z', '2026-11-18T14:00Z']);
	assert.equal(v.buckets[0].queue.science.peak, 3);
	for (const bad of [null, [], {}, { ...answer([]), bucket: 'week' }, { ...answer([]), updated: 'soon' }, answer([], { samples: 1 })]) {
		assert.equal(parsePressure(bad), null, JSON.stringify(bad));
	}
	// Classes with no requests, unknown classes and odd outcome names are dropped; utilisation stays within 0..1.
	const b = sum({ utilisation: 1.2, wait_s: { chat: { count: 0, p50: 0, p95: 0 }, science: { count: 3, p50: 1, p95: 9 }, admin: { count: 1, p50: 1, p95: 1 } },
		outcomes: { ok: 3, '<b>': 1, refused: 'x' } });
	assert.deepEqual(b.wait, { science: { count: 3, p50: 1, p95: 9 } });
	assert.deepEqual(b.outcomes, { ok: 3 });
	assert.equal(b.utilisation, 1);
	assert.equal(sum({ utilisation: undefined }).utilisation, null);
});

test('the grid has every hour from the first to the last, gaps as null', () => {
	const bs = parsePressure(answer([raw('2026-11-18T23:00Z'), raw('2026-11-19T02:00Z')]))!.buckets;
	const grid = onGrid(bs, 'hour');
	assert.deepEqual(grid.map((x) => [x.start, x.b !== null]), [
		['2026-11-18T23:00Z', true], ['2026-11-19T00:00Z', false], ['2026-11-19T01:00Z', false], ['2026-11-19T02:00Z', true],
	]);
	assert.deepEqual(onGrid([], 'hour'), []);
	const days = onGrid([parseBucket(raw('2026-11-17'))!, parseBucket(raw('2026-11-19'))!], 'day');
	assert.deepEqual(days.map((x) => x.start), ['2026-11-17', '2026-11-18', '2026-11-19']);
});

test('durations in words', () => {
	assert.deepEqual([0, 0.4, 3, 59.6, 180, 330, 900, 5400].map(fmtDuration),
		['no time', 'under 1 s', '3 s', '60 s', '3 min', '5.5 min', '15 min', '1.5 h']);
});

test('what limits us now', () => {
	assert.equal(limitLine(null), null);
	assert.equal(limitLine(sum({ samples: 0 })), null);
	assert.equal(limitLine(sum({ slots: g(0), busy: g(0), utilisation: undefined })), 'Nodes are the limit now: no GPU slots were online in the last 24 h.');
	// Busy slots, and science waiting longest: the slots are the limit.
	assert.equal(limitLine(sum({ utilisation: 0.92, wait_s: { chat: { count: 5, p50: 0, p95: 2 }, science: { count: 40, p50: 30, p95: 180 } } })),
		'GPU slots are the limit now: they were 92% busy in the last 24 h; science waited p95 3 min for a slot.');
	// Not very busy, but a long wait still says the slots are the limit (a burst, or one large node).
	assert.match(limitLine(sum({ utilisation: 0.4, wait_s: { agent: { count: 5, p50: 1, p95: 45 } } }))!, /^GPU slots are the limit now: .*API and agents waited p95 45 s/);
	// Idle: not the slots; refusals are named.
	assert.equal(limitLine(sum({ utilisation: 0.35, wait_s: { chat: { count: 9, p50: 0, p95: 0 } }, outcomes: { ok: 9, refused: 1 } })),
		'GPU slots are not the limit now: they were 35% busy in the last 24 h; no request waited for one; 1 of 10 requests found no node to take it.');
	assert.match(limitLine(sum({ utilisation: 0.9, outcomes: { ok: 1000, refused: 12 } }))!, /; 12 of 1,012 requests found no node to take them\.$/);
	assert.equal(limitLine(sum({ utilisation: 0.1, outcomes: {} })), 'GPU slots are not the limit now: they were 10% busy in the last 24 h.');
});
