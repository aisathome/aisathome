// What search engines and AI answer engines read: the home page's title and structured data
// (JSON-LD, schema.org), /robots.txt and /llms.txt (llmstxt.org). The share image's tags are in
// astro.config.mjs (every page); the image itself is drawn by scripts/share-image.mjs.
// Facts here must match the pages they summarise. SPDX-License-Identifier: Apache-2.0
import { domain } from './site';
import { anonCommand, needs, nodeRelease, repoURL } from './node-release';

export const origin = `https://${domain}`;
export const githubOrg = 'https://github.com/aisathome';
export const tagline = 'Open-weight AI on volunteers’ GPUs';
export const description = 'Open-weight AI on volunteers’ GPUs. Non-commercial and open source.';
// The home page's own description (search results show it under the title): what you can do there.
export const homeDescription =
	'Volunteer computing for open-weight LLMs, BOINC-style: Qwen3.8 27B on volunteers’ NVIDIA GPUs. Free chat and OpenAI-compatible API. Non-commercial, open source.';
// How the name is written where "@" cannot go (the domain, the GitHub organisation): for search engines
// and AI answers, in the JSON-LD and llms.txt.
export const alternateName = 'aisathome';
// The home page's <title> (others are "<page> | AIs@home", Starlight's own).
export const homeTitle = `AIs@home: open-weight AI on volunteers’ GPUs`;

// Served by the gateway, not the static site: kept out of search results.
export const privatePaths = ['/account', '/v1/', '/stats/', '/__login'];

/** The home page's schema.org graph: the project and its website. The node software is described on /node/
 * (nodeJsonLd), the page about it, once. */
export function homeJsonLd(base = origin) {
	const org = `${base}/#organization`;
	return {
		'@context': 'https://schema.org',
		'@graph': [
			{
				'@type': 'Organization',
				'@id': org,
				name: 'AIs@home',
				alternateName: alternateName,
				url: `${base}/`,
				logo: { '@type': 'ImageObject', url: `${base}/logo-512.png`, width: 512, height: 512 },
				description,
				sameAs: [githubOrg, 'https://www.reddit.com/r/aisathome/'],
			},
			{
				'@type': 'WebSite',
				'@id': `${base}/#website`,
				name: 'AIs@home',
				alternateName: alternateName,
				url: `${base}/`,
				description,
				inLanguage: 'en',
				publisher: { '@id': org },
			},
		],
	};
}

/** /node/'s schema.org graph: the node software (its @id is /node/#software), published by the project
 * (the Organization, defined on the home page and referred to by its @id). */
export function nodeJsonLd(base = origin) {
	const org = `${base}/#organization`;
	return {
		'@context': 'https://schema.org',
		'@graph': [
			{ '@type': 'Organization', '@id': org, name: 'AIs@home', url: `${base}/` },
			{
				'@type': 'SoftwareApplication',
				'@id': `${base}/node/#software`,
				name: 'AIs@home node',
				alternateName: 'aisathome-node',
				description:
					'Lets a volunteer’s NVIDIA GPU (16 GB or more) serve the open-weight model Qwen3.8 27B to AIs@home users and science projects. One command on Linux, an installer on Windows; no account needed to start. Voluntary and unpaid.',
				url: `${base}/node/`,
				installUrl: `${base}/node/`,
				downloadUrl: `${repoURL}/releases/latest`,
				operatingSystem: 'Linux, Windows 10, Windows 11',
				applicationCategory: 'UtilitiesApplication',
				softwareVersion: nodeRelease.version.replace(/^v/, ''),
				releaseNotes: `${repoURL}/releases/tag/${nodeRelease.version}`,
				softwareRequirements: `NVIDIA GPU with 16 GB of memory or more; NVIDIA driver ${needs.linuxDriver} or newer (Linux) or ${needs.windowsDriver} or newer (Windows)`,
				license: 'https://www.apache.org/licenses/LICENSE-2.0',
				isAccessibleForFree: true,
				offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' },
				publisher: { '@id': org },
				sameAs: repoURL,
			},
		],
	};
}

/** JSON for a <script type="application/ld+json">: "<" escaped so no text can close the script. */
export const jsonLdScript = (data: unknown) => JSON.stringify(data).replace(/</g, '\\u003c');

