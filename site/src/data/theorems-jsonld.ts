// Structured data (JSON-LD, schema.org) of the proved theorems, for search engines and AI answer engines: the list
// (/science/prove2me/theorems/) as a Dataset whose download is theorems.json, and each proof's page
// (/science/prove2me/theorems/<leaf>/) as a CreativeWork. Built from the same records the pages render, at build time
// (prove2me-theorems-build.ts); src/route-data.ts puts them in the pages' heads, so the pages themselves do not change.
//
// A proof's page is a CreativeWork, not a SoftwareSourceCode: the page describes the proof and links its statement on
// prove2me, but does not publish the Lean source. schema.org's author and creator are a Person or an Organization, so
// the creator is AIs@home, whose network ran the model; the description names the model that wrote the proof. Titles
// come from prove2me's users through the gateway: jsonLdScript (seo.ts) escapes every "<" when this goes in a script.
// The site states no licence for the records, so the Dataset names none.
// SPDX-License-Identifier: Apache-2.0
import { origin } from './seo';
import { THEOREMS_PATH, nameOf, theoremPath, type TheoremRow, type TheoremsView } from './prove2me-theorems';

/** prove2me's theorem pages, the only statement links the JSON-LD carries (as prove2me.ts's theoremURL). */
const statementURL = /^https:\/\/prove2\.me\/theorems\/[0-9a-f-]{36}$/;
const statement = (url?: string | null) => (url && statementURL.test(url) ? url : undefined);

const organization = (base: string) => ({ '@type': 'Organization', '@id': `${base}/#organization`, name: 'AIs@home', url: `${base}/` });
export const datasetId = (base = origin) => `${base}${THEOREMS_PATH}#dataset`;
export const THEOREMS_JSON = '/science/prove2me/theorems.json';

/** The list's Dataset: what the page and theorems.json hold, made by AIs@home from the gateway's public records. */
export function theoremsDatasetJsonLd(view: Pick<TheoremsView, 'updated'>, model: string, description: string, base = origin) {
	return {
		'@context': 'https://schema.org',
		'@type': 'Dataset',
		'@id': datasetId(base),
		name: `Lean theorems ${model} proved on prove2me`,
		description,
		url: `${base}${THEOREMS_PATH}`,
		creator: organization(base),
		isAccessibleForFree: true,
		keywords: ['Lean 4', 'formal proof', 'automated theorem proving', 'prove2me', 'open-weight model', model],
		// theorems.json's fields: the counts, a proof's GPU-hours and the tokens its tasks computed
		variableMeasured: ['accepted proofs', 'GPU-hours per proof', 'tokens computed per proof'],
		isBasedOn: 'https://prove2.me/',
		dateModified: view.updated,
		distribution: [{ '@type': 'DataDownload', encodingFormat: 'application/json', contentUrl: `${base}${THEOREMS_JSON}` }],
	};
}

/** One accepted leaf's proof: about the prove2me theorem, based on its statement there, dated when it was submitted. */
export function theoremJsonLd(
	t: Pick<TheoremRow, 'leaf' | 'kind' | 'theorem' | 'title' | 'url' | 'submitted_at' | 'reuses'>,
	model: string,
	description: string,
	base = origin,
) {
	const name = nameOf(t);
	const url = `${base}${theoremPath(t.leaf)}`;
	const reuse = t.kind === 'reuse';
	const basedOn = [statement(t.url), reuse ? statement(t.reuses?.url) : undefined].filter((u): u is string => !!u);
	return {
		'@context': 'https://schema.org',
		'@type': 'CreativeWork',
		'@id': `${url}#proof`,
		name: reuse ? `prove2me leaf closed by reuse: ${name}` : `Lean 4 proof of ${name}`,
		description,
		url,
		about: { '@type': 'Thing', name, alternateName: t.theorem, identifier: t.leaf, ...(statement(t.url) ? { url: statement(t.url) } : {}) },
		...(basedOn.length ? { isBasedOn: basedOn.length === 1 ? basedOn[0] : basedOn } : {}),
		creator: organization(base),
		dateCreated: t.submitted_at,
		keywords: reuse ? ['Lean 4', 'formal proof', 'prove2me'] : ['Lean 4', 'formal proof', 'prove2me', model],
		isAccessibleForFree: true,
		isPartOf: { '@id': datasetId(base) },
	};
}

const LIST_ID = THEOREMS_PATH.replace(/^\/|\/$/g, '');

/** The JSON-LD of a Starlight route (src/route-data.ts), by its id: the list's or a proof page's, else null. The
 * description is the page's own, so the two never differ. */
export async function theoremsJsonLdForRoute(id: string, description: string, base = origin) {
	if (id !== LIST_ID && !id.startsWith(`${LIST_ID}/`)) return null;
	const [{ buildTheorems }, { p2m }] = await Promise.all([import('./prove2me-theorems-build'), import('./prove2me')]);
	const { view, pages } = await buildTheorems();
	if (id === LIST_ID) return theoremsDatasetJsonLd(view, p2m.model, description, base);
	const t = pages.find((x) => id === `${LIST_ID}/${x.leaf}`);
	return t ? theoremJsonLd(t, p2m.model, description, base) : null;
}
