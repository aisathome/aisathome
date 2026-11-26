// Starlight route middleware (astro.config.mjs, routeMiddleware): adds what a docs page's frontmatter cannot
// generate. /node/ gets the node software's JSON-LD (src/data/seo.ts, nodeJsonLd), built from node-release.ts
// so its version follows the release. SPDX-License-Identifier: Apache-2.0
import { defineRouteMiddleware } from '@astrojs/starlight/route-data';
import { jsonLdScript, nodeJsonLd } from './data/seo';

export const onRequest = defineRouteMiddleware((context) => {
	const route = context.locals.starlightRoute;
	if (route.id === 'node') {
		route.head.push({ tag: 'script', attrs: { type: 'application/ld+json' }, content: jsonLdScript(nodeJsonLd()) });
	}
});
