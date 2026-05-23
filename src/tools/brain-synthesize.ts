/**
 * brain_synthesize — Phase 1 synthesis worker.
 *
 * Claims one item from the queue, reads its raw content, writes a summary
 * page under Wiki/summaries/, fans out into entity pages under
 * Wiki/entities/ (creating new pages or appending to existing ones),
 * appends a synthesis log line, records the Raw -> Wiki mapping in
 * provenance.json, and refreshes the graduated Wiki/_index.md.
 *
 * Phase 2 will replace the stub summary body and the pending reliability
 * verdicts with a real extraction + gate pass.
 *
 * Failure handling: any error after a successful claim unclaims the item
 * so it stays in the pending queue for the next run.
 */
import { z } from 'zod';
import fs from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CONFIG } from '../config.js';
import { writeAtomicFile, withFileLock } from '../vault/filesystem.js';
import { slugFromTitle } from '../vault/naming.js';
import {
  claimNextItem,
  markDone,
  unclaimItem,
  type QueueItem,
} from '../vault/queue.js';
import {
  readProvenance,
  writeProvenance,
  isHashKnown,
  type ProvenanceEntry,
} from '../vault/provenance.js';
import { indexWikiEntry } from '../vault/search.js';
import {
  extractEntities,
  entityPaths,
  readEntityPage,
  createEntityPage,
  appendToEntityPage,
  buildContentSnippet,
} from '../vault/entities.js';
import { updateWikiIndex } from '../vault/wiki-index-md.js';
import { runGate, type GateVerdict } from '../gate/evaluate.js';
import { nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainSynthesizeSchema = z.object({}).strict();

export const brainSynthesizeToolDefinition = {
  name: 'brain_synthesize',
  description:
    'Process the next queued item from brain_learn. Reads the raw document, runs the Gate to ' +
    'judge source reliability, writes a summary page to Wiki/summaries/, extracts entities and ' +
    'creates or appends to entity pages under Wiki/entities/, refreshes the graduated ' +
    'Wiki/_index.md, appends a line to Wiki/_log.md, and records the Raw -> Wiki mapping in ' +
    '_index/provenance.json.',
  inputSchema: {
    type: 'object' as const,
    properties: {},
    additionalProperties: false,
  },
};

const STUB_BODY_LIMIT = 2000;
const FETCH_TIMEOUT_MS = 20_000;
const FETCH_MAX_BYTES = 2 * 1024 * 1024; // 2 MB

async function fetchUrl(url: string): Promise<string> {
  const res = await fetch(url, {
    signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    headers: { 'User-Agent': 'mcp-phantom-brain/1.0 (+research-bot)' },
  });
  if (!res.ok) throw new Error(`HTTP ${res.status} ${res.statusText}`);
  const buf = await res.arrayBuffer();
  const bytes = new Uint8Array(buf.slice(0, FETCH_MAX_BYTES));
  return Buffer.from(bytes).toString('utf-8');
}

async function materializeDeferred(item: QueueItem): Promise<{ rawPathRel: string; content: string; contentHash: string }> {
  const url = item.source_url!;
  const content = await fetchUrl(url);
  const contentHash = createHash('sha256').update(content).digest('hex');

  const slug = slugFromTitle(item.title);
  const date = new Date().toISOString().slice(0, 10);
  const gatheredDir = path.join(CONFIG.VAULT_PATH, CONFIG.RAW_GATHERED);
  await fs.mkdir(gatheredDir, { recursive: true });

  let candidate = `${date}-${slug}`;
  let counter = 2;
  let targetPath = path.join(gatheredDir, `${candidate}.html`);
  while (true) {
    try {
      await fs.stat(targetPath);
      candidate = `${date}-${slug}-${counter}`;
      targetPath = path.join(gatheredDir, `${candidate}.html`);
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') break;
      throw err;
    }
  }

  await writeAtomicFile(targetPath, content);
  const rawPathRel = path.posix.join(CONFIG.RAW_GATHERED, `${candidate}.html`);
  return { rawPathRel, content, contentHash };
}

async function nextAvailableSummaryPath(slug: string): Promise<{ relPath: string; absPath: string; finalSlug: string }> {
  const summariesDir = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, CONFIG.WIKI_SUMMARIES);
  let candidate = slug;
  let counter = 2;
  while (true) {
    const absPath = path.join(summariesDir, `${candidate}.md`);
    try {
      await fs.stat(absPath);
      candidate = `${slug}-${counter}`;
      counter++;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
        const relPath = path.posix.join(CONFIG.WIKI_FOLDER, CONFIG.WIKI_SUMMARIES, `${candidate}.md`);
        return { relPath, absPath, finalSlug: candidate };
      }
      throw err;
    }
  }
}

