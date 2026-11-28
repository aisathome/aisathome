// The licence of the prove2me records the site publishes (the proved theorems' pages and JSON, /science/prove2me.json)
// and of the proofs AIs@home submitted: Apache License 2.0, as prove2me licenses every public contribution
// (https://prove2.me/terms), theorem statements and proofs included. One place for the JSON-LD, the JSON files and the
// line on the proved theorems' page.
// SPDX-License-Identifier: Apache-2.0
export const RECORDS_LICENSE_URL = 'https://www.apache.org/licenses/LICENSE-2.0';

export const recordsLicenceText =
	'The records on these pages and the proofs we submitted are published under the Apache License 2.0, as is everything public on prove2me; theorem statements belong to their prove2me authors, under the same licence.';

/** The `license` field of the published JSON files. */
export const recordsLicense = { spdx: 'Apache-2.0', url: RECORDS_LICENSE_URL, note: recordsLicenceText } as const;
