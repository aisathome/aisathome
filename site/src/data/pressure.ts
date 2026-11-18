// The network page's "Pressure" section: the gateway's GET /v1/stats/pressure (gateway pressure.go), checked and put
// in words. Pure functions, unit-tested (tests/pressure.test.ts); the page draws the charts (pressure-chart.ts).
// Aggregates only: the endpoint has no node, operator, project or user, and nothing here adds one.
// SPDX-License-Identifier: Apache-2.0

export const CLASSES = ['chat', 'agent', 'science'] as const;
export type Klass = (typeof CLASSES)[number];
export const CLASS_LABEL: Record<Klass, string> = { chat: 'chat', agent: 'API and agents', science: 'science' };

export interface Gauge { avg: number; peak: number }
export interface Dur { count: number; p50: number; p95: number }
export interface Bucket {
	start: string; // "2026-11-18T13:00Z" (an hour) or "2026-11-18" (a day); "" for a sum
	samples: number;
	utilisation: number | null; // 0..1; null without slots online
	slots: Gauge;
	busy: Gauge;
	queue: { people: Gauge; science: Gauge };
	scienceSlots: Gauge;
	openTasks: Gauge;
	demand: Gauge;
	wait: Partial<Record<Klass, Dur>>;
	firstToken: Partial<Record<Klass, Dur>>;
	outcomes: Record<string, number>;
	holds: number;
}
export interface PressureView {
	bucket: 'hour' | 'day';
	buckets: Bucket[];
	last24h: Bucket;
	updated: Date;
}

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
	return {
		start: typeof o.start === 'string' ? o.start : '',
		samples: o.samples,
		utilisation: num(u) ? Math.min(u, 1) : null,
		slots: g.slots!, busy: g.busy!, queue: { people: g.people!, science: g.science! },
		scienceSlots: g.scienceSlots!, openTasks: g.openTasks!, demand: g.demand!,
		wait: durs(o.wait_s), firstToken: durs(o.first_token_s), outcomes, holds: num(o.holds) ? o.holds : 0,
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
	return { bucket: o.bucket, buckets, last24h, updated };
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
	const limited = s.utilisation >= BUSY_LIMIT || (worst !== null && worst.p95 >= WAIT_LIMIT_S);
	const parts = [`they were ${u}% busy in the last 24 h`];
	if (worst) {
		parts.push(worst.p95 > 0 ? `${CLASS_LABEL[worst.k]} waited p95 ${fmtDuration(worst.p95)} for a slot` : 'no request waited for one');
	}
	const refused = s.outcomes.refused ?? 0;
	if (refused > 0) {
		const all = Object.values(s.outcomes).reduce((a, b) => a + b, 0);
		parts.push(`${refused.toLocaleString('en')} of ${all.toLocaleString('en')} request${all === 1 ? '' : 's'} found no node to take ${refused === 1 ? 'it' : 'them'}`);
	}
	return `${limited ? 'GPU slots are the limit now' : 'GPU slots are not the limit now'}: ${parts.join('; ')}.`;
}

/** A bucket's label for a tooltip: "18 Nov 13:00 UTC" or "18 Nov". */
export function bucketLabel(start: string): string {
	const d = new Date(startMs(start));
	if (Number.isNaN(d.getTime())) return start;
	const day = `${d.getUTCDate()} ${['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getUTCMonth()]}`;
	return start.length === 10 ? day : `${day} ${String(d.getUTCHours()).padStart(2, '0')}:00 UTC`;
}
