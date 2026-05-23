import matter from 'gray-matter';
import yaml from 'js-yaml';
import type { Frontmatter } from '../schemas/frontmatter.js';
import { FrontmatterSchema, normalizeFrontmatter } from '../schemas/frontmatter.js';

// js-yaml's DEFAULT_SCHEMA parses ISO datetime strings as JS Date objects,
// which breaks Zod string validation. JSON_SCHEMA disables that behaviour.
const matterOpts = {
  engines: {
    yaml: {
      parse: (str: string) => yaml.load(str, { schema: yaml.JSON_SCHEMA }) as Record<string, unknown>,
      stringify: (obj: unknown) => yaml.dump(obj),
    },
  },
} as const;

export interface ParsedMemory {
  frontmatter: Frontmatter;
  content: string;
}

export function parseMemoryFile(raw: string, filePath?: string): ParsedMemory {
  const { data, content } = matter(raw, matterOpts);
  const frontmatter = normalizeFrontmatter(FrontmatterSchema.parse(data), filePath);
  return { frontmatter, content: content.trim() };
}

export function serializeMemory(frontmatter: Frontmatter, content: string): string {
  return matter.stringify(content, frontmatter, matterOpts);
}
