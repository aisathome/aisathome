// What the site tells search engines and AI answer engines, checked twice: on the functions that
// write it (tests/seo.test.ts) and on the built files (scripts/check-dist.mjs). Each function
// returns a list of problems, empty when all is well. SPDX-License-Identifier: Apache-2.0

const isURL = (v, origin) => typeof v === 'string' && (origin ? v.startsWith(`${origin}/`) : /^https:\/\//.test(v));

const graphOf = (data, p) => {
	if (data?.['@context'] !== 'https://schema.org') p.push('@context is not https://schema.org');
	const graph = Array.isArray(data?.['@graph']) ? data['@graph'] : [];
	return (t) => graph.filter((n) => n?.['@type'] === t);
};
// The project speaks for itself: no people (the maintainer's name stays off the page's data).
const namesAPerson = (data) => /"(author|founder|employee|member|contactPoint|@type":"Person)/.test(JSON.stringify(data ?? {}));

/** The home page's JSON-LD: an Organization and a WebSite. The node software is on /node/ (nodeJsonLdProblems). */
export function jsonLdProblems(data, origin) {
	const p = [];
	const byType = graphOf(data, p);
	for (const t of ['Organization', 'WebSite']) if (byType(t).length !== 1) p.push(`not exactly one ${t}`);
	if (byType('SoftwareApplication').length) p.push('a SoftwareApplication (it is described on /node/)');
	const [org] = byType('Organization');
	const [web] = byType('WebSite');
	if (org) {
		if (org.name !== 'AIs@home') p.push(`Organization.name is ${org.name}`);
		if (org.url !== `${origin}/`) p.push(`Organization.url is ${org.url}`);
		if (!isURL(org.logo?.url, origin) || !/\.(png|svg)$/.test(org.logo.url)) p.push('Organization.logo is not an image on the site');
		if (!org.sameAs?.includes('https://github.com/aisathome')) p.push('Organization.sameAs lacks the GitHub organisation');
	}
	if (web) {
		if (web.url !== `${origin}/` || web.name !== 'AIs@home') p.push('WebSite has the wrong name or url');
		if (web.publisher?.['@id'] !== org?.['@id']) p.push('WebSite.publisher is not the Organization');
	}
	if (namesAPerson(data)) p.push('names a person or contact');
	return p;
}

/** /node/'s JSON-LD: the node as a SoftwareApplication (@id /node/#software), published by the home page's
 * Organization (referred to by its @id). */
export function nodeJsonLdProblems(data, origin) {
	const p = [];
	const byType = graphOf(data, p);
	if (byType('SoftwareApplication').length !== 1) p.push('not exactly one SoftwareApplication');
	const [app] = byType('SoftwareApplication');
	if (app) {
		if (!app.name) p.push('SoftwareApplication has no name');
		if (app['@id'] !== `${origin}/node/#software`) p.push(`SoftwareApplication's @id is ${app['@id']}`);
		if (app.url !== `${origin}/node/` || app.installUrl !== `${origin}/node/`) p.push('SoftwareApplication.url or installUrl is not /node/');
		for (const os of ['Windows', 'Linux']) if (!String(app.operatingSystem).includes(os)) p.push(`SoftwareApplication.operatingSystem lacks ${os}`);
		if (!app.applicationCategory) p.push('SoftwareApplication has no applicationCategory');
		if (!String(app.downloadUrl).startsWith('https://github.com/aisathome/aisathome/releases/')) p.push(`SoftwareApplication.downloadUrl is ${app.downloadUrl}`);
		if (!/^\d+\.\d+\.\d+$/.test(app.softwareVersion ?? '')) p.push(`SoftwareApplication.softwareVersion is ${app.softwareVersion}`);
		else if (!String(app.releaseNotes).endsWith(`/releases/tag/v${app.softwareVersion}`)) p.push(`SoftwareApplication.releaseNotes is ${app.releaseNotes}`);
		if (!/16 GB/.test(app.softwareRequirements ?? '')) p.push('SoftwareApplication.softwareRequirements lacks the 16 GB');
		if (!/apache\.org\/licenses\/LICENSE-2\.0/.test(app.license ?? '')) p.push('SoftwareApplication.license is not Apache-2.0');
		const o = app.offers;
		if (o?.['@type'] !== 'Offer' || Number(o.price) !== 0 || !/^[A-Z]{3}$/.test(o.priceCurrency ?? '')) p.push('SoftwareApplication.offers is not a free Offer');
		if (app.publisher?.['@id'] !== `${origin}/#organization`) p.push('SoftwareApplication.publisher is not the Organization');
	}
	if (namesAPerson(data)) p.push('names a person or contact');
	return p;
}

/** /robots.txt: open to every crawler, AI crawlers included; only the gateway's paths are left out. */
export function robotsProblems(text, origin) {
	const p = [];
	const lines = text.split('\n').map((l) => l.replace(/#.*/, '').trim()).filter(Boolean);
	const agents = lines.filter((l) => /^user-agent:/i.test(l)).map((l) => l.split(':')[1].trim());
	if (agents.join() !== '*') p.push(`user agents ${agents} (one group for every crawler expected)`);
	for (const l of lines.filter((l) => /^disallow:/i.test(l))) {
		const path = l.slice(l.indexOf(':') + 1).trim();
		if (!['/account', '/v1/', '/stats/', '/__login'].includes(path)) p.push(`disallows ${path || '(nothing)'}`);
	}
	const maps = lines.filter((l) => /^sitemap:/i.test(l)).map((l) => l.slice(l.indexOf(':') + 1).trim());
	if (maps.join() !== `${origin}/sitemap-index.xml`) p.push(`sitemap ${maps}`);
	return p;
}

/** /llms.txt in the llmstxt.org shape (an H1, a quoted summary, H2 sections of links), with the site's own claims. */
export function llmsProblems(text, origin) {
	const p = [];
	const lines = text.split('\n');
	if (lines[0] !== '# AIs@home') p.push('does not start with "# AIs@home"');
	if (!lines.slice(1).find((l) => l.trim())?.startsWith('> ')) p.push('no "> " summary after the title');
	if ((text.match(/^# /gm) ?? []).length !== 1) p.push('more than one H1');
	const sections = text.split(/^## /m).slice(1);
	if (sections.length < 3) p.push('fewer than three sections');
	for (const s of sections) {
		const items = s.split('\n').slice(1).filter((l) => l.trim());
		if (!items.length || !items.every((l) => /^- \[[^\]]+\]\(https:\/\/[^)\s]+\): \S/.test(l))) p.push(`section "${s.split('\n')[0]}" is not a list of "- [title](url): about"`);
	}
	const links = [...text.matchAll(/\]\((https:\/\/[^)\s]+)\)/g)].map((m) => m[1]);
	for (const path of ['/node/', '/node/windows/', '/node/operate/', '/science/', '/science/prove2me/', '/use/api/', '/network/', '/devlog/', '/about/privacy/', '/about/terms/']) {
		if (!links.includes(origin + path)) p.push(`no link to ${path}`);
	}
	if (!links.includes('https://github.com/aisathome')) p.push('no link to GitHub');
	// What the site says: hardened, not confidential; an unsigned installer; credit is not money.
	if (/(?<!not )confidential/i.test(text.replace(/hardened, not confidential/gi, ''))) p.push('claims confidentiality');
	if (!/hardened, not confidential/i.test(text)) p.push('does not say "hardened, not confidential"');
	if (/(?<!not (code-)?)signed installer|installer is (code-)?signed/i.test(text)) p.push('claims a signed installer');
	if (!/Qwen3\.8 27B/.test(text) || !/16 GB/.test(text) || !/BOINC/.test(text) || !/not money/.test(text)) p.push('lacks the model, the 16 GB, BOINC or "not money"');
	return p;
}

/** A PNG's width and height from its IHDR chunk, or null when the bytes are not a PNG. */
export function pngSize(buf) {
	const sig = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
	if (buf.length < 24 || !sig.every((b, i) => buf[i] === b) || buf.toString('latin1', 12, 16) !== 'IHDR') return null;
	return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20) };
}
