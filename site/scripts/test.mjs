// Unit tests of the site's pure TypeScript (src/data): every tests/*.test.ts, loaded through Vite (the same transform
// the build uses, so the tests import the modules as the pages do) and run by node:test. `npm test`; CI runs it.
// SPDX-License-Identifier: Apache-2.0
import { readdirSync } from 'node:fs';
import { createServer } from 'vite';

const root = new URL('..', import.meta.url).pathname;
const dir = new URL('../tests/', import.meta.url).pathname;
const files = readdirSync(dir).filter((f) => f.endsWith('.test.ts')).sort();
if (!files.length) {
	console.error('no tests in tests/');
	process.exit(1);
}
const vite = await createServer({
	root,
	configFile: false,
	logLevel: 'error',
	appType: 'custom',
	server: { middlewareMode: true, hmr: false, watch: null },
	optimizeDeps: { noDiscovery: true, include: [] },
});
try {
	// node:test runs the tests each file registers once the files are loaded
	for (const f of files) await vite.ssrLoadModule(`/tests/${f}`);
} finally {
	await vite.close();
}
