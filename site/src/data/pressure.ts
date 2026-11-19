// The network page's "Pressure" section: the gateway's GET /v1/stats/pressure (gateway pressure.go), checked and put
// in words. Pure functions, unit-tested (tests/pressure.test.ts); the page draws the charts (pressure-chart.ts).
// Aggregates only: the endpoint has no node, operator, project or user, and nothing here adds one.
// SPDX-License-Identifier: Apache-2.0

export const CLASSES = ['chat', 'agent', 'science'] as const;
export type Klass = (typeof CLASSES)[number];
export const CLASS_LABEL: Record<Klass, string> = { chat: 'chat', agent: 'API and agents', science: 'science' };

export interface Gauge { avg: number; peak: number }
export interface Dur { count: number; p50: number; p95: number }
/** The science runners' minute reports in a bucket (gateway pressure_runner.go): averages over runner-minutes. */
export interface RunnerStats {
	reports: number;
	cpu: Gauge; // share 0..1
	iowait: Gauge;
	memTotalGb: number;
	memPrivateGb: Gauge;
	pageCacheGb: { avg: number; low: number };
	refaultsS: Gauge; // evicted file pages read again, per second
	diskReadMBs: Gauge;
	diskWriteMBs: Gauge;
	freeGbLow: number;
	psiMemory: Gauge | null;
	lean: { slots: number; busy: Gauge; waiting: Gauge; run: Dur | null; wait: Dur | null };
	sessions: Gauge;
	spotNotices: number;
}
/** How the nodes' answers spent their time (gateway pressure_node.go). */
export interface NodeStats {
	requests: number;
	engineTimed: number;
	prefillShare: number | null; // 0..1
	prefillTokS: number | null;
	decodeTokS: number | null;
	cacheHit: number | null; // 0..1
}
export interface Bucket {
	start: string; // "2026-11-18T13:00Z" (an hour) or "2026-11-18" (a day); "" for a sum
	samples: number;
	utilisation: number | null; // 0..1; null without slots online
	slots: Gauge;
	busy: Gauge;
	queue: { people: Gauge; science: Gauge; all: Gauge };
	scienceSlots: Gauge;
	openTasks: Gauge;
	demand: Gauge;
	wait: Partial<Record<Klass, Dur>>;
	firstToken: Partial<Record<Klass, Dur>>;
	outcomes: Record<string, number>;
	holds: number;
	runners: Gauge | null; // runners reporting (sampled)
	runner: RunnerStats | null;
	node: NodeStats | null;
}
export interface PressureView {
	range: Range;
	bucket: 'hour' | 'day';
	buckets: Bucket[];
	last24h: Bucket;
	updated: Date;
}

/** The ranges the endpoint serves: hourly up to 14 days, daily beyond. */
export const RANGES = ['24h', '48h', '7d', '14d', '30d', '90d'] as const;
export type Range = (typeof RANGES)[number];
export const DEFAULT_RANGE: Range = '7d';
export const isRange = (v: unknown): v is Range => typeof v === 'string' && (RANGES as readonly string[]).includes(v);

const num = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n >= 0;
const obj = (v: unknown): Record<string, unknown> | null => (v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null);

function gauge(v: unknown): Gauge | null {
	const o = obj(v);
	return o && num(o.avg) && num(o.peak) ? { avg: o.avg, peak: o.peak } : null;
}

function durs(v: unknown): Partial<Record<Klass, Dur>> {
	const o = obj(v), out: Partial<Record<Klass, Dur>> = {};
	for (const k of CLASSES) {
		const d = obj(o?.[k]);
		if (d && num(d.count) && d.count > 0 && num(d.p50) && num(d.p95)) out[k] = { count: d.count, p50: d.p50, p95: d.p95 };
	}
	return out;
}

function dur(v: unknown): Dur | null {
	const d = obj(v);
	return d && num(d.count) && d.count > 0 && num(d.p50) && num(d.p95) ? { count: d.count, p50: d.p50, p95: d.p95 } : null;
}

