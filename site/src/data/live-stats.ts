// The live network strip's numbers (components/LiveStats.astro). Pure, unit-tested (tests/live-stats.test.ts).
// SPDX-License-Identifier: Apache-2.0

const plain = new Intl.NumberFormat('en', { maximumFractionDigits: 0 });
const short = new Intl.NumberFormat('en', { notation: 'compact', minimumSignificantDigits: 3, maximumSignificantDigits: 3 });

/** Under 1000 as is (2, 999); from 1000 on short with three digits (owner, 2026-11-19): 1.00K, 19.7K, 1.25B, 12.5B, 125B. */
export const liveNumber = (n: number) => (n < 1000 ? plain : short).format(n);
