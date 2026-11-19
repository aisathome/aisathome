// The network page's pressure section (pressure.ts) on fixed answers of GET /v1/stats/pressure (gateway pressure.go):
// what it accepts, the gap-free grid, and the line on what limits the network now.
// SPDX-License-Identifier: Apache-2.0
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	LEAN_WAIT_LIMIT_S, RUNNER_CPU_LIMIT, RUNNER_FREE_LIMIT_GB, RUNNER_REFAULTS_LIMIT, fmtDuration, limitLine, onGrid, parseBucket, parseNode,
	parsePressure, parseRunner, rangeWords, runnerLimits, type Bucket,
} from '../src/data/pressure';

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

/** The runners' part as the gateway serves it: a 7.6 GB runner, nothing at its limit. */
function runner(more: Record<string, unknown> = {}, lean: Record<string, unknown> = {}) {
	return {
		reports: 120, cpu: g(0.4, 0.9), iowait: g(0.02, 0.1), steal: g(0, 0), mem_total_gb: 7.6, mem_private_gb: g(1.3, 2.2),
		page_cache_gb: { avg: 5.6, low: 5.0 }, refaults_s: g(300, 4000), disk_read_mb_s: g(2, 60), disk_write_mb_s: g(0.5, 9),
		free_gb_low: 28, psi: { cpu: g(0.1, 0.5), memory: g(0.01, 0.2), io: g(0.02, 0.3) },
		lean: { slots: 1, busy: g(0.3, 1), waiting: g(0.1, 2), run_s: { count: 900, p50: 8, p95: 30 }, wait_s: { count: 400, p50: 0, p95: 4 }, ...lean },
		sessions: g(2.5, 3), spot_notices: 0, ...more,
	};
}

test('the runners and nodes parts are checked; a bad one is dropped, the bucket stays', () => {
	const b = sum({ runner: runner(), node: { requests: 50, engine_timed: 40, prefill_share: 0.21, prefill_tok_s: 900, decode_tok_s: 31.5, cache_hit: 0.83 },
		runners: g(1.5, 2), queue: { people: g(0, 3), science: g(1, 4), all: g(1, 5) } });
	assert.equal(b.runner?.reports, 120);
	assert.equal(b.runner?.lean.wait?.p95, 4);
	assert.equal(b.runner?.psiMemory?.avg, 0.01);
	assert.deepEqual(b.node, { requests: 50, engineTimed: 40, prefillShare: 0.21, prefillTokS: 900, decodeTokS: 31.5, cacheHit: 0.83 });
	assert.equal(b.runners?.peak, 2);
	assert.equal(b.queue.all.peak, 5);
	for (const bad of [{ reports: 0 }, { ...runner(), cpu: 'x' }, { ...runner(), page_cache_gb: { avg: 1 } }, { ...runner(), lean: null }, 'runner']) {
		const x = sum({ runner: bad });
		assert.equal(x.runner, null, JSON.stringify(bad));
		assert.equal(x.samples, 360);
	}
	assert.equal(parseRunner(runner({ psi: undefined }))?.psiMemory, null); // a kernel without PSI
	assert.equal(parseRunner(runner({}, { wait_s: { count: 0, p50: 0, p95: 0 } }))?.lean.wait, null);
	assert.equal(parseNode({ requests: 0 }), null);
	assert.equal(parseNode({ requests: 2, prefill_share: 3, cache_hit: -1 })?.prefillShare, 1);
	assert.equal(parseNode({ requests: 2, cache_hit: -1 })?.cacheHit, null);
	// Waiting together: never less than either alone (a bucket from before the joint peak was kept).
	assert.equal(sum({ queue: { people: g(0, 3), science: g(1, 4) } }).queue.all.peak, 4);
	assert.equal(sum({ queue: { people: g(0, 3), science: g(1, 4), all: g(1, 0) } }).queue.all.peak, 4);
});

test('the range comes from the answer; words for the note', () => {
	assert.equal(parsePressure({ ...answer([]), range: '48h' })?.range, '48h');
	assert.equal(parsePressure({ ...answer([]), range: '1y' })?.range, '7d');
	assert.equal(parsePressure({ ...answer([]), range: undefined, bucket: 'day' })?.range, '30d');
	assert.equal(rangeWords('hour', 48), 'the last 48 hours');
	assert.equal(rangeWords('hour', 168), 'the last 7 days');
	assert.equal(rangeWords('hour', 165), 'the last 7 days');
	assert.equal(rangeWords('day', 30), 'the last 30 days');
	assert.equal(rangeWords('hour', 1), 'so far');
});

