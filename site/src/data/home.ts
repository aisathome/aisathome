// Copy for the home page (src/pages/index.astro). The node block's command, version, sizes and
// driver minimums come from node-release.ts, so a release changes them in one place.
import { chatURL } from './site';

export const links = {
	chat: chatURL,
	node: '/node/',
	nodeAnchor: '#run-a-node',
	nodeLinux: '/node/install/',
	nodeWindows: '/node/windows/',
	nodeOperate: '/node/operate/',
	network: '/network/',
	privacy: '/about/privacy/',
	terms: '/about/terms/',
	docs: '/use/chat/',
	github: 'https://github.com/aisathome/aisathome',
	issues: 'https://github.com/aisathome/aisathome/issues',
	reddit: 'https://www.reddit.com/r/aisathome/',
	contact: 'contact@aisathome.org',
};

export const hero = {
	pill: 'Non-commercial · Open source · Early experiment',
	title: 'Open-weight AI on volunteers’ GPUs',
	lead:
		'Free chat and an OpenAI-compatible API on an open-weight model, answered by volunteers’ GPUs at home instead of a big company’s data centre. So that AI does not have to go through a handful of big companies.',
	privacyTitle: 'Hardened, not confidential',
	privacyText: 'The volunteer whose GPU answers you can technically read the conversation.',
	privacyLink: 'How your data is handled',
};

// The node block: what the one command does, in three steps (site/public/node.sh).
export const nodeBlock = {
	kicker: 'Give your GPU',
	title: 'Run a node with one command',
	intro:
		'No account needed to start. A node without an account does science only: it works on prove2me proofs, never on people’s chats. Link it to an account later and it serves chats too and earns credit (recognition, not money).',
	does: [
		'Downloads the latest release from GitHub and checks it against its SHA-256 sums.',
		'Checks your driver, card and disk, says what it will install, and asks before it changes anything (it needs sudo).',
		'Installs the node and the model for your card, tests its speed, and starts on science work.',
	],
};

export const steps = [
	{
		title: 'You ask',
		text: 'In the chat, through the API, or as a coding agent in opencode. Open-weight models and open-source software instead of a company’s closed service.',
	},
	{
		title: 'A volunteer’s GPU answers',
		text: 'A volunteer’s own GPU, at home, runs the model (currently Qwen3.8 27B) and writes the answer. While the network is small, the maintainer adds a few nodes of their own, some of them rented.',
	},
	{
		title: 'The answer comes back',
		text: 'Streamed back the same way. The gateway keeps usage numbers, never the text; only when a safety check stops or flags a node’s answer does it keep a short excerpt of that answer, for 14 days.',
	},
];

export const cards = [
	{
		title: 'Chat',
		href: '/use/chat/',
		text: 'Sign in with GitHub or Google and chat. Free, with a daily limit. Chats are temporary unless you save them.',
	},
	{
		title: 'API and opencode',
		href: '/use/api/',
		text: 'An OpenAI-compatible API with a personal key and a daily limit, for your own tools and coding agents.',
	},
	{
		title: 'The network',
		href: '/network/',
		text: 'Nodes online, the work they did day by day, and the credit volunteers earned.',
	},
];

export const guarantees = [
	{
		title: 'No open ports',
		text: 'The node only connects outward to the gateway. No router changes.',
	},
	{
		title: 'Sandboxed',
		text: 'The model runs as its own unprivileged user: no access to /home, a read-only system, no core dumps, no swap.',
	},
	{
		title: 'Users stay anonymous',
		text: 'Your node never learns who sent a request: no name, no email, no account id.',
	},
	{
		title: 'Open source',
		text: 'The node, the gateway and the installer are on GitHub under Apache-2.0; releases are built from the tagged source by GitHub Actions.',
	},
];