/** The runners' part of a bucket, or null when missing or malformed (the rest of the bucket stays). */
export function parseRunner(v: unknown): RunnerStats | null {
	const o = obj(v);
	if (!o || !num(o.reports) || o.reports <= 0) return null;
	const pc = obj(o.page_cache_gb), lean = obj(o.lean), psi = obj(o.psi);
	const g = {
		cpu: gauge(o.cpu), iowait: gauge(o.iowait), memPrivateGb: gauge(o.mem_private_gb), refaultsS: gauge(o.refaults_s),
		diskReadMBs: gauge(o.disk_read_mb_s), diskWriteMBs: gauge(o.disk_write_mb_s), sessions: gauge(o.sessions),
		busy: gauge(lean?.busy), waiting: gauge(lean?.waiting),
	};
	if (Object.values(g).some((x) => x === null) || !pc || !num(pc.avg) || !num(pc.low) || !num(o.mem_total_gb) ||
		!num(o.free_gb_low) || !lean || !num(lean.slots)) return null;
	return {
		reports: o.reports, cpu: { avg: Math.min(g.cpu!.avg, 1), peak: Math.min(g.cpu!.peak, 1) }, iowait: g.iowait!,
		memTotalGb: o.mem_total_gb, memPrivateGb: g.memPrivateGb!, pageCacheGb: { avg: pc.avg, low: pc.low }, refaultsS: g.refaultsS!,
		diskReadMBs: g.diskReadMBs!, diskWriteMBs: g.diskWriteMBs!, freeGbLow: o.free_gb_low, psiMemory: gauge(psi?.memory),
		lean: { slots: lean.slots, busy: g.busy!, waiting: g.waiting!, run: dur(lean.run_s), wait: dur(lean.wait_s) },
		sessions: g.sessions!, spotNotices: num(o.spot_notices) ? o.spot_notices : 0,
	};
}

/** The nodes' part of a bucket, or null. Shares stay within 0..1. */
export function parseNode(v: unknown): NodeStats | null {
	const o = obj(v);
	if (!o || !num(o.requests) || o.requests <= 0) return null;
	const share = (x: unknown) => (num(x) ? Math.min(x, 1) : null);
	return {
		requests: o.requests, engineTimed: num(o.engine_timed) ? o.engine_timed : 0, prefillShare: share(o.prefill_share),
		prefillTokS: num(o.prefill_tok_s) ? o.prefill_tok_s : null, decodeTokS: num(o.decode_tok_s) ? o.decode_tok_s : null,
		cacheHit: share(o.cache_hit),
	};
}

/** One bucket (or a sum) of the answer, or null when its shape is wrong. */
export function parseBucket(v: unknown): Bucket | null {
	const o = obj(v);
	if (!o || !num(o.samples)) return null;
	const q = obj(o.queue);
	const g = {
		slots: gauge(o.slots), busy: gauge(o.busy), people: gauge(q?.people), science: gauge(q?.science),
		scienceSlots: gauge(o.science_slots), openTasks: gauge(o.open_tasks), demand: gauge(o.demand),
	};
	if (Object.values(g).some((x) => x === null)) return null;
	const outcomes: Record<string, number> = {};
	for (const [k, n] of Object.entries(obj(o.outcomes) ?? {})) if (num(n) && /^[a-z]+$/.test(k)) outcomes[k] = n;
	const u = o.utilisation;
	// Together: never less than either alone (a bucket from before the gateway kept the joint peak has 0 there).
	const all = gauge(q?.all) ?? { avg: g.people!.avg + g.science!.avg, peak: 0 };
	all.peak = Math.max(all.peak, g.people!.peak, g.science!.peak);
	return {
		start: typeof o.start === 'string' ? o.start : '',
		samples: o.samples,
		utilisation: num(u) ? Math.min(u, 1) : null,
		slots: g.slots!, busy: g.busy!, queue: { people: g.people!, science: g.science!, all },
		scienceSlots: g.scienceSlots!, openTasks: g.openTasks!, demand: g.demand!,
		wait: durs(o.wait_s), firstToken: durs(o.first_token_s), outcomes, holds: num(o.holds) ? o.holds : 0,
		runners: gauge(o.runners), runner: parseRunner(o.runner), node: parseNode(o.node),
	};
}

const HOUR = /^\d{4}-\d{2}-\d{2}T\d{2}:00Z$/, DAY = /^\d{4}-\d{2}-\d{2}$/;

/** The endpoint's answer, checked; null when it is not one (the section then stays hidden). Bad buckets are left out. */
export function parsePressure(raw: unknown): PressureView | null {
	const o = obj(raw);
	if (!o || (o.bucket !== 'hour' && o.bucket !== 'day') || !Array.isArray(o.buckets)) return null;
	const last24h = parseBucket(o.last_24h);
	const updated = new Date(typeof o.updated === 'string' ? o.updated : NaN);
	if (!last24h || Number.isNaN(updated.getTime())) return null;
	const re = o.bucket === 'hour' ? HOUR : DAY;
	const buckets = o.buckets.map(parseBucket).filter((b): b is Bucket => b !== null && re.test(b.start) && b.samples > 0)
		.sort((a, b) => (a.start < b.start ? -1 : a.start > b.start ? 1 : 0));
	const range = isRange(o.range) ? o.range : o.bucket === 'hour' ? DEFAULT_RANGE : '30d';
	return { range, bucket: o.bucket, buckets, last24h, updated };
}