/** /robots.txt: everything public is open to every crawler, AI crawlers included. */
export function robotsTxt(base = origin) {
	return [
		'# aisathome.org: every crawler is welcome, search engines and AI crawlers alike (GPTBot, ClaudeBot,',
		'# PerplexityBot, Google-Extended, ...). Only the account pages and the gateway (API, statistics) are left out.',
		'# A summary for language models: /llms.txt',
		'User-agent: *',
		...privatePaths.map((p) => `Disallow: ${p}`),
		'Allow: /',
		'',
		`Sitemap: ${base}/sitemap-index.xml`,
		'',
	].join('\n');
}

/** /llms.txt (https://llmstxt.org): what AIs@home is, and where to read more. */
export function llmsTxt(base = origin) {
	const api = base.replace('https://', 'https://api.');
	const chat = base.replace('https://', 'https://chat.');
	const link = (title: string, path: string, about: string) => `- [${title}](${path.startsWith('http') ? path : base + path}): ${about}`;
	return `# AIs@home

> AIs@home (written aisathome in web addresses: aisathome.org) runs open-weight AI models on GPUs that volunteers
> contribute, in the spirit of BOINC, so that AI does not have to go through a handful of big companies. It is a
> non-commercial, open-source (Apache-2.0) experiment run by one volunteer maintainer: no company, no investors, no
> token, no ads.

Mission: open-weight models made it possible for anyone to run good AI; AIs@home tries to serve them from volunteers' GPUs
to people who do not have expensive GPUs themselves.

Facts:

- The model is Qwen3.8 27B, an open-weight model, quantised to fit one GPU (about 4 bits on 24 GB cards with 192k
  context, about 3 bits on 16 GB cards with 80k context).
- A node needs an NVIDIA GPU with 16 GB of memory or more, on Windows 10 or 11 (64-bit) or Linux with systemd.
- Running a node is voluntary and unpaid. Nodes earn credit in BOINC's unit (the cobblestone): recognition, not money;
  there is no token and no payment.
- Privacy: hardened, not confidential. The volunteer whose GPU answers can technically read the conversation. For each
  request the gateway keeps usage numbers, never the text; when a safety check stops or flags a node's answer, it keeps
  the part that triggered the check (at most 300 characters, known secret formats removed, admins only) for 14 days,
  which can quote the conversation. Chats are temporary unless you save them.
- Using the chat and the API is free, with a daily limit per account (sign in with GitHub or Google).
- The Windows installer is not code-signed yet.

## Use it

${link('Chat', chat, 'the chat (Open WebUI); sign in with GitHub or Google')}
${link('API and opencode', '/use/api/', `an OpenAI-compatible API at ${api}/v1 with a personal key, for your own tools and coding agents`)}

## Run a node

${link('Run a node', '/node/', `what a node needs and does. Linux: \`${anonCommand}\` (no account needed; science only until linked); Windows: one installer`)}
${link('Install on Windows', '/node/windows/', 'one installer for Windows 10 or 11, step by step')}
${link('Install on Linux', '/node/install/', 'the systemd install, step by step')}
${link('Day to day', '/node/operate/', 'stop, start, update and remove a node; its local page; troubleshooting')}
${link('Credit', '/node/credit/', 'how GPU time is counted in BOINC’s unit, and what credit is not')}

## Science

${link('Science', '/science/', 'what the open model on the network does for science: work a machine checks, every result counted')}
${link('prove2me', '/science/prove2me/', 'research theorems stated in Lean 4 that the model tries to prove; every run counts')}
${link('Tau Ceti', '/science/tauceti/', 'pull requests the model opened on a library of formal mathematics (stopped on 7 November 2026 at the maintainers’ request)')}

## Project

${link('Network', '/network/', 'nodes online and credit earned by volunteers, live')}
${link('Dev log', '/devlog/', 'notes from building AIs@home: what we tried, what it cost, what we learned')}
${link('Mission', '/about/', 'why AIs@home exists and who runs it')}
${link('FAQ', '/about/faq/', 'common questions: cost, privacy, the model, Windows and smaller GPUs, BOINC')}
${link('Privacy', '/about/privacy/', 'who handles your data and what a volunteer’s GPU can see')}
${link('Terms of use', '/about/terms/', 'terms for users and for node operators')}
${link('GitHub', githubOrg, 'the source code (Apache-2.0), issues and releases')}

## Optional

${link('Docs overview', '/docs/', 'every page of the documentation in one place')}
${link('Model settings', '/node/presets/', 'the two model files and their context on 16 GB and 24 GB cards')}
${link('About the binaries', '/node/binaries/', 'what is in a release, how it is built and how to check it')}
`;
}
