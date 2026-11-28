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

// robots.txt (seo.ts): the crawlers named explicitly, and the training crawlers, which stay under "*": changing their
// policy is a decision of its own.
const PRIVATE = ['/account', '/v1/', '/stats/', '/__login'];
const SEARCH_CRAWLERS = ['Googlebot', 'Bingbot', 'OAI-SearchBot', 'ChatGPT-User', 'PerplexityBot', 'Perplexity-User', 'Claude-SearchBot', 'Claude-User', 'ClaudeBot'];
const TRAINING_CRAWLERS = ['GPTBot', 'Google-Extended', 'CCBot'];

/** robots.txt's groups (RFC 9309): consecutive User-agent lines, then their rules. */
export function robotsGroups(text) {
	const groups = [];
	let cur = null;
	for (const raw of text.split('\n')) {
		const l = raw.replace(/#.*/, '').trim();
		const m = l.match(/^([A-Za-z-]+)\s*:\s*(.*)$/);
		if (!m) continue;
		const [key, value] = [m[1].toLowerCase(), m[2].trim()];
		if (key === 'user-agent') {
			if (!cur || cur.rules.length) groups.push((cur = { agents: [], rules: [] }));
			cur.agents.push(value);
		} else if (key === 'allow' || key === 'disallow') {
			if (cur) cur.rules.push([key, value]);
		}
	}
	return groups;
}

/** /robots.txt: open to every crawler, AI crawlers included; only the gateway's paths are left out, the same in every
 * group; the search and answer engines' crawlers named; the training crawlers left to "*". */
export function robotsProblems(text, origin) {
	const p = [];
	const groups = robotsGroups(text);
	const named = (a) => groups.filter((g) => g.agents.some((x) => x.toLowerCase() === a.toLowerCase()));
	if (named('*').length !== 1) p.push('not exactly one group for every crawler (User-agent: *)');
	for (const a of SEARCH_CRAWLERS) if (named(a).length !== 1) p.push(`${a} is not named in exactly one group`);
	for (const a of TRAINING_CRAWLERS) if (named(a).length) p.push(`names ${a}: the training crawlers' policy is a decision of its own`);
	for (const g of groups) {
		const who = g.agents.join(', ');
		const disallows = g.rules.filter(([k]) => k === 'disallow').map(([, v]) => v);
		for (const path of disallows) if (!PRIVATE.includes(path)) p.push(`disallows ${path || '(nothing)'} for ${who}`);
		for (const path of PRIVATE) if (!disallows.includes(path)) p.push(`does not disallow ${path} for ${who}`);
		if (!g.rules.some(([k, v]) => k === 'allow' && v === '/')) p.push(`no "Allow: /" for ${who}`);
	}
	const lines = text.split('\n').map((l) => l.replace(/#.*/, '').trim());
	const maps = lines.filter((l) => /^sitemap:/i.test(l)).map((l) => l.slice(l.indexOf(':') + 1).trim());
	if (maps.join() !== `${origin}/sitemap-index.xml`) p.push(`sitemap ${maps}`);
	return p;
}

// ---- the proved theorems' JSON-LD (theorems-jsonld.ts)

const LIST = '/science/prove2me/theorems/';
const STATEMENT = /^https:\/\/prove2\.me\/theorems\/[0-9a-f-]{36}$/;
const isoTime = (v) => typeof v === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d(:\d\d(\.\d+)?)?(Z|[+-]\d\d:\d\d)$/.test(v) && !Number.isNaN(Date.parse(v));
const isOrg = (o, origin) => o?.['@type'] === 'Organization' && o.name === 'AIs@home' && o.url === `${origin}/` && o['@id'] === `${origin}/#organization`;
const text = (v, min = 1) => typeof v === 'string' && v.trim().length >= min;

/** The list's Dataset (Google's Dataset fields: name, a description of 50 to 5000 characters, url, creator,
 * distribution); no licence while the site states none for the records. */
export function datasetJsonLdProblems(d, origin) {
	const p = [];
	if (d?.['@context'] !== 'https://schema.org') p.push('@context is not https://schema.org');
	if (d?.['@type'] !== 'Dataset') p.push(`@type is ${d?.['@type']}, not Dataset`);
	if (!text(d?.name)) p.push('no name');
	if (!text(d?.description, 50) || d.description.length > 5000) p.push('description is not 50 to 5000 characters');
	if (d?.url !== `${origin}${LIST}`) p.push(`url is ${d?.url}`);
	if (d?.['@id'] !== `${origin}${LIST}#dataset`) p.push(`@id is ${d?.['@id']}`);
	if (!isOrg(d?.creator, origin)) p.push('creator is not the AIs@home Organization');
	const dist = Array.isArray(d?.distribution) ? d.distribution : [];
	if (!dist.some((x) => x?.['@type'] === 'DataDownload' && x.encodingFormat === 'application/json' && x.contentUrl === `${origin}/science/prove2me/theorems.json`)) p.push('no DataDownload of theorems.json');
	if (!isoTime(d?.dateModified)) p.push(`dateModified is ${d?.dateModified}`);
	if ('license' in (d ?? {})) p.push('names a licence the site does not state');
	if (namesAPerson(d)) p.push('names a person or contact');
	return p;
}

/** One proof page's CreativeWork: about the prove2me theorem, based on its statement, made by AIs@home, part of the list's
 * Dataset. */
export function proofJsonLdProblems(d, origin, leaf) {
	const p = [];
	const url = `${origin}${LIST}${leaf}/`;
	if (d?.['@context'] !== 'https://schema.org') p.push('@context is not https://schema.org');
	if (d?.['@type'] !== 'CreativeWork') p.push(`@type is ${d?.['@type']}, not CreativeWork`);
	if (!text(d?.name) || !text(d?.description, 50)) p.push('no name or a short description');
	if (d?.url !== url || d?.['@id'] !== `${url}#proof`) p.push(`url or @id is not ${url}`);
	if (d?.about?.['@type'] !== 'Thing' || !text(d.about.name) || d.about.identifier !== leaf) p.push('about is not the leaf\'s theorem');
	if (d?.about?.url !== undefined && !STATEMENT.test(d.about.url)) p.push(`about.url ${d.about.url} is not a prove2me theorem`);
	for (const u of [d?.isBasedOn ?? []].flat()) if (!STATEMENT.test(u)) p.push(`isBasedOn ${u} is not a prove2me theorem`);
	if (d?.about?.url && ![d?.isBasedOn ?? []].flat().includes(d.about.url)) p.push('isBasedOn lacks the statement');
	if (!isOrg(d?.creator, origin)) p.push('creator is not the AIs@home Organization');
	if (!isoTime(d?.dateCreated)) p.push(`dateCreated is ${d?.dateCreated}`);
	if (d?.isPartOf?.['@id'] !== `${origin}${LIST}#dataset`) p.push('isPartOf is not the list\'s Dataset');
	if (namesAPerson(d)) p.push('names a person or contact');
	return p;
}

/** Every application/ld+json script of a built page, parsed; a script that does not parse is an Error in the list. */
export function jsonLdScripts(html) {
	return [...html.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)].map((m) => {
		try {
			return JSON.parse(m[1]);
		} catch (e) {
			return e;
		}
	});
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
