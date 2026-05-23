/**
 * Core lifecycle for mcp-phantom-brain: vault structure, search index,
 * vector index, working memory DB.
 */
import { ensureVaultStructure } from '../vault/structure.js';
import { buildIndex } from '../vault/search.js';
import { initVectorIndex, syncVectorIndex } from '../vault/vector-index.js';
import { ensureWorkingDb, cleanupSnapshot } from '../working/db.js';

export { CONFIG, getVaultPath } from '../config.js';
export { logger } from '../shared/logger.js';

/**
 * Bring the brain online: vault folders exist, search + vector + FTS indexes
 * built, working memory DB ready. Idempotent at the module level.
 */
export async function initialize(): Promise<void> {
  await ensureVaultStructure();
  await initVectorIndex();
  await buildIndex();
  ensureWorkingDb();
  syncVectorIndex();
}

/**
 * Snapshot working memory before exit. Called by SIGINT/SIGTERM handlers.
 */
export function shutdown(): void {
  cleanupSnapshot();
}
