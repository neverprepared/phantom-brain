import { formatError } from '../shared/errors.js';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { StoreInputSchema } from '../schemas/tools.js';
import type { Frontmatter } from '../schemas/frontmatter.js';
import { slugFromTitle, deduplicateSlug } from '../vault/naming.js';
import { serializeMemory } from '../vault/frontmatter.js';
import { memoryFilePath, writeMemoryFile, appendToDaily } from '../vault/filesystem.js';
import { indexEntry, getIndex } from '../vault/search.js';
import { buildRelatedSection, autoLinkRelated } from '../vault/links.js';
import { generateMemoryId, nowISO } from '../shared/utils.js';
import { CONFIG } from '../config.js';
import { scheduleClusterRebuild } from '../vault/cluster-index.js';
import { logger } from '../shared/logger.js';
import path from 'node:path';

export const storeToolDefinition = {
  name: 'memory_store',
  description:
    'Store a new memory in the second brain vault. Creates a markdown file with frontmatter in the Memory folder.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      title: { type: 'string', description: 'Memory title (1-200 chars)' },
      content: { type: 'string', description: 'Memory content in markdown' },
      lifecycle_status: {
        type: 'string',
        enum: ['active', 'reference', 'archive'],
        description:
          'Lifecycle status. "active" for time-bound/evolving content, "reference" for stable reference material, "archive" for inactive.',
      },
      tags: {
        type: 'array',
        items: { type: 'string' },
        description: 'Tags for categorization',
      },
      related: {
        type: 'array',
        items: { type: 'string' },
        description: 'Slugs of related memories to link via [[wiki-links]]',
      },
      confidence: {
        type: 'string',
        enum: ['low', 'medium', 'high'],
        description: 'How verified/certain this info is (default: medium)',
      },
      source: {
        type: 'string',
        enum: ['conversation', 'manual', 'import'],
        description: 'Origin of this memory (default: conversation)',
      },
      source_urls: {
        type: 'array',
        items: { type: 'string' },
        description: 'Original source URLs for re-validation',
      },
      ttl_days: {
        type: 'number',
        description: 'Days before this memory is considered stale (defaults by lifecycle: active=90, reference=180, archive=365)',
      },
    },
    required: ['title', 'content', 'lifecycle_status'],
  },
};

export async function handleStore(args: unknown): Promise<CallToolResult> {
  try {
    const input = StoreInputSchema.parse(args);
    const baseSlug = slugFromTitle(input.title);
    const dir = path.join(CONFIG.VAULT_PATH, CONFIG.MEMORY_FOLDER);
    const slug = await deduplicateSlug(dir, baseSlug);
    const id = generateMemoryId(slug);
    const now = nowISO();

    const frontmatter: Frontmatter = {
      id,
      title: input.title,
      lifecycle_status: input.lifecycle_status,
      tags: input.tags,
      created: now,
      updated: now,
      source: input.source,
      related: input.related,
      confidence: input.confidence,
      status: 'active',
      last_accessed: now,
      source_urls: input.source_urls,
      input_sources: [],
      wiki_refs: [],
      ...(input.ttl_days !== undefined && { ttl_days: input.ttl_days }),
      ...(input.deadline !== undefined && { deadline: input.deadline }),
    };

    let body = input.content;
    if (input.related.length > 0) {
      body += buildRelatedSection(input.related);
    }

    const fileContent = serializeMemory(frontmatter, body);
    const filePath = memoryFilePath(slug);

    await writeMemoryFile(filePath, fileContent);

    indexEntry(id, { frontmatter, filePath, slug }, body);

    // Schedule cluster index rebuild after store
    scheduleClusterRebuild(getIndex);

    // Auto-link to related memories by shared tags (bidirectional)
    const { linked: linkedSlugs, failed: failedLinks } = await autoLinkRelated(slug, filePath, input.tags);
    if (failedLinks.length > 0) {
      logger.warn('Some auto-links failed during store', { slug, failed: failedLinks });
    }

    // Append to daily note
    await appendToDaily(`- [[${slug}]] — ${input.title} (${input.lifecycle_status}, ${input.tags.join(', ')})`);

    logger.info('Stored memory', { id, slug, lifecycle_status: input.lifecycle_status, linkedCount: linkedSlugs.length });

    const linkedInfo = linkedSlugs.length > 0
      ? `\nLinked to: ${linkedSlugs.length} related memories`
      : '';

    return {
      content: [
        {
          type: 'text',
          text: `Stored memory: "${input.title}"\nID: ${id}\nPath: ${CONFIG.MEMORY_FOLDER}/${slug}.md\nTags: ${input.tags.join(', ') || 'none'}${linkedInfo}`,
        },
      ],
    };
  } catch (error) {
    logger.error('Failed to store memory', { error: String(error) });
    return {
      content: [
        {
          type: 'text',
          text: `Error storing memory: ${formatError(error)}`,
        },
      ],
      isError: true,
    };
  }
}
