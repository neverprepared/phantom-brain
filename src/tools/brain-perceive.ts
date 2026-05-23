/**
 * brain_perceive — gathered-source ingest (Phase 3).
 *
 * The hook counterpart for manual use: writes content to Raw/gathered/ and
 * queues for synthesis with source='gathered', which routes through the LLM
 * gate (Phase 2). Semantically identical to brain_learn except that gathered
 * sources are auto-captured web content rather than human-curated documents.
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

export const BrainPerceiveSchema = z.object({
  content: z.string().describe('Raw web content to ingest'),
  title: z.string().describe('Human-readable title (URL hostname or page title)'),
  source_url: z.string().optional().describe('URL the content was fetched from'),
  format: z.enum(['markdown', 'html', 'text']).optional().default('html'),
});

export const brainPerceiveToolDefinition = {
  name: 'brain_perceive',
  description:
    'Ingest a gathered web source into the brain pipeline. Writes to Raw/gathered/ and queues ' +
    'for gate evaluation + synthesis. Use for web content fetched during research — the hook ' +
    'calls this automatically on every WebFetch/WebSearch; use manually to re-ingest or force ' +
    'content that the hook may have skipped. Duplicate content (SHA256 match) is a no-op.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      content: { type: 'string', description: 'Raw web content to ingest' },
      title: { type: 'string', description: 'Human-readable title (URL hostname or page title)' },
      source_url: { type: 'string', description: 'URL the content was fetched from' },
      format: {
        type: 'string',
        enum: ['markdown', 'html', 'text'],
        description: 'Content format (default html for web content)',
      },
    },
    required: ['content', 'title'],
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

export async function runBrainPerceive(input: z.infer<typeof BrainPerceiveSchema>) {
  const { content, title, source_url, format } = input;

  const contentHash = sha256(content);
  const provenance = await readProvenance();
  if (await isHashKnown(contentHash, provenance)) {
    return {
      status: 'duplicate' as const,
      content_hash: contentHash,
      message: 'Already ingested (SHA256 match). No action taken.',
    };
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

  const item: QueueItem = {
    raw_path: rawPathRel,
    source: 'gathered',
    captured_at: nowISO(),
    title,
    ...(source_url !== undefined && { source_url }),
    format,
    content_hash: contentHash,
  };

  await enqueueItem(item);
  logger.info('brain_perceive ingested web content', { raw_path: rawPathRel, content_hash: contentHash });

  return {
    status: 'queued' as const,
    raw_path: rawPathRel,
    content_hash: contentHash,
    message: `Stored to ${rawPathRel}. Queued for gate evaluation + synthesis — call brain_synthesize to process.`,
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
