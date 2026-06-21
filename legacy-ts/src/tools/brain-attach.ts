/**
 * brain_attach — ingest a binary file (PDF, Word doc, image, etc.) into the vault.
 *
 * Stores the raw binary in Raw/attachments/<sha256><ext> as an immutable artifact,
 * extracts text via the appropriate system tool, writes the extracted text as a
 * sidecar in Raw/curated/, and enqueues it for synthesis.
 *
 * The original binary is never deleted — Raw/attachments/ has no cleanup policy.
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
import { extractTextFromFile } from '../vault/extract-text.js';
import { todayDateString, nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainAttachSchema = z.object({
  file_path: z.string().optional().describe('Absolute local path to the binary file'),
  base64_data: z.string().optional().describe('Base64-encoded file contents'),
  original_filename: z.string().describe('Original filename, used to derive the extension (e.g. "invoice.pdf")'),
  title: z.string().describe('Human-readable title for synthesis'),
  source_url: z.string().optional().describe('URL the file was downloaded from, if known'),
}).refine(
  (d) => (d.file_path !== undefined) !== (d.base64_data !== undefined),
  { message: 'Provide exactly one of file_path or base64_data' },
);

export const brainAttachToolDefinition = {
  name: 'brain_attach',
  description:
    'Store a binary file (PDF, Word doc, image, etc.) in the vault as an immutable artifact. ' +
    'The raw binary is saved to Raw/attachments/ by SHA256 hash, text is extracted via the ' +
    'appropriate system tool, and the extracted text is queued for synthesis. ' +
    'Attachments are never deleted. Duplicate detection by SHA256.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      file_path: { type: 'string', description: 'Absolute local path to the binary file' },
      base64_data: { type: 'string', description: 'Base64-encoded file contents' },
      original_filename: { type: 'string', description: 'Original filename, e.g. "invoice.pdf"' },
      title: { type: 'string', description: 'Human-readable title for synthesis' },
      source_url: { type: 'string', description: 'URL the file was downloaded from, if known' },
    },
    required: ['original_filename', 'title'],
  },
};

export async function runBrainAttach(input: z.infer<typeof BrainAttachSchema>) {
  const { original_filename, title, source_url } = input;

  // Resolve binary content as Buffer
  let buffer: Buffer;
  if (input.file_path) {
    buffer = await fs.readFile(input.file_path);
  } else {
    buffer = Buffer.from(input.base64_data!, 'base64');
  }

  const contentHash = createHash('sha256').update(buffer).digest('hex');

  // Dedup against provenance map
  const provenance = await readProvenance();
  if (await isHashKnown(contentHash, provenance)) {
    return {
      status: 'duplicate' as const,
      content_hash: contentHash,
      message: 'This file has already been ingested (SHA256 match). No action taken.',
    };
  }

  const ext = path.extname(original_filename).toLowerCase() || '.bin';
  const attachmentRelPath = path.posix.join(CONFIG.RAW_ATTACHMENTS, `${contentHash}${ext}`);
  const attachmentAbsPath = path.join(CONFIG.VAULT_PATH, attachmentRelPath);

  // Binary-safe atomic write (writeAtomicFile is text-only so we do it inline)
  await fs.mkdir(path.dirname(attachmentAbsPath), { recursive: true });
  const tmp = `${attachmentAbsPath}.tmp`;
  await fs.writeFile(tmp, buffer);
  await fs.rename(tmp, attachmentAbsPath);

  // Extract text
  let extractionMethod: string;
  let extractedText: string;
  let extractionFailed = false;

  try {
    const result = await extractTextFromFile(attachmentAbsPath, ext);
    extractionMethod = result.method;
    extractedText = result.text;
  } catch (extractErr) {
    extractionMethod = 'failed';
    extractedText = '';
    extractionFailed = true;
    logger.warn('brain_attach: text extraction failed', {
      attachment: attachmentRelPath,
      error: String(extractErr),
    });
  }

  // Build sidecar content — placeholder when extraction yielded nothing
  const sidecarContent = extractedText.trim().length > 0
    ? extractedText
    : `Attachment stored at ${attachmentRelPath}. Extraction method: ${extractionMethod}. No text content available.`;

  // Write sidecar .txt to Raw/curated/ for synthesis
  const slug = slugFromTitle(title);
  const date = todayDateString();
  const curatedDir = path.join(CONFIG.VAULT_PATH, CONFIG.RAW_CURATED);

  let candidate = `${date}-${slug}`;
  let counter = 2;
  let sidecarAbsPath = path.join(curatedDir, `${candidate}.txt`);
  while (true) {
    try {
      await fs.stat(sidecarAbsPath);
      candidate = `${date}-${slug}-${counter}`;
      sidecarAbsPath = path.join(curatedDir, `${candidate}.txt`);
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') break;
      throw err;
    }
  }

  const sidecarRelPath = path.posix.join(CONFIG.RAW_CURATED, `${candidate}.txt`);
  await writeAtomicFile(sidecarAbsPath, sidecarContent);

  const item: QueueItem = {
    raw_path: sidecarRelPath,
    source: 'curated',
    captured_at: nowISO(),
    title,
    ...(source_url !== undefined && { source_url }),
    format: 'text',
    content_hash: contentHash,
    source_attachment: attachmentRelPath,
  };

  await enqueueItem(item);

  logger.info('brain_attach ingested file', {
    attachment: attachmentRelPath,
    sidecar: sidecarRelPath,
    content_hash: contentHash,
    extraction_method: extractionMethod,
    extracted_chars: extractedText.length,
  });

  if (extractionFailed) {
    return {
      status: 'extraction_failed' as const,
      attachment_path: attachmentRelPath,
      raw_path: sidecarRelPath,
      content_hash: contentHash,
      extraction_method: extractionMethod,
      extracted_chars: 0,
      message: `Binary stored to ${attachmentRelPath}. Text extraction failed — placeholder body queued. Call brain_synthesize to process.`,
    };
  }

  return {
    status: 'queued' as const,
    attachment_path: attachmentRelPath,
    raw_path: sidecarRelPath,
    content_hash: contentHash,
    extraction_method: extractionMethod,
    extracted_chars: extractedText.length,
    message: `Stored binary to ${attachmentRelPath}. Queued for synthesis — call brain_synthesize to process.`,
  };
}

export async function handleBrainAttach(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainAttachSchema.parse(args);
    const result = await runBrainAttach(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_attach failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_attach: ${formatError(err)}` }],
      isError: true,
    };
  }
}
