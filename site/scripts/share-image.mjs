// Renders the share image (public/og.png, 1200 x 630, the og:image of every page) and the square
// logo (public/logo-512.png, the Organization logo in the home page's JSON-LD) with headless
// Chrome, from the site's own fonts (node_modules/@fontsource-variable) and colours. The PNGs are
// committed; run this after changing the design or the copy below, then commit them:
//   npm ci && node scripts/share-image.mjs     (CHROME=/path/to/chrome if it is not google-chrome)
// The page's og:image tags are in astro.config.mjs; npm run check reads the PNG's size from its header.
// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const site = new URL('..', import.meta.url);
const font = (pkg, file) => new URL(`node_modules/@fontsource-variable/${pkg}/files/${file}`, site).href;
const chrome = process.env.CHROME || 'google-chrome';

// The dark theme (src/styles/global.css, brand.css) and the logo's own orange (src/assets/logo.svg).
const css = `
@font-face { font-family: 'Display'; font-weight: 100 900; src: url('${font('big-shoulders-display', 'big-shoulders-display-latin-wght-normal.woff2')}') format('woff2'); }
@font-face { font-family: 'Sans'; font-weight: 100 900; src: url('${font('public-sans', 'public-sans-latin-wght-normal.woff2')}') format('woff2'); }
@font-face { font-family: 'Mono'; font-weight: 100 800; src: url('${font('jetbrains-mono', 'jetbrains-mono-latin-wght-normal.woff2')}') format('woff2'); }
* { margin: 0; box-sizing: border-box; }
html, body { width: var(--w); height: var(--h); overflow: hidden; background: #15110e; color: #fff8f0; }
.logo { display: grid; place-items: center; background: #c2410c; color: #fff8f0; font: 800 var(--at) / 1 'Mono'; }
`;

const homes = [[92, 92, 1], [280, 52, 0.9], [468, 96, 1], [500, 300, 0.85], [292, 404, 1], [72, 318, 0.9]];
const hub = { x: 280, y: 228 };
const network = `
<svg viewBox="0 0 560 460" width="440" height="361" style="overflow: visible">
	<defs><symbol id="home" viewBox="0 0 100 100">
		<rect x="64" y="14" width="10" height="20" fill="#fff8f0" />
		<path d="M16 46 L50 16 L84 46 V88 H16 Z" fill="#1f1a16" stroke="#fff8f0" stroke-width="3.5" stroke-linejoin="round" />
		<rect x="26" y="54" width="48" height="22" rx="3" fill="#ff9842" />
		<circle cx="40" cy="65" r="6.5" fill="#15110e" /><circle cx="60" cy="65" r="6.5" fill="#15110e" />
	</symbol></defs>
	${homes.map(([x, y]) => `<line x1="${hub.x}" y1="${hub.y}" x2="${x}" y2="${y}" stroke="#ff9842" stroke-width="2.5" stroke-dasharray="2 9" stroke-linecap="round" />`).join('')}
	<circle cx="${hub.x}" cy="${hub.y}" r="74" fill="none" stroke="#4b423a" stroke-dasharray="4 6" />
	${homes.map(([x, y, s]) => `<use href="#home" x="${x - 50 * s}" y="${y - 54 * s}" width="${100 * s}" height="${100 * s}" />`).join('')}
	<circle cx="${hub.x}" cy="${hub.y}" r="50" fill="#ff9842" />
	<text x="${hub.x}" y="${hub.y + 15}" text-anchor="middle" style="font: 800 46px 'Mono'; fill: #15110e">@</text>
</svg>`;

const share = `<!doctype html><meta charset="utf-8"><style>${css}
:root { --w: 1200px; --h: 630px; --at: 40px; }
body { display: grid; grid-template-columns: 1fr 440px; align-items: center; padding: 0 48px 0 72px; gap: 32px;
	background: radial-gradient(circle at 78% 50%, #22180f 0, #15110e 62%); }
.brand { display: flex; align-items: center; gap: 18px; font: 800 52px / 1 'Display'; letter-spacing: 0.01em; }
.brand .logo { width: 64px; height: 64px; border-radius: 14px; }
h1 { margin-top: 40px; font: 900 94px / 0.92 'Display'; text-transform: uppercase; letter-spacing: -0.005em; }
h1 em { font-style: normal; color: #ff9842; }
h1 span, p { white-space: nowrap; }
p { margin-top: 36px; font: 600 25px / 1.3 'Sans'; color: #d8cdc1; }
p b { color: #ffc896; font-weight: 600; }
</style>
<body><div>
	<div class="brand"><div class="logo">@</div>AIs@home</div>
	<h1><span>Open-weight</span> AI on <em>volunteers’</em> GPUs</h1>
	<p>Non-commercial · Open source · <b>aisathome.org</b></p>
</div>${network}</body>`;

// The favicon's design (src/assets/logo.svg), large: the rounded square on a transparent ground.
const logo = `<!doctype html><meta charset="utf-8"><style>${css}
:root { --w: 512px; --h: 512px; --at: 290px; }
html, body { background: transparent; }
.logo { width: 512px; height: 512px; border-radius: 112px; padding-bottom: 24px; }
</style><body><div class="logo">@</div></body>`;

const tmp = mkdtempSync(join(tmpdir(), 'aisathome-share-'));
try {
	for (const [name, page, w, h] of [['og.png', share, 1200, 630], ['logo-512.png', logo, 512, 512]]) {
		const src = join(tmp, name.replace('.png', '.html'));
		writeFileSync(src, page);
		const out = new URL(`public/${name}`, site).pathname;
		// A throwaway profile (never the user's own Chrome); the virtual time budget lets the fonts load first.
		execFileSync(chrome, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--no-first-run', '--no-default-browser-check',
			`--user-data-dir=${join(tmp, 'profile')}`, '--force-device-scale-factor=1', `--window-size=${w},${h}`,
			'--virtual-time-budget=5000', '--default-background-color=00000000', `--screenshot=${out}`, pathToFileURL(src).href], { stdio: 'inherit' });
		console.log(`wrote public/${name}`);
	}
} finally {
	rmSync(tmp, { recursive: true, force: true });
}
