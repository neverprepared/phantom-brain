/**
 * brain_learn — Phase 0 manual ingest.
 *
 * Drops a raw document into Raw/curated/ and queues it for synthesis.
 * Synthesis itself happens in brain_synthesize so the gate logic stays
 * separate from the storage write path.
 *
 * Dedup: if the same byte-for-byte content has already been ingested
 * (matched by SHA256 against provenance.json), we return early without
 * writing a duplicate Raw file or enqueueing a redundant job.
 */
import { z } from 'zod';
import fs from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CONFIG } from '../config.js';
import { writeAtomicFile } from '../vault/filesystem.js';
import { slugFromTitle } from '../vault/naming.js';
import { readProvenance, isHashKnown } from '../vault/provenance.js';
import { enqueueItem, type QueueItem } from '../vault/queue.js';
import { todayDateString, nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainLearnSchema = z.object({
  content: z.string().describe('Raw document content to ingest'),
  title: z.string().describe('Human-readable title for the source'),
  filename: z.string().describe('Original filename hint, e.g. "karpathy-wiki.md"'),
  source_url: z.string().optional().describe('URL the source came from, if known'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('markdown'),
});

export const brainLearnToolDefinition = {
  name: 'brain_learn',
  description:
    'Ingest a raw document into the brain. Writes the content to Raw/curated/ and queues it ' +
    'for synthesis. Duplicate detection by SHA256 — re-submitting the same content is a no-op. ' +
    'After learning, call brain_synthesize to process the queued item (or wait for the next ' +
    'scheduled run once that exists).',
  inputSchema: {
    type: 'object' as const,
    properties: {
      content: { type: 'string', description: 'Raw document content to ingest' },
      title: { type: 'string', description: 'Human-readable title for the source' },
      filename: { type: 'string', description: 'Original filename hint, e.g. "karpathy-wiki.md"' },
      source_url: { type: 'string', description: 'URL the source came from, if known' },
      format: {
        type: 'string',
        enum: ['markdown', 'html', 'text'],
        description: 'Source format (default markdown)',
      },
    },
    required: ['content', 'title', 'filename'],
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

export async function runBrainLearn(input: z.infer<typeof BrainLearnSchema>) {
  const { content, title, source_url, format } = input;

  const contentHash = sha256(content);

  // Dedup against provenance map before doing any I/O on disk.
  const provenance = await readProvenance();
  if (await isHashKnown(contentHash, provenance)) {
    return {
      status: 'duplicate' as const,
      content_hash: contentHash,
      message: 'This content has already been ingested (SHA256 match). No action taken.',
    };
  }

  // Build the Raw/curated filename: <YYYY-MM-DD>-<slug>.<ext>
  // deduplicateSlug in vault/naming.ts hardcodes the .md extension, so we
  // re-implement the same dedup loop here against the real target extension.
  // The provenance hash check above already eliminates same-content duplicates;
  // this loop only catches same-title-different-content collisions on a single day.
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

  const item: QueueItem = {
    raw_path: rawPathRel,
    source: 'curated',
    captured_at: nowISO(),
    title,
    ...(source_url !== undefined && { source_url }),
    format,
    content_hash: contentHash,
  };

  await enqueueItem(item);

  logger.info('brain_learn ingested document', { raw_path: rawPathRel, content_hash: contentHash });

  return {
    status: 'queued' as const,
    raw_path: rawPathRel,
    content_hash: contentHash,
    message: `Stored to ${rawPathRel}. Queued for synthesis — call brain_synthesize to process now, or it will be picked up on the next scheduled run.`,
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
