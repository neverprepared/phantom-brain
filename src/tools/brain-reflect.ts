/**
 * brain_reflect — Phase 4 Wiki maintenance pass.
 *
 * light scope (default): scan and report only.
 * full scope: scan + re-gate all stale entries via the claude CLI.
 *
 * Checks performed:
 *   1. Orphan raw files — Raw/ files with no provenance entry (never synthesized).
 *   2. Broken provenance — provenance entries whose raw file no longer exists.
 *   3. Stale gates — summaries whose gate verdict was a fallback (no API key, timeout, etc.).
 *      Full scope re-gates these and patches the summary frontmatter + provenance.
 *   4. Duplicate URLs — same source_url synthesized into multiple summary pages.
 *
 * Contract: never throws. Returns a structured report on every code path.
 */
import { z } from 'zod';
import fs from 'node:fs/promises';
import path from 'node:path';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CONFIG } from '../config.js';
import { readProvenance, writeProvenance } from '../vault/provenance.js';
import { writeAtomicFile } from '../vault/filesystem.js';
import { runGate } from '../gate/evaluate.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainReflectSchema = z.object({
  scope: z.enum(['full', 'light']).optional().default('light'),
});

export const brainReflectToolDefinition = {
  name: 'brain_reflect',
  description:
    'Wiki maintenance pass: orphan detection, stale gate re-scoring, broken provenance cleanup, ' +
    'and duplicate URL flagging. light scope (default) reports issues only. ' +
    'full scope re-gates stale entries via the claude CLI and patches summary pages.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      scope: {
        type: 'string',
        enum: ['full', 'light'],
        description: 'light = report only (default); full = report + re-gate stale entries',
      },
    },
  },
};

// Phrases in a gate reason string that indicate a fallback verdict (re-gateable).
const STALE_PHRASES = [
  'not configured',
  'Phase 2 gate skipped',
  'Gate disabled',
  'timed out',
  'returned no text',
  'unparseable',
  'call failed',
  'gate skipped',
];

function isStaleReason(reason: string): boolean {
  return STALE_PHRASES.some(p => reason.toLowerCase().includes(p.toLowerCase()));
}

interface FrontmatterData {
  title?: string;
  source_url?: string;
  source?: string;
  reliability?: string;
  reason?: string;
  format?: string;
  category?: string;
}

function parseFrontmatter(content: string): FrontmatterData {
  const match = content.match(/^---\n([\s\S]*?)\n---/);
  if (!match || !match[1]) return {};
  const fm: FrontmatterData = {};
  for (const line of match[1].split('\n')) {
    const colon = line.indexOf(':');
    if (colon < 0) continue;
    const key = line.slice(0, colon).trim();
    const val = line.slice(colon + 1).trim().replace(/^"(.*)"$/, '$1');
    (fm as Record<string, string>)[key] = val;
  }
  return fm;
}

function patchFrontmatter(content: string, patches: Record<string, string | null>): string {
  const match = content.match(/^(---\n)([\s\S]*?)(\n---\n)/);
  if (!match || !match[2]) return content;
  let fm: string = match[2];
  for (const [key, value] of Object.entries(patches)) {
    const lineRe = new RegExp(`^${key}:.*$`, 'm');
    if (value === null) {
      fm = fm.replace(new RegExp(`^${key}:.*\n?`, 'm'), '');
    } else if (lineRe.test(fm)) {
      fm = fm.replace(lineRe, `${key}: ${value}`);
    } else {
      fm += `\n${key}: ${value}`;
    }
  }
  return content.replace(/^---\n[\s\S]*?\n---\n/, `---\n${fm}\n---\n`);
}

async function scanDir(relPath: string): Promise<string[]> {
  const absDir = path.join(CONFIG.VAULT_PATH, relPath);
  try {
    const entries = await fs.readdir(absDir);
    return entries
      .filter(n => !n.startsWith('.') && !n.startsWith('_'))
      .map(n => path.posix.join(relPath, n));
  } catch {
    return [];
  }
}

async function readSummaryFrontmatter(relPath: string): Promise<FrontmatterData | null> {
  try {
    const content = await fs.readFile(path.join(CONFIG.VAULT_PATH, relPath), 'utf-8');
    return parseFrontmatter(content);
  } catch {
    return null;
  }
}