const startMs = (s: string) => Date.parse(s.length === 10 ? `${s}T00:00Z` : s);

/** The buckets on a gap-free grid from the first to the last: an hour or day with no data (the gateway was down) is
 * null, so a chart shows the gap instead of joining across it. */
export function onGrid(buckets: Bucket[], unit: 'hour' | 'day'): { start: string; b: Bucket | null }[] {
	if (!buckets.length) return [];
	const step = unit === 'hour' ? 3600_000 : 86400_000;
	const by = new Map(buckets.map((b) => [startMs(b.start), b]));
	const first = startMs(buckets[0].start), last = startMs(buckets[buckets.length - 1].start);
	const out: { start: string; b: Bucket | null }[] = [];
	for (let t = first; t <= last && out.length < 2000; t += step) {
		const iso = new Date(t).toISOString();
		out.push({ start: unit === 'hour' ? `${iso.slice(0, 13)}:00Z` : iso.slice(0, 10), b: by.get(t) ?? null });
	}
	return out;
}

/** A duration in seconds, in words: "no time", "under 1 s", "42 s", "3 min", "1.5 h". */
export function fmtDuration(s: number): string {
	if (!(s > 0)) return 'no time';
	if (s < 1) return 'under 1 s';
	if (s < 60) return `${Math.round(s)} s`;
	if (s < 3600) {
		const m = s / 60;
		return `${m < 10 ? Math.round(m * 10) / 10 : Math.round(m)} min`;
	}
	return `${Math.round((s / 3600) * 10) / 10} h`;
}

/** Utilisation at or above this, or a p95 wait for a slot at or above WAIT_LIMIT_S, says the GPU slots are the limit. */
export const BUSY_LIMIT = 0.8;
export const WAIT_LIMIT_S = 30;

// The runners' limits, from the runner profile (research prove2me NOTES, "Runner profile" 2026-11-18, and
// docs/reviews/2026-11-19-runner-scaling.md; a 7.6 GB runner with one Lean build slot):
/** Runner memory: evicted file pages read again, per second, averaged over runner-minutes. A warm Lean start re-reads
 * ~32k pages (about 530/s over a minute with one start a minute; ~1,200/s with 1 GB less page cache, still fine); two
 * checks at once thrashed at ~3,800/s (p2m-check 8 s -> 38 s) and a start with 2 GB less re-read 2.7 M pages. 2,000/s
 * lies between the fine minutes and the measured thrash. */
export const RUNNER_REFAULTS_LIMIT = 2000;
/** Runner CPU: share busy, averaged over runner-minutes. With one Lean slot a check keeps one of the two vCPUs busy; at
 * 85% on average the other is saturated by the sessions too, so checks wait for CPU and not only for the slot. */
export const RUNNER_CPU_LIMIT = 0.85;
/** The Lean build slot: p95 wait for it. The profile's queue model puts the knee at ~0.9 slot utilisation, where the
 * mean wait is ~25 s per call (6 sessions per runner); at 30 s p95 the slot holds the sessions back. */
export const LEAN_WAIT_LIMIT_S = 30;
/** Runner disk: free space at the lowest. The 50 GB volume holds the 10.6 GB Lean env, parts during extraction and the
 * task trees; under 5 GB the next env or a large task tree fails. */
export const RUNNER_FREE_LIMIT_GB = 5;

export type RunnerLimit = 'memory' | 'cpu' | 'lean' | 'disk';

/** What limits the runners, by the thresholds above; empty when nothing does. */
export function runnerLimits(r: RunnerStats): RunnerLimit[] {
	const out: RunnerLimit[] = [];
	if (r.refaultsS.avg >= RUNNER_REFAULTS_LIMIT) out.push('memory');
	if (r.cpu.avg >= RUNNER_CPU_LIMIT) out.push('cpu');
	if (r.lean.wait && r.lean.wait.p95 >= LEAN_WAIT_LIMIT_S) out.push('lean');
	if (r.freeGbLow < RUNNER_FREE_LIMIT_GB) out.push('disk');
	return out;
}