function escapeYaml(s: string): string {
  return s.replace(/"/g, '\\"');
}

function buildSummaryPage(opts: {
  title: string;
  rawPath: string;
  sourceUrl?: string;
  capturedAt: string;
  synthesizedAt: string;
  body: string;
  verdict: GateVerdict;
}): string {
  const { title, rawPath, sourceUrl, capturedAt, synthesizedAt, body, verdict } = opts;
  const escapedTitle = escapeYaml(title);
  const sourceUrlLine = sourceUrl ? `source_url: "${sourceUrl}"\n` : '';
  const truncated = body.slice(0, STUB_BODY_LIMIT);
  const truncatedNote = body.length > STUB_BODY_LIMIT ? '\n\n<!-- Body truncated at 2000 chars for Phase 0 stub -->' : '';
  const categoryLine = verdict.category ? `category: ${verdict.category}\n` : '';
  return (
    `---\n` +
    `title: "${escapedTitle}"\n` +
    `kind: summary\n` +
    `source: "${rawPath}"\n` +
    sourceUrlLine +
    `captured_at: "${capturedAt}"\n` +
    `synthesized_at: "${synthesizedAt}"\n` +
    `reliability: ${verdict.reliability}\n` +
    categoryLine +
    `reason: "${escapeYaml(verdict.reason)}"\n` +
    `tags: []\n` +
    `---\n\n` +
    `${truncated}${truncatedNote}\n\n` +
    `<!-- Full extraction: Phase 2 -->\n`
  );
}

export async function runBrainSynthesize(_input: z.infer<typeof BrainSynthesizeSchema>) {
  const claim = await claimNextItem();
  if (!claim) {
    return {
      status: 'empty' as const,
      message: 'Queue is empty. Nothing to synthesize.',
    };
  }

  const [claimedPath, item] = claim;

  try {
    // Resolve raw content — either read from vault or fetch the URL (deferred-fetch items).
    let rawContent: string;
    let rawPath: string;
    let contentHash: string | undefined;

    if (item.deferred_fetch) {
      if (!item.source_url) {
        throw new Error('deferred_fetch item has no source_url');
      }
      // Check provenance first — same URL may already be synthesized via brain_learn.
      const provenance = await readProvenance();
      const materialized = await materializeDeferred(item);
      rawContent = materialized.content;
      rawPath = materialized.rawPathRel;
      contentHash = materialized.contentHash;
      if (await isHashKnown(contentHash, provenance)) {
        await markDone(claimedPath);
        return {
          status: 'duplicate' as const,
          message: `Content at ${item.source_url} already synthesized (SHA256 match). Marked done.`,
        };
      }
    } else {
      if (!item.raw_path) throw new Error('Queue item has no raw_path and deferred_fetch is not set');
      rawPath = item.raw_path;
      rawContent = await fs.readFile(path.join(CONFIG.VAULT_PATH, rawPath), 'utf-8');
      contentHash = item.content_hash;
    }

    // Phase 2 — Gate the source. runGate never throws; on any failure it
    // returns a safe medium fallback so synthesis can proceed.
    const verdict = await runGate({
      title: item.title,
      ...(item.source_url !== undefined && { sourceUrl: item.source_url }),
      content: rawContent,
      format: item.format,
      source: item.source,
    });

    const synthesizedAt = nowISO();
    const { relPath: summaryRel, absPath: summaryAbs, finalSlug } = await nextAvailableSummaryPath(slugFromTitle(item.title));

    // Write summary page
    const page = buildSummaryPage({
      title: item.title,
      rawPath,
      ...(item.source_url !== undefined && { sourceUrl: item.source_url }),
      capturedAt: item.captured_at,
      synthesizedAt,
      body: rawContent,
      verdict,
    });
    await writeAtomicFile(summaryAbs, page);

    // Phase 1: extract entities from raw content, then create or append
    // to entity pages under Wiki/entities/.
    const entityNames = extractEntities(rawContent);
    const entityPagePaths: string[] = [];
    const entityNamesProcessed: string[] = [];

    for (const name of entityNames) {
      try {
        const { absPath: entAbs, relPath: entRel } = entityPaths(name);
        const snippet = buildContentSnippet(rawContent, name);
        const existing = await readEntityPage(entAbs);

        if (existing === null) {
          await createEntityPage({
            name,
            absPath: entAbs,
            sourceTitle: item.title,
            rawPath,
            contentSnippet: snippet,
            now: synthesizedAt,
            verdict,
          });
        } else {
          await appendToEntityPage({
            absPath: entAbs,
            sourceTitle: item.title,
            rawPath,
            contentSnippet: snippet,
            now: synthesizedAt,
            verdict,
          });
        }

        // Index the entity page so brain_recall picks it up without a full rebuild.
        // Wiki index relPath is relative to Wiki/, not the vault root.
        const wikiRelForIndex = path.posix.join(CONFIG.WIKI_ENTITIES, `${slugFromTitle(name)}.md`);
        // Use the freshly written content if we have it, otherwise the snippet
        // is a good-enough body for the index.
        const indexedBody = snippet;
        indexWikiEntry(wikiRelForIndex, name, 'entity', [], indexedBody, synthesizedAt, synthesizedAt);

        entityPagePaths.push(entRel);
        entityNamesProcessed.push(name);
      } catch (entErr) {
        // Don't fail the whole synthesis because one entity page broke.
        // Log it and move on; the queue item is still useful with the summary.
        logger.warn('Failed to process entity page', {
          entity: name,
          raw_path: item.raw_path,
          error: String(entErr),
        });
      }
    }

    // Append to Wiki/_log.md (append-only, serialized via lock)
    const logPath = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, CONFIG.WIKI_LOG_FILE);
    const entitiesLine = entityNamesProcessed.length > 0
      ? `- Entities: ${entityNamesProcessed.join(', ')}\n`
      : `- Entities: (none extracted)\n`;
    const logLine =
      `\n## ${synthesizedAt} — ${item.title}\n` +
      `- Source: ${rawPath}\n` +
      `- Summary: ${summaryRel}\n` +
      entitiesLine +
      `- Gate: ${verdict.reliability} — ${verdict.reason}\n`;
    await withFileLock(logPath, async () => {
      await fs.mkdir(path.dirname(logPath), { recursive: true });
      await fs.appendFile(logPath, logLine, 'utf-8');
    });

    // Update provenance.json — wiki_pages includes the summary AND all
    // entity pages this source contributed to.
    const provenance = await readProvenance();
    const entry: ProvenanceEntry = {
      wiki_pages: [summaryRel, ...entityPagePaths],
      synthesized_at: synthesizedAt,
      reliability: verdict.reliability,
      ...(verdict.category !== undefined && { category: verdict.category }),
      content_hash: contentHash ?? '',
    };
    provenance[rawPath] = entry;
    await writeProvenance(provenance);

    // Refresh Wiki/_index.md graduated tiers from the updated provenance.
    try {
      await updateWikiIndex(provenance);
    } catch (idxErr) {
      logger.warn('Failed to update Wiki/_index.md', { error: String(idxErr) });
    }

    // Done — move the queue item to done/
    await markDone(claimedPath);

    // Index the new summary page so brain_recall sees it without a full rebuild.
    // relPath for the wiki index is relative to Wiki/, not the vault root.
    const wikiRelPath = path.posix.join(CONFIG.WIKI_SUMMARIES, `${finalSlug}.md`);
    indexWikiEntry(wikiRelPath, item.title, 'summary', [], rawContent.slice(0, STUB_BODY_LIMIT), synthesizedAt, synthesizedAt);

    logger.info('brain_synthesize processed item', {
      raw_path: rawPath,
      summary: summaryRel,
      entities: entityNamesProcessed.length,
      reliability: verdict.reliability,
      ...(verdict.category !== undefined && { category: verdict.category }),
    });

    return {
      status: 'synthesized' as const,
      raw_path: rawPath,
      summary_path: summaryRel,
      entity_pages: entityPagePaths,
      entities: entityNamesProcessed,
      synthesized_at: synthesizedAt,
      gate: verdict,
      message:
        `Synthesized ${rawPath} -> ${summaryRel} ` +
        `(+${entityPagePaths.length} entity pages). ` +
        `Gate: ${verdict.reliability}.`,
    };
  } catch (err) {
    // Restore the queue item so the next call can retry.
    try {
      await unclaimItem(claimedPath);
    } catch (unclaimErr) {
      logger.error('Failed to unclaim queue item after synthesis error', {
        claimedPath,
        error: String(unclaimErr),
      });
    }
    throw err;
  }
}

export async function handleBrainSynthesize(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainSynthesizeSchema.parse(args);
    const result = await runBrainSynthesize(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_synthesize failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_synthesize: ${formatError(err)}` }],
      isError: true,
    };
  }
}
