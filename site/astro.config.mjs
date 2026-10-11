// @ts-check
// Website for aisathome.org: landing page and docs (Astro + Starlight, static output).
// Build: see README.md. SPDX-License-Identifier: Apache-2.0
import { defineConfig, passthroughImageService } from 'astro/config';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';

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
			// global.css wires Tailwind 4 and Starlight to one theme (fonts, accent, grays).
			customCss: ['./src/styles/global.css', './src/styles/brand.css'],
			components: {
				SocialIcons: './src/components/HeaderLinks.astro',
				Footer: './src/components/Footer.astro',
				Hero: './src/components/Hero.astro',
			},
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/aisathome/aisathome' },
			],
			editLink: { baseUrl: 'https://github.com/aisathome/aisathome/edit/main/site/' },
			sidebar: [
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
						{ label: 'Presets', slug: 'node/presets' },
						{ label: 'Day to day', slug: 'node/operate' },
						{ label: 'About the binaries', slug: 'node/binaries' },
						{ label: 'Credit', slug: 'node/credit' },
					],
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
