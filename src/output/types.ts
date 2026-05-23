import { z } from 'zod';

export const OutputFrontmatterSchema = z.object({
  title: z.string().min(1),
  kind: z.enum(['article', 'report', 'deck']),
  status: z.enum(['draft', 'published', 'archived']).default('draft'),
  tags: z.array(z.string()).default([]),
  created: z.string(),
  updated: z.string(),
  published_at: z.string().optional(),
  sources: z.array(z.string()).default([]),  // atom slugs and Wiki/ paths cited
  format: z.enum(['markdown', 'pdf', 'slides']).default('markdown'),
  attachments: z.array(z.string()).default([]),
});

export type OutputFrontmatter = z.infer<typeof OutputFrontmatterSchema>;

export interface OutputEntry {
  relPath: string;       // relative to Output/ folder, e.g. 'articles/oauth-guide.md'
  subfolder: string;
  slug: string;
  frontmatter: OutputFrontmatter;
}
