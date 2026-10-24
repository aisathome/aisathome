// @ts-check
// Website for aisathome.org: landing page and docs (Astro + Starlight, static output).
// Build: see README.md. SPDX-License-Identifier: Apache-2.0
import { defineConfig, passthroughImageService } from 'astro/config';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';
import { readFileSync } from 'node:fs';

// Shares the theme choice with the account pages (see the file); inlined before Starlight's
// theme script. The file's leading comment stays in the repo, not in every page.
const themeSync = readFileSync(new URL('./src/scripts/theme-sync.js', import.meta.url), 'utf8')
	.replace(/^(\/\/.*\n)+/, '')
	.trim();

export default defineConfig({
	site: `https://${process.env.PUBLIC_DOMAIN || 'aisathome.org'}`,
	// No image processing (no native sharp dependency); the site has almost no images.
	image: { service: passthroughImageService() },
	integrations: [
		starlight({
			title: 'AIs@home',
			description:
				'Open AI models on volunteers’ GPUs, in the spirit of BOINC. Non-commercial and open source.',
			logo: { src: './src/assets/logo.svg', alt: 'AIs@home' },
			favicon: '/favicon.svg',
			head: [
				{ tag: 'script', content: themeSync },
				{
					tag: 'link',
					attrs: { rel: 'alternate', type: 'application/rss+xml', title: 'AIs@home dev log', href: '/devlog/rss.xml' },
				},
			],
			// global.css wires Tailwind 4 and Starlight to one theme (fonts, accent, grays).
			customCss: ['./src/styles/global.css', './src/styles/brand.css'],
			components: {
				SocialIcons: './src/components/HeaderLinks.astro',
				Footer: './src/components/Footer.astro',
				Hero: './src/components/Hero.astro',
				PageTitle: './src/components/PageTitle.astro',
			},
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/aisathome/aisathome' },
			],
			editLink: { baseUrl: 'https://github.com/aisathome/aisathome/edit/main/site/' },
			sidebar: [
				{ label: 'Docs overview', slug: 'docs' },
				{
					label: 'Use it',
					items: [
						{ label: 'Chat', slug: 'use/chat' },
						{ label: 'API and opencode', slug: 'use/api' },
					],
				},
				{
					label: 'Run a node',
					items: [
						{ label: 'Overview', slug: 'node' },
						{ label: 'Install on Linux', slug: 'node/install' },
						{ label: 'Model settings', slug: 'node/presets' },
						{ label: 'Day to day', slug: 'node/operate' },
						{ label: 'Update your node', slug: 'node/update' },
						{ label: 'About the binaries', slug: 'node/binaries' },
						{ label: 'Credit', slug: 'node/credit' },
					],
				},
				{
					label: 'Science',
					items: [{ label: 'Tau Ceti', link: '/science/tauceti/' }],
				},
				{
					label: 'About',
					items: [
						{ label: 'Mission', slug: 'about' },
						{ label: 'FAQ', slug: 'about/faq' },
						{ label: 'Privacy', slug: 'about/privacy' },
						{ label: 'Terms of use', slug: 'about/terms' },
					],
				},
			],
		}),
	],
	vite: { plugins: [tailwindcss()] },
});
