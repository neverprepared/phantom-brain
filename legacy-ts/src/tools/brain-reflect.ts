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
import { readProvenance, upsertProvenanceEntry, deleteProvenanceEntry } from '../vault/provenance.js';
import type { ProvenanceEntry } from '../vault/provenance.js';
import { updateWikiIndex } from '../vault/wiki-index-md.js';
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

  // 2. Broken provenance — entry exists but raw file was deleted; clean up automatically
  const brokenEntries: string[] = [];
  for (const rawPath of provenanceKeys) {
    try {
      await fs.stat(path.join(CONFIG.VAULT_PATH, rawPath));
    } catch {
      brokenEntries.push(rawPath);
    }
  }
  for (const rawPath of brokenEntries) {
    await deleteProvenanceEntry(rawPath);
  }
  if (brokenEntries.length > 0) {
    try { await updateWikiIndex(); } catch { /* non-fatal */ }
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
  // Collect provenance updates separately so we apply them atomically per-entry
  // rather than bulk-writing the stale snapshot read at the start of this pass.
  const provenanceUpdates: Array<{ rawPath: string; entry: ProvenanceEntry }> = [];

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

      provenanceUpdates.push({
        rawPath,
        entry: {
          ...entry,
          reliability: verdict.reliability,
          ...(verdict.category ? { category: verdict.category } : {}),
        },
      });

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

  for (const { rawPath, entry } of provenanceUpdates) {
    await upsertProvenanceEntry(rawPath, entry);
  }

  // 5. Prune done/ queue items older than 30 days
  const DONE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;
  let donesPruned = 0;
  try {
    const doneDir = path.join(CONFIG.VAULT_PATH, CONFIG.QUEUE_DONE);
    const doneEntries = await fs.readdir(doneDir).catch(() => [] as string[]);
    const now = Date.now();
    await Promise.all(doneEntries.map(async (name) => {
      const filePath = path.join(doneDir, name);
      try {
        const stat = await fs.stat(filePath);
        if (now - stat.mtimeMs > DONE_MAX_AGE_MS) {
          await fs.unlink(filePath);
          donesPruned++;
        }
      } catch { /* best-effort */ }
    }));
  } catch { /* done dir may not exist */ }

  // 6. Rotate Wiki/_log.md if it exceeds 5000 lines
  const LOG_MAX_LINES = 5000;
  const LOG_KEEP_LINES = 4000;
  let logRotated = false;
  try {
    const logPath = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, CONFIG.WIKI_LOG_FILE);
    const logContent = await fs.readFile(logPath, 'utf-8').catch(() => '');
    const lines = logContent.split('\n');
    if (lines.length > LOG_MAX_LINES) {
      const kept = lines.slice(-LOG_KEEP_LINES);
      const notice = `# Log rotated at ${new Date().toISOString()} — older entries trimmed\n`;
      await writeAtomicFile(logPath, notice + kept.join('\n'));
      logRotated = true;
      logger.info('brain_reflect rotated _log.md', { from: lines.length, to: kept.length });
    }
  } catch { /* log may not exist yet */ }

  // 7. Reap dead working-memory shards (wm-<pid>.sqlite in _index/)
  let shardsReaped = 0;
  try {
    const indexDir = path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER);
    const indexEntries = await fs.readdir(indexDir).catch(() => [] as string[]);
    await Promise.all(indexEntries
      .filter(name => /^wm-\d+\.sqlite$/.test(name))
      .map(async (name) => {
        const pid = parseInt(name.slice(3, -7), 10);
        let alive = false;
        try { process.kill(pid, 0); alive = true; } catch { alive = false; }
        if (!alive) {
          // Delete the shard and any WAL/SHM companions
          const base = path.join(indexDir, name);
          await fs.unlink(base).catch(() => {});
          await fs.unlink(base + '-wal').catch(() => {});
          await fs.unlink(base + '-shm').catch(() => {});
          shardsReaped++;
          logger.info('brain_reflect reaped dead WM shard', { name });
        }
      })
    );
  } catch { /* best-effort */ }

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
  if (donesPruned > 0) messageParts.push(`${donesPruned} done/ queue file(s) pruned.`);
  if (logRotated) messageParts.push(`Wiki/_log.md rotated.`);
  if (shardsReaped > 0) messageParts.push(`${shardsReaped} dead WM shard(s) reaped.`);
  if (issueCount === 0 && staleEntries.length === 0) messageParts.push('Wiki is clean.');

  logger.info('brain_reflect complete', {
    orphans: orphanRaw.length,
    broken: brokenEntries.length,
    stale: staleEntries.length,
    re_gated: reGated.length,
    duplicates: duplicateUrls.length,
    donesPruned,
    logRotated,
    shardsReaped,
  });

  return {
    status,
    scope,
    orphan_raw_files: orphanRaw,
    broken_provenance: brokenEntries,
    stale_gates: staleEntries.map(e => ({ raw_path: e.rawPath, summary: e.summaryPath, reason: e.oldReason })),
    re_gated: reGated,
    duplicate_urls: duplicateUrls,
    done_queue_pruned: donesPruned,
    log_rotated: logRotated,
    wm_shards_reaped: shardsReaped,
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
