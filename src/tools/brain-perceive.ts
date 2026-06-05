/**
 * brain_perceive — gathered-source ingest (Phase 3).
 *
 * Accepts a single item or a batch via items[]. Writes to Raw/gathered/ and
 * queues for synthesis with source='gathered', routing through the LLM gate.
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

const PerceiveItemSchema = z.object({
  content: z.string().describe('Raw web content to ingest'),
  title: z.string().describe('Human-readable title (URL hostname or page title)'),
  source_url: z.string().optional().describe('URL the content was fetched from'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('html'),
});

export const BrainPerceiveSchema = z.object({
  content: z.string().optional().describe('Raw web content (single-item mode)'),
  title: z.string().optional().describe('Human-readable title (single-item mode)'),
  source_url: z.string().optional().describe('URL the content was fetched from'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('html'),
  items: z.array(PerceiveItemSchema).min(1).max(100).optional().describe(
    'Batch mode: up to 100 items. When provided, top-level content/title are ignored.'
  ),
}).refine(
  (d) => d.items !== undefined || (d.content !== undefined && d.title !== undefined),
  { message: 'Provide items[] for batch mode, or content + title for single mode' },
);

export const brainPerceiveToolDefinition = {
  name: 'brain_perceive',
  description:
    'Ingest one or more gathered web sources into the brain pipeline. Single mode: pass content + title. ' +
    'Batch mode: pass items[] (up to 100). Each item is written to Raw/gathered/ and queued for ' +
    'gate evaluation + synthesis. Pass full fetched content — do NOT summarize before ingesting. ' +
    'Duplicate content (SHA256 match) is a no-op.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      content: { type: 'string', description: 'Raw web content (single-item mode)' },
      title: { type: 'string', description: 'Human-readable title (single-item mode)' },
      source_url: { type: 'string', description: 'URL the content was fetched from' },
      format: {
        type: 'string',
        enum: ['markdown', 'html', 'text'],
        description: 'Content format (default html)',
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
            source_url: { type: 'string' },
            format: { type: 'string', enum: ['markdown', 'html', 'text'] },
          },
          required: ['content', 'title'],
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
  item: z.infer<typeof PerceiveItemSchema>,
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
  const gatheredDir = path.join(CONFIG.VAULT_PATH, CONFIG.RAW_GATHERED);

  let candidate = `${date}-${slug}`;
  let counter = 2;
  let targetPath = path.join(gatheredDir, `${candidate}.${ext}`);
  while (true) {
    try {
      await fs.stat(targetPath);
      candidate = `${date}-${slug}-${counter}`;
      targetPath = path.join(gatheredDir, `${candidate}.${ext}`);
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') break;
      throw err;
    }
  }

  const rawPathRel = path.posix.join(CONFIG.RAW_GATHERED, `${candidate}.${ext}`);
  await writeAtomicFile(targetPath, content);

  const queueItem: QueueItem = {
    raw_path: rawPathRel,
    source: 'gathered',
    captured_at: nowISO(),
    title,
    ...(source_url !== undefined && { source_url }),
    format,
    content_hash: contentHash,
  };

  await enqueueItem(queueItem);
  logger.info('brain_perceive ingested web content', { raw_path: rawPathRel, content_hash: contentHash });

  return { status: 'queued', raw_path: rawPathRel, content_hash: contentHash, title };
}

export async function runBrainPerceive(input: z.infer<typeof BrainPerceiveSchema>) {
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
      source_url: input.source_url,
      format: input.format,
    },
    provenance,
  );

  if (result.status === 'duplicate') {
    return {
      status: 'duplicate' as const,
      content_hash: result.content_hash,
      message: 'Already ingested (SHA256 match). No action taken.',
    };
  }

  return {
    status: 'queued' as const,
    raw_path: result.raw_path,
    content_hash: result.content_hash,
    message: `Stored to ${result.raw_path}. Queued for gate evaluation + synthesis — call brain_synthesize to process.`,
  };
}

export async function handleBrainPerceive(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainPerceiveSchema.parse(args);
    const result = await runBrainPerceive(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_perceive failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_perceive: ${formatError(err)}` }],
      isError: true,
    };
  }
}