export async function runBrainReflect(input: z.infer<typeof BrainReflectSchema>) {
  const { scope } = input;
  logger.info('brain_reflect started', { scope });

  const provenance = await readProvenance();
  const provenanceKeys = new Set(Object.keys(provenance));

  // 1. Orphan raw files — in Raw/ but not in provenance
  const gatheredFiles = await scanDir(CONFIG.RAW_GATHERED);
  const curatedFiles = await scanDir(CONFIG.RAW_CURATED);
  const orphanRaw = [...gatheredFiles, ...curatedFiles].filter(f => !provenanceKeys.has(f));

  // 2. Broken provenance — entry exists but raw file was deleted
  const brokenEntries: string[] = [];
  for (const rawPath of provenanceKeys) {
    try {
      await fs.stat(path.join(CONFIG.VAULT_PATH, rawPath));
    } catch {
      brokenEntries.push(rawPath);
    }
  }

  // 3. Stale gates — summaries whose gate reason is a known fallback phrase
  const staleEntries: Array<{ rawPath: string; summaryPath: string; oldReason: string }> = [];
  const reGated: Array<{
    rawPath: string;
    summaryPath: string;
    oldReliability: string;
    newReliability: string;
    newReason: string;
  }> = [];

  for (const [rawPath, entry] of Object.entries(provenance)) {
    const summaryPath = entry.wiki_pages.find(p => p.includes('/summaries/'));
    if (!summaryPath) continue;

    const fm = await readSummaryFrontmatter(summaryPath);
    if (!fm?.reason) continue;
    if (!isStaleReason(fm.reason)) continue;

    staleEntries.push({ rawPath, summaryPath, oldReason: fm.reason });

    if (scope !== 'full') continue;

    try {
      const rawContent = await fs.readFile(path.join(CONFIG.VAULT_PATH, rawPath), 'utf-8');
      const verdict = await runGate({
        title: fm.title ?? rawPath,
        ...(fm.source_url ? { sourceUrl: fm.source_url } : {}),
        content: rawContent,
        format: (fm.format as 'markdown' | 'html' | 'text' | 'pdf' | undefined) ?? 'html',
        source: rawPath.includes('/curated/') ? 'curated' : 'gathered',
      });

      const summaryAbs = path.join(CONFIG.VAULT_PATH, summaryPath);
      const summaryContent = await fs.readFile(summaryAbs, 'utf-8');
      const patches: Record<string, string | null> = {
        reliability: verdict.reliability,
        reason: `"${verdict.reason.replace(/"/g, '\\"')}"`,
        category: verdict.category ?? null,
      };
      await writeAtomicFile(summaryAbs, patchFrontmatter(summaryContent, patches));

      provenance[rawPath] = {
        ...entry,
        reliability: verdict.reliability,
        ...(verdict.category ? { category: verdict.category } : {}),
      };

      reGated.push({
        rawPath,
        summaryPath,
        oldReliability: entry.reliability,
        newReliability: verdict.reliability,
        newReason: verdict.reason,
      });
      logger.info('brain_reflect re-gated', { rawPath, reliability: verdict.reliability });
    } catch (err) {
      logger.warn('brain_reflect re-gate failed', { rawPath, error: String(err) });
    }
  }

  // 4. Duplicate URLs — same source_url in multiple summary pages
  const urlToSummaries = new Map<string, string[]>();
  for (const [, entry] of Object.entries(provenance)) {
    const summaryPath = entry.wiki_pages.find(p => p.includes('/summaries/'));
    if (!summaryPath) continue;
    const fm = await readSummaryFrontmatter(summaryPath);
    if (!fm?.source_url) continue;
    const arr = urlToSummaries.get(fm.source_url) ?? [];
    arr.push(summaryPath);
    urlToSummaries.set(fm.source_url, arr);
  }
  const duplicateUrls = [...urlToSummaries.entries()]
    .filter(([, paths]) => paths.length > 1)
    .map(([url, summaries]) => ({ url, summaries }));

  if (reGated.length > 0) {
    await writeProvenance(provenance);
  }

  const issueCount = orphanRaw.length + brokenEntries.length + duplicateUrls.length;
  const status = issueCount === 0 && staleEntries.length === 0 ? 'ok' : 'issues_found';

  const messageParts = [`Reflect complete (${scope}).`];
  if (orphanRaw.length > 0) messageParts.push(`${orphanRaw.length} orphan raw file(s) with no provenance.`);
  if (brokenEntries.length > 0) messageParts.push(`${brokenEntries.length} broken provenance entry/entries (raw file missing).`);
  if (staleEntries.length > 0) {
    messageParts.push(
      scope === 'full'
        ? `${staleEntries.length} stale gate(s) found, ${reGated.length} re-gated.`
        : `${staleEntries.length} stale gate(s) — run full scope to re-gate.`,
    );
  }
  if (duplicateUrls.length > 0) messageParts.push(`${duplicateUrls.length} URL(s) synthesized more than once.`);
  if (issueCount === 0 && staleEntries.length === 0) messageParts.push('Wiki is clean.');

  logger.info('brain_reflect complete', {
    orphans: orphanRaw.length,
    broken: brokenEntries.length,
    stale: staleEntries.length,
    re_gated: reGated.length,
    duplicates: duplicateUrls.length,
  });

  return {
    status,
    scope,
    orphan_raw_files: orphanRaw,
    broken_provenance: brokenEntries,
    stale_gates: staleEntries.map(e => ({ raw_path: e.rawPath, summary: e.summaryPath, reason: e.oldReason })),
    re_gated: reGated,
    duplicate_urls: duplicateUrls,
    message: messageParts.join(' '),
  };
}

export async function handleBrainReflect(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainReflectSchema.parse(args);
    const result = await runBrainReflect(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_reflect failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_reflect: ${formatError(err)}` }],
      isError: true,
    };
  }
}
