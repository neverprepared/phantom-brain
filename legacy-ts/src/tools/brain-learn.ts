/**
 * brain_learn — Phase 0 manual ingest.
 *
 * Accepts a single item (content + title + filename) or a batch via items[].
 * Each item is written to Raw/curated/ and queued for synthesis.
 * Duplicate detection by SHA256 — re-submitting the same content is a no-op.
 */
import { z } from 'zod';
import fs from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CONFIG } from '../config.js';
import { writeAtomicFile } from '../vault/filesystem.js';
import { slugFromTitle } from '../vault/naming.js';
import { readProvenance, isHashKnown, type ProvenanceMap } from '../vault/provenance.js';
import { enqueueItem, type QueueItem } from '../vault/queue.js';
import { todayDateString, nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

const LearnItemSchema = z.object({
  content: z.string().describe('Raw document content to ingest'),
  title: z.string().describe('Human-readable title for the source'),
  filename: z.string().describe('Original filename hint, e.g. "karpathy-wiki.md"'),
  source_url: z.string().optional().describe('URL the source came from, if known'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('markdown'),
});

export const BrainLearnSchema = z.object({
  content: z.string().optional().describe('Raw document content (single-item mode)'),
  title: z.string().optional().describe('Human-readable title (single-item mode)'),
  filename: z.string().optional().describe('Original filename hint (single-item mode)'),
  source_url: z.string().optional().describe('URL the source came from, if known'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('markdown'),
  items: z.array(LearnItemSchema).min(1).max(100).optional().describe(
    'Batch mode: up to 100 items. When provided, top-level content/title/filename are ignored.'
  ),
}).refine(
  (d) => d.items !== undefined || (d.content !== undefined && d.title !== undefined && d.filename !== undefined),
  { message: 'Provide items[] for batch mode, or content + title + filename for single mode' },
);

export const brainLearnToolDefinition = {
  name: 'brain_learn',
  description:
    'Ingest one or more raw documents into the brain. Single mode: pass content + title + filename. ' +
    'Batch mode: pass items[] (up to 100). Each item is written to Raw/curated/ and queued for synthesis. ' +
    'Duplicate detection by SHA256 — re-submitting the same content is a no-op.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      content: { type: 'string', description: 'Raw document content (single-item mode)' },
      title: { type: 'string', description: 'Human-readable title (single-item mode)' },
      filename: { type: 'string', description: 'Original filename hint, e.g. "karpathy-wiki.md"' },
      source_url: { type: 'string', description: 'URL the source came from, if known' },
      format: {
        type: 'string',
        enum: ['markdown', 'html', 'text'],
        description: 'Source format (default markdown)',
      },
      items: {
        type: 'array',
        description: 'Batch mode: up to 100 items. When provided, top-level fields are ignored.',
        maxItems: 100,
        items: {
          type: 'object',
          properties: {
            content: { type: 'string' },
            title: { type: 'string' },
            filename: { type: 'string' },
            source_url: { type: 'string' },
            format: { type: 'string', enum: ['markdown', 'html', 'text'] },
          },
          required: ['content', 'title', 'filename'],
        },
      },
    },
  },
};

function extensionForFormat(format: 'markdown' | 'html' | 'text'): string {
  switch (format) {
    case 'markdown': return 'md';
    case 'html': return 'html';
    case 'text': return 'txt';
  }
}

function sha256(content: string): string {
  return createHash('sha256').update(content).digest('hex');
}

async function ingestOne(
  item: z.infer<typeof LearnItemSchema>,
  provenance: ProvenanceMap,
): Promise<{ status: 'queued' | 'duplicate'; raw_path?: string; content_hash: string; title: string }> {
  const { content, title, source_url, format } = item;
  const contentHash = sha256(content);

  if (await isHashKnown(contentHash, provenance)) {
    return { status: 'duplicate', content_hash: contentHash, title };
  }

  const slug = slugFromTitle(title);
  const ext = extensionForFormat(format);
  const date = todayDateString();
  const curatedDir = path.join(CONFIG.VAULT_PATH, CONFIG.RAW_CURATED);

  let candidate = `${date}-${slug}`;
  let counter = 2;
  let targetPath = path.join(curatedDir, `${candidate}.${ext}`);
  while (true) {
    try {
      await fs.stat(targetPath);
      candidate = `${date}-${slug}-${counter}`;
      targetPath = path.join(curatedDir, `${candidate}.${ext}`);
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') break;
      throw err;
    }
  }

  const rawPathRel = path.posix.join(CONFIG.RAW_CURATED, `${candidate}.${ext}`);
  await writeAtomicFile(targetPath, content);

  const queueItem: QueueItem = {
    raw_path: rawPathRel,
    source: 'curated',
    captured_at: nowISO(),
    title,
    ...(source_url !== undefined && { source_url }),
    format,
    content_hash: contentHash,
  };

  await enqueueItem(queueItem);
  logger.info('brain_learn ingested document', { raw_path: rawPathRel, content_hash: contentHash });

  return { status: 'queued', raw_path: rawPathRel, content_hash: contentHash, title };
}

export async function runBrainLearn(input: z.infer<typeof BrainLearnSchema>) {
  const provenance = await readProvenance();

  // Batch mode
  if (input.items) {
    const results = [];
    for (const item of input.items) {
      results.push(await ingestOne(item, provenance));
    }
    const queued = results.filter((r) => r.status === 'queued').length;
    const duplicates = results.filter((r) => r.status === 'duplicate').length;
    return {
      status: 'batch_complete' as const,
      queued,
      duplicates,
      results,
      message: `Batch complete: ${queued} queued, ${duplicates} duplicate(s). Call brain_synthesize to process.`,
    };
  }

  // Single mode
  const result = await ingestOne(
    {
      content: input.content!,
      title: input.title!,
      filename: input.filename!,
      source_url: input.source_url,
      format: input.format,
    },
    provenance,
  );

  if (result.status === 'duplicate') {
    return {
      status: 'duplicate' as const,
      content_hash: result.content_hash,
      message: 'This content has already been ingested (SHA256 match). No action taken.',
    };
  }

  return {
    status: 'queued' as const,
    raw_path: result.raw_path,
    content_hash: result.content_hash,
    message: `Stored to ${result.raw_path}. Queued for synthesis — call brain_synthesize to process.`,
  };
}

export async function handleBrainLearn(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainLearnSchema.parse(args);
    const result = await runBrainLearn(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_learn failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_learn: ${formatError(err)}` }],
      isError: true,
    };
  }
}
