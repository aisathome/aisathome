// Starlight route middleware (astro.config.mjs, routeMiddleware): adds what a docs page's frontmatter cannot
// generate. /node/ gets the node software's JSON-LD (src/data/seo.ts, nodeJsonLd), built from node-release.ts
// so its version follows the release; the proved theorems' list and each proof's page get theirs
// (src/data/theorems-jsonld.ts), built from the records the pages render. SPDX-License-Identifier: Apache-2.0
import { defineRouteMiddleware } from '@astrojs/starlight/route-data';
import { jsonLdScript, nodeJsonLd } from './data/seo';
import { theoremsJsonLdForRoute } from './data/theorems-jsonld';

const ldScript = (data: unknown) => ({ tag: 'script' as const, attrs: { type: 'application/ld+json' }, content: jsonLdScript(data) });

export const onRequest = defineRouteMiddleware(async (context) => {
	const route = context.locals.starlightRoute;
	if (route.id === 'node') route.head.push(ldScript(nodeJsonLd()));
	const theorems = await theoremsJsonLdForRoute(route.id, route.entry.data.description ?? '');
	if (theorems) route.head.push(ldScript(theorems));
});
