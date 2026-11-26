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

const site = `https://${process.env.PUBLIC_DOMAIN || 'aisathome.org'}`;
// The share image of every page (scripts/share-image.mjs draws it; npm run check compares its size).
const shareImage = [
	['og:image', `${site}/og.png`],
	['og:image:type', 'image/png'],
	['og:image:width', '1200'],
	['og:image:height', '630'],
	['og:image:alt', 'AIs@home: open-weight AI on volunteers’ GPUs. Non-commercial, open source.'],
].map(([property, content]) => ({ tag: 'meta', attrs: { property, content } }));

export default defineConfig({
	site,
	// No image processing (no native sharp dependency); the site has almost no images.
	image: { service: passthroughImageService() },
	integrations: [
		starlight({
			title: 'AIs@home',
			description: 'Open-weight AI on volunteers’ GPUs. Non-commercial and open source.',
			logo: { src: './src/assets/logo.svg', alt: 'AIs@home' },
			favicon: '/favicon.svg',
			head: [
				{ tag: 'script', content: themeSync },
				{
					tag: 'link',
					attrs: { rel: 'alternate', type: 'application/rss+xml', title: 'AIs@home dev log', href: '/devlog/rss.xml' },
				},
				...shareImage,
				{ tag: 'meta', attrs: { name: 'twitter:image:alt', content: shareImage[4].attrs.content } },
			],
			// global.css wires Tailwind 4 and Starlight to one theme (fonts, accent, grays).
			customCss: ['./src/styles/global.css', './src/styles/brand.css'],
			// /node/'s JSON-LD (src/route-data.ts).
			routeMiddleware: './src/route-data.ts',
			components: {
				SocialIcons: './src/components/HeaderLinks.astro',
				Footer: './src/components/Footer.astro',
				Hero: './src/components/Hero.astro',
				PageTitle: './src/components/PageTitle.astro',
				Header: './src/components/Header.astro',
				Sidebar: './src/components/Sidebar.astro',
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
						{ label: 'Install on Windows', slug: 'node/windows' },
						{ label: 'Model settings', slug: 'node/presets' },
						{ label: 'Day to day', slug: 'node/operate' },
						{ label: 'Update your node', slug: 'node/update' },
						{ label: 'About the binaries', slug: 'node/binaries' },
						{ label: 'Credit', slug: 'node/credit' },
					],
				},
				{
					label: 'Science',
					items: [
						{ label: 'Overview', link: '/science/' },
						{ label: 'prove2me', link: '/science/prove2me/' },
						{ label: 'Tau Ceti', link: '/science/tauceti/' },
					],
				},
				{
					label: 'About',
					items: [
						{ label: 'Mission', slug: 'about' },
						{ label: 'FAQ', slug: 'about/faq' },
						{ label: 'Privacy', slug: 'about/privacy' },
						{ label: 'Terms of use', slug: 'about/terms' },
						{ label: 'Contribute', link: 'https://github.com/aisathome/aisathome/blob/main/CONTRIBUTING.md' },
					],
				},
			],
		}),
	],
	vite: { plugins: [tailwindcss()] },
});
