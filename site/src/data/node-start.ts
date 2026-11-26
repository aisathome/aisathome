// The start of a node, as the home page (#run-a-node) and /node/ both show it (src/components/NodeStart.astro):
// the Linux one-liner and what it does, beside the Windows installer. One copy, so the two pages cannot
// offer different commands. The command, version, sizes and driver minimums come from node-release.ts.
// SPDX-License-Identifier: Apache-2.0
import { needs, repoURL } from './node-release';

// What site/public/node.sh does, in three steps. A step may link some of its words (`link.words`, found in
// `text`).
export const linuxSteps: { text: string; link?: { words: string; href: string } }[] = [
	{
		text: 'Downloads the latest release from GitHub and checks it against its SHA-256 sums.',
		link: { words: 'the latest release', href: `${repoURL}/releases/latest` },
	},
	{ text: 'Checks your driver, card and disk, says what it will install, and asks before it changes anything (it needs sudo).' },
	{ text: `Installs the node and the model for your card (${needs.modelGB} GB), tests its speed, and starts on science work.` },
];

// The picture of the node's own page (e2e/browser/node_page_image.py draws both, light and dark, from the
// real local page with made-up data): its top on the home page, down to its controls on /node/.
export const nodePageShots = {
	top: {
		src: (theme: 'light' | 'dark') => `/images/home/node-page-${theme}.webp`,
		width: 640,
		height: 448,
		alt: 'The node’s own page, with example data: Serving, connected to aisathome.org; without an account, admitted for science work, with the command to link it; one slot in use; model qwen3.8-27b.',
	},
	controls: {
		src: (theme: 'light' | 'dark') => `/images/home/node-page-controls-${theme}.webp`,
		width: 640,
		height: 945,
		alt: 'The node’s own page, with example data: Serving, connected to aisathome.org; without an account, admitted for science work, with the command to link it; one slot in use; model qwen3.8-27b; the GPU at 66 °C; requests and tokens today and in total; and the controls Pause, Restart engine, Stop and free the GPU, and Uninstall.',
	},
};