const LIMIT_NAME: Record<RunnerLimit | 'gpu', [string, boolean]> = {
	gpu: ['GPU slots', true], memory: ['runner memory', false], cpu: ['runner CPU', false], lean: ["the runners' Lean slots", true],
	disk: ['runner disk', false],
};
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const and = (xs: string[]) => (xs.length < 2 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs[xs.length - 1]}`);
const gb = (v: number) => (v < 10 ? Math.round(v * 10) / 10 : Math.round(v)).toLocaleString('en');

/** The runners' clause: CPU, the memory they lacked (when they did), the Lean slot's wait, low disk. */
function runnerClause(r: RunnerStats, limits: RunnerLimit[]): string {
	const bits = [`runners were ${Math.round(r.cpu.avg * 100)}% busy (CPU)`];
	if (limits.includes('memory')) {
		bits.push(`re-read ${gb((r.refaultsS.avg * 4) / 1024)} MB/s from disk that memory could not keep (${gb(r.pageCacheGb.avg)} GB of files cached on average)`);
	}
	if (r.lean.wait) bits.push(r.lean.wait.p95 > 0 ? `Lean checks waited p95 ${fmtDuration(r.lean.wait.p95)} for a slot` : 'no Lean check waited for a slot');
	if (limits.includes('disk')) bits.push(`only ${gb(r.freeGbLow)} GB of disk free at the lowest`);
	return bits.join(', ');
}

/** "What limits us now", in one plain sentence from the last 24 hours; null without samples. */
export function limitLine(s: Bucket | null): string | null {
	if (!s || s.samples <= 0) return null;
	if (s.utilisation === null || s.slots.avg <= 0) return 'Nodes are the limit now: no GPU slots were online in the last 24 h.';
	const u = Math.round(s.utilisation * 100);
	let worst: { k: Klass; p95: number } | null = null;
	for (const k of CLASSES) {
		const d = s.wait[k];
		if (d && (!worst || d.p95 > worst.p95)) worst = { k, p95: d.p95 };
	}
	const gpu = s.utilisation >= BUSY_LIMIT || (worst !== null && worst.p95 >= WAIT_LIMIT_S);
	const parts = [s.runner ? `GPU slots were ${u}% busy in the last 24 h` : `they were ${u}% busy in the last 24 h`];
	if (worst) {
		parts.push(worst.p95 > 0 ? `${CLASS_LABEL[worst.k]} waited p95 ${fmtDuration(worst.p95)} for a slot` : 'no request waited for one');
	}
	const refused = s.outcomes.refused ?? 0;
	if (refused > 0) {
		const all = Object.values(s.outcomes).reduce((a, b) => a + b, 0);
		parts.push(`${refused.toLocaleString('en')} of ${all.toLocaleString('en')} request${all === 1 ? '' : 's'} found no node to take ${refused === 1 ? 'it' : 'them'}`);
	}
	if (!s.runner) return `${gpu ? 'GPU slots are the limit now' : 'GPU slots are not the limit now'}: ${parts.join('; ')}.`;
	const rl = runnerLimits(s.runner);
	const names = [...(gpu ? (['gpu'] as const) : []), ...rl].map((k) => LIMIT_NAME[k]);
	const head = names.length === 0 ? 'Neither GPU slots nor runners are the limit now'
		: `${cap(and(names.map(([n]) => n)))} ${names.length > 1 || names[0][1] ? 'are' : 'is'} the limit now`;
	parts.push(runnerClause(s.runner, rl));
	return `${head}: ${parts.join('; ')}.`;
}

/** The range in words for the section's note: "the last 48 hours", "the last 7 days" (hours beyond two days), "the last
 * 30 days". */
export function rangeWords(unit: 'hour' | 'day', n: number): string {
	if (n <= 1) return 'so far';
	if (unit === 'hour' && n > 48) return `the last ${Math.round(n / 24)} days`;
	return `the last ${n} ${unit === 'hour' ? 'hours' : 'days'}`;
}

/** A bucket's label for a tooltip: "18 Nov 13:00 UTC" or "18 Nov". */
export function bucketLabel(start: string): string {
	const d = new Date(startMs(start));
	if (Number.isNaN(d.getTime())) return start;
	const day = `${d.getUTCDate()} ${['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getUTCMonth()]}`;
	return start.length === 10 ? day : `${day} ${String(d.getUTCHours()).padStart(2, '0')}:00 UTC`;
}
