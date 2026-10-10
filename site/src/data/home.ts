// Copy for the home page (src/pages/index.astro).
import { chatURL } from './site';

export const links = {
	chat: chatURL,
	node: '/node/',
	privacy: '/about/privacy/',
	terms: '/about/terms/',
	docs: '/use/chat/',
	github: 'https://github.com/aisathome/aisathome',
	discussions: 'https://github.com/aisathome/aisathome/discussions',
	contact: 'contact@aisathome.org',
};

export const hero = {
	title: 'Open AI models on volunteers’ GPUs',
	lead:
		'AIs@home runs open-weight AI models on GPUs that volunteers contribute, in the spirit of BOINC and Gridcoin: AI that does not have to go through a handful of big companies.',
	status: 'Early experiment. Non-commercial and open source. Things will break.',
	privacyTitle: 'Hardened, not confidential',
	privacyText: 'The volunteer whose GPU answers you can technically read the conversation.',
	privacyLink: 'How your data is handled',
};

export const steps = [
	{
		title: 'You ask',
		text: 'Get help from AI the way you are used to: in the chat, through the API, or as a coding agent in opencode. The difference: open-weight models and open-source software instead of a company’s closed service.',
	},
	{
		title: 'A volunteer’s GPU answers',
		text: 'Not a big company’s data centre: volunteers’ own GPUs, at home, run the model (currently Qwen3.8 27B) and write the answer. Anyone with a 24 GB NVIDIA GPU can join; while the network is small, the maintainer adds a few rented ones.',
	},
	{
		title: 'The answer comes back',
		text: 'Streamed back the same way. The gateway keeps usage numbers, never the content.',
	},
];

export const cards = [
	{
		title: 'Chat',
		href: '/use/chat/',
		text: 'Sign in with GitHub or Google and chat. Chats are temporary unless you save them.',
	},
	{
		title: 'API and opencode',
		href: '/use/api/',
		text: 'An OpenAI-compatible API with a personal key and a daily limit.',
	},
	{
		title: 'Run a node',
		href: '/node/',
		text: 'An NVIDIA GPU with 24 GB, Linux, about an hour. Voluntary and unpaid.',
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
		title: 'Stop any time',
		text: 'One command pauses the node and frees the GPU. Uninstalling removes everything except the model file, unless you ask for that too.',
	},
];

