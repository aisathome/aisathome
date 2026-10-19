import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

export const collections = {
	docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
	// The dev log (src/content/devlog/<number>-<slug from the title>.md, see src/data/devlog.ts),
	// numbered #1, #2, ... Only posts with
	// `publish: true` are built, unless DEVLOG_DRAFTS=1 (the staging deploy sets it).
	devlog: defineCollection({
		loader: glob({ pattern: '*.md', base: './src/content/devlog' }),
		schema: z.object({
			title: z.string(),
			number: z.number().int().positive(),
			date: z.coerce.date(),
			summary: z.string(),
			publish: z.boolean(),
		}),
	}),
};