test("what limits the runners, at the profile's thresholds", () => {
	const r = (more: Record<string, unknown> = {}, lean: Record<string, unknown> = {}) => parseRunner(runner(more, lean))!;
	assert.deepEqual(runnerLimits(r()), []);
	assert.deepEqual(runnerLimits(r({ refaults_s: g(RUNNER_REFAULTS_LIMIT, 9000) })), ['memory']);
	assert.deepEqual(runnerLimits(r({ refaults_s: g(RUNNER_REFAULTS_LIMIT - 1, 90000) })), []); // a peak alone is not the limit
	assert.deepEqual(runnerLimits(r({ cpu: g(RUNNER_CPU_LIMIT, 1) })), ['cpu']);
	assert.deepEqual(runnerLimits(r({ cpu: g(RUNNER_CPU_LIMIT - 0.01, 1) })), []);
	assert.deepEqual(runnerLimits(r({}, { wait_s: { count: 5, p50: 9, p95: LEAN_WAIT_LIMIT_S } })), ['lean']);
	assert.deepEqual(runnerLimits(r({}, { wait_s: { count: 5, p50: 9, p95: LEAN_WAIT_LIMIT_S - 1 } })), []);
	assert.deepEqual(runnerLimits(r({ free_gb_low: RUNNER_FREE_LIMIT_GB - 0.1 })), ['disk']);
	assert.deepEqual(runnerLimits(r({ free_gb_low: RUNNER_FREE_LIMIT_GB })), []);
	assert.deepEqual(runnerLimits(r({ refaults_s: g(3800, 9000), cpu: g(0.9, 1) }, { wait_s: { count: 5, p50: 20, p95: 45 } })), ['memory', 'cpu', 'lean']);
});

test('what limits us now, with the runners', () => {
	// Nothing at its limit: both named as not.
	assert.equal(limitLine(sum({ utilisation: 0.35, wait_s: { chat: { count: 9, p50: 0, p95: 0 } }, runner: runner() })),
		'Neither GPU slots nor runners are the limit now: GPU slots were 35% busy in the last 24 h; no request waited for one; ' +
		'runners were 40% busy (CPU), Lean checks waited p95 4 s for a slot.');
	// Runner memory: the re-reads in MB/s and the cache, the Lean wait.
	assert.equal(limitLine(sum({ utilisation: 0.4, wait_s: { science: { count: 9, p50: 5, p95: 20 } },
		runner: runner({ refaults_s: g(4096, 20000), page_cache_gb: { avg: 4.1, low: 2.5 }, cpu: g(0.6, 1) }, { wait_s: { count: 50, p50: 10, p95: 45 } }) })),
		'Runner memory and the runners\' Lean slots are the limit now: GPU slots were 40% busy in the last 24 h; science waited p95 20 s for a slot; ' +
		'runners were 60% busy (CPU), re-read 16 MB/s from disk that memory could not keep (4.1 GB of files cached on average), Lean checks waited p95 45 s for a slot.');
	// GPU and runner CPU together; one alone takes "is".
	assert.match(limitLine(sum({ utilisation: 0.9, runner: runner({ cpu: g(0.92, 1) }) }))!, /^GPU slots and runner CPU are the limit now: GPU slots were 90% busy/);
	assert.match(limitLine(sum({ utilisation: 0.2, runner: runner({ cpu: g(0.92, 1) }) }))!, /^Runner CPU is the limit now: /);
	assert.match(limitLine(sum({ utilisation: 0.2, runner: runner({ free_gb_low: 3.2 }) }))!, /^Runner disk is the limit now: .*, only 3.2 GB of disk free at the lowest\.$/);
	// No Lean waits reported: the clause leaves them out.
	assert.equal(limitLine(sum({ utilisation: 0.2, outcomes: {}, runner: runner({}, { wait_s: { count: 0, p50: 0, p95: 0 } }) })),
		'Neither GPU slots nor runners are the limit now: GPU slots were 20% busy in the last 24 h; runners were 40% busy (CPU).');
	// No slots online: the nodes first, whatever the runners say.
	assert.equal(limitLine(sum({ slots: g(0), busy: g(0), utilisation: undefined, runner: runner({ cpu: g(0.99, 1) }) })),
		'Nodes are the limit now: no GPU slots were online in the last 24 h.');
});

test("the thresholds are the runner profile's numbers (a change here is a decision, not a refactor)", () => {
	const r = (more: Record<string, unknown> = {}, lean: Record<string, unknown> = {}) => parseRunner(runner(more, lean))!;
	// refaults: thrash measured at ~3,800/s, fine minutes at <= ~1,200/s
	assert.deepEqual(runnerLimits(r({ refaults_s: g(2000, 2000) })), ['memory']);
	assert.deepEqual(runnerLimits(r({ refaults_s: g(1999, 1999) })), []);
	assert.deepEqual(runnerLimits(r({ cpu: g(0.85, 1) })), ['cpu']);
	assert.deepEqual(runnerLimits(r({ cpu: g(0.84, 1) })), []);
	assert.deepEqual(runnerLimits(r({}, { wait_s: { count: 1, p50: 30, p95: 30 } })), ['lean']);
	assert.deepEqual(runnerLimits(r({}, { wait_s: { count: 1, p50: 29, p95: 29 } })), []);
	assert.deepEqual(runnerLimits(r({ free_gb_low: 4.9 })), ['disk']);
	assert.deepEqual(runnerLimits(r({ free_gb_low: 5 })), []);
});
