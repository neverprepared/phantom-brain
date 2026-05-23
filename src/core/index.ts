/**
 * Core lifecycle for mcp-brain: vault structure, seed reference wiki pages,
 * search index, vector index, working memory DB.
 */
import fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { ensureVaultStructure } from '../vault/structure.js';
import { buildIndex } from '../vault/search.js';
import { initVectorIndex, syncVectorIndex } from '../vault/vector-index.js';
import { renameSlugReferences } from '../vault/links.js';
import { readRenameJournal, deleteRenameJournal } from '../vault/rename-journal.js';
import { ensureWorkingDb, cleanupSnapshot } from '../working/db.js';
import { logger } from '../shared/logger.js';
import { CONFIG } from '../config.js';

export { CONFIG, DEFAULT_TTL_DAYS, getVaultPath } from '../config.js';
export { logger } from '../shared/logger.js';

const SEED_FILES = [
  'wiki/References/logical-fallacies.md',
  'wiki/References/philosophical-razors.md',
  'wiki/References/philosophical-logic.md',
] as const;

/**
 * Copy seed wiki pages from src/seed/ into the vault if they don't exist yet.
 * Idempotent: existing files are never overwritten. Resolved relative to this
 * compiled module so it works from both src (tsx) and dist (node).
 */
export async function ensureSeedFiles(): Promise<void> {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const seedRoot = path.resolve(here, '..', 'seed');

  for (const rel of SEED_FILES) {
    // rel begins with "wiki/" — strip and remap to <vault>/Wiki/...
    const wikiRel = rel.replace(/^wiki\//, '');
    const target = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, wikiRel);
    const source = path.join(seedRoot, rel);

    try {
      await fs.access(target);
      // Already exists — never overwrite user-modified content
      continue;
    } catch {
      // Missing — copy from seed
    }

    try {
      await fs.mkdir(path.dirname(target), { recursive: true });
      const content = await fs.readFile(source, 'utf-8');
      await fs.writeFile(target, content, 'utf-8');
      logger.info('Seeded reference wiki page', { relPath: wikiRel });
    } catch (err) {
      logger.warn('Failed to seed reference wiki page', { rel, error: String(err) });
    }
  }
}

/**
 * Bring the brain online: vault folders exist, seed reference wiki pages
 * present, search + vector + FTS indexes built, working memory DB ready.
 * Idempotent at the module level (init functions guard their own state),
 * but call once per process to be safe.
 */
export async function initialize(): Promise<void> {
  await ensureVaultStructure();
  await ensureSeedFiles();
  await initVectorIndex();
  await buildIndex();
  await recoverInflightRename();
  ensureWorkingDb();
  syncVectorIndex();
}

/**
 * Replay a partially-completed slug rename from the journal on startup.
 * renameSlugReferences is idempotent (files without the old slug are skipped),
 * so re-running is safe whether the prior run completed partially or not.
 */
async function recoverInflightRename(): Promise<void> {
  const journal = await readRenameJournal();
  if (!journal) return;
  logger.info('Recovering in-flight slug rename from journal', { ...journal });
  try {
    const result = await renameSlugReferences(journal.from, journal.to);
    logger.info('Rename recovery complete', { ...journal, updated: result.updated.length, failed: result.failed.length });
  } catch (err) {
    logger.warn('Rename recovery failed; leaving journal in place', { ...journal, error: String(err) });
    return;
  }
  await deleteRenameJournal();
}

/**
 * Snapshot working memory before exit. Called by SIGINT/SIGTERM handlers.
 */
export function shutdown(): void {
  cleanupSnapshot();
}
