import Database from 'better-sqlite3';
import * as sqliteVec from 'sqlite-vec';
import path from 'node:path';
import fs from 'node:fs/promises';
import fsSync from 'node:fs';
import { CONFIG } from '../config.js';
import { getWikiIndex, type WikiIndexEntry } from './search.js';
import { embedBatch, buildEmbedText, isEmbeddingAvailable } from './embeddings.js';
import { logger } from '../shared/logger.js';
import { initFts, isFtsReady } from './fts-index.js';

let vecDb: Database.Database | null = null;

function vectorDbPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER, 'vectors.db');
}

export async function initVectorIndex(): Promise<void> {
  try {
    const dbPath = vectorDbPath();
    await fs.mkdir(path.dirname(dbPath), { recursive: true });

    vecDb = new Database(dbPath);
    sqliteVec.load(vecDb);

    // Enable WAL mode for concurrent read/write access across multiple
    // MCP server instances sharing the same database file.
    vecDb.pragma('journal_mode = WAL');
    // Wait up to 5 seconds when another process holds a write lock
    // instead of failing immediately with SQLITE_BUSY.
    vecDb.pragma('busy_timeout = 5000');

    // Mapping table: string memory ID -> integer rowid for vec0
    vecDb.exec(`
      CREATE TABLE IF NOT EXISTS vec_map (
        id        TEXT PRIMARY KEY,
        vec_rowid INTEGER UNIQUE NOT NULL,
        updated   TEXT NOT NULL
      );
    `);

    // vec0 virtual table for native KNN search with cosine distance
    const dims = CONFIG.EMBEDDING_DIMS;
    vecDb.exec(`
      CREATE VIRTUAL TABLE IF NOT EXISTS vec_embeddings
      USING vec0(embedding float[${dims}] distance_metric=cosine);
    `);

    // Migrate from legacy embeddings table if it exists
    migrateFromLegacyTable();

    // Initialize FTS5 in the same database
    initFts(vecDb);

    logger.info('Vector index initialized', { path: dbPath });
  } catch (err) {
    logger.warn('Vector index init failed — vector search disabled', { error: String(err) });
    vecDb = null;
  }
}

/** Migrate data from the old flat embeddings table to vec0 + vec_map. */
function migrateFromLegacyTable(): void {
  if (!vecDb) return;

  // Check if old table exists
  const oldTable = vecDb.prepare(
    "SELECT name FROM sqlite_master WHERE type='table' AND name='embeddings'"
  ).get() as { name: string } | undefined;
  if (!oldTable) return;

  logger.info('Migrating legacy embeddings table to vec0...');

  const oldRows = vecDb.prepare('SELECT id, embedding, dims FROM embeddings').all() as {
    id: string;
    embedding: Buffer;
    dims: number;
  }[];

  if (oldRows.length > 0) {
    let nextRowid = (vecDb.prepare('SELECT COALESCE(MAX(vec_rowid), 0) as m FROM vec_map').get() as { m: number }).m + 1;

    const insVec = vecDb.prepare('INSERT INTO vec_embeddings(rowid, embedding) VALUES (?, ?)');
    const insMap = vecDb.prepare("INSERT OR IGNORE INTO vec_map(id, vec_rowid, updated) VALUES (?, ?, datetime('now'))");

    const tx = vecDb.transaction(() => {
      for (const row of oldRows) {
        // Skip if already migrated
        const existing = vecDb!.prepare('SELECT vec_rowid FROM vec_map WHERE id = ?').get(row.id) as { vec_rowid: number } | undefined;
        if (existing) continue;

        insVec.run(BigInt(nextRowid), row.embedding);
        insMap.run(row.id, nextRowid);
        nextRowid++;
      }
    });
    tx();

    logger.info('Migrated legacy embeddings', { count: oldRows.length });
  }

  vecDb.exec('DROP TABLE embeddings');
  logger.info('Dropped legacy embeddings table');
}

export function isVectorIndexReady(): boolean {
  return vecDb !== null;
}

/** Upsert a single embedding. Uses delete+insert since vec0 doesn't support UPSERT. */
export function upsertVector(id: string, embedding: number[]): void {
  if (!vecDb) return;
  const buf = Buffer.from(new Float32Array(embedding).buffer);

  const tx = vecDb.transaction(() => {
    const existing = vecDb!.prepare('SELECT vec_rowid FROM vec_map WHERE id = ?').get(id) as { vec_rowid: number } | undefined;

    if (existing) {
      // Update: delete old vec0 row, insert new one at same rowid
      vecDb!.prepare('DELETE FROM vec_embeddings WHERE rowid = ?').run(BigInt(existing.vec_rowid));
      vecDb!.prepare('INSERT INTO vec_embeddings(rowid, embedding) VALUES (?, ?)').run(BigInt(existing.vec_rowid), buf);
      vecDb!.prepare("UPDATE vec_map SET updated = datetime('now') WHERE id = ?").run(id);
    } else {
      // Insert: allocate next rowid
      const nextRowid = (vecDb!.prepare('SELECT COALESCE(MAX(vec_rowid), 0) + 1 as n FROM vec_map').get() as { n: number }).n;
      vecDb!.prepare('INSERT INTO vec_embeddings(rowid, embedding) VALUES (?, ?)').run(BigInt(nextRowid), buf);
      vecDb!.prepare("INSERT INTO vec_map(id, vec_rowid, updated) VALUES (?, ?, datetime('now'))").run(id, nextRowid);
    }
  });
  tx();
}

/** Remove a vector by memory id. */
export function deleteVector(id: string): void {
  if (!vecDb) return;
  const tx = vecDb.transaction(() => {
    const existing = vecDb!.prepare('SELECT vec_rowid FROM vec_map WHERE id = ?').get(id) as { vec_rowid: number } | undefined;
    if (existing) {
      vecDb!.prepare('DELETE FROM vec_embeddings WHERE rowid = ?').run(BigInt(existing.vec_rowid));
      vecDb!.prepare('DELETE FROM vec_map WHERE id = ?').run(id);
    }
  });
  tx();
}

/** Returns the set of memory ids that already have embeddings. */
export function getEmbeddedIds(): Set<string> {
  if (!vecDb) return new Set();
  const rows = vecDb.prepare('SELECT id FROM vec_map').all() as { id: string }[];
  return new Set(rows.map((r) => r.id));
}

/** Returns embedding coverage stats. */
export function getEmbeddingStats(): { embedded: number; total: number } {
  const total = getWikiIndex().size;
  const embedded = vecDb
    ? (vecDb.prepare('SELECT COUNT(*) as n FROM vec_map').get() as { n: number }).n
    : 0;
  return { embedded, total };
}

/** Returns detailed vector index diagnostics for observability. */
export function getVectorDiagnostics(): {
  dbPath: string;
  dbSizeBytes: number;
  journalMode: string;
  busyTimeout: number;
  embeddingCount: number;
  ftsRowCount: number;
  ftsReady: boolean;
  vectorReady: boolean;
  unembeddedIds: string[];
  syncLockHeld: boolean;
  syncLockPid: number | null;
} {
  const dbPath = vectorDbPath();
  let dbSizeBytes = 0;
  try {
    const stat = fsSync.statSync(dbPath);
    dbSizeBytes = stat.size;
    // Include WAL and SHM files if they exist
    try { dbSizeBytes += fsSync.statSync(dbPath + '-wal').size; } catch { /* no WAL file */ }
    try { dbSizeBytes += fsSync.statSync(dbPath + '-shm').size; } catch { /* no SHM file */ }
  } catch { /* DB file doesn't exist yet */ }

  let journalMode = 'unknown';
  let busyTimeout = 0;
  let embeddingCount = 0;
  let ftsRowCount = 0;

  if (vecDb) {
    try {
      journalMode = vecDb.pragma('journal_mode', { simple: true }) as string ?? 'unknown';
    } catch { /* */ }
    try {
      busyTimeout = vecDb.pragma('busy_timeout', { simple: true }) as number ?? 0;
    } catch { /* */ }
    try {
      embeddingCount = (vecDb.prepare('SELECT COUNT(*) as n FROM vec_map').get() as { n: number }).n;
    } catch { /* */ }
    try {
      // FTS5 virtual tables: use a MATCH-less count via rowid
      ftsRowCount = (vecDb.prepare('SELECT COUNT(rowid) as n FROM fts_memories').get() as { n: number }).n;
    } catch { /* */ }
  }

  const embeddedIds = getEmbeddedIds();
  const unembeddedIds = [...getWikiIndex().keys()].filter((id) => !embeddedIds.has(id));

  // Check sync lock state
  let syncLockHeld = false;
  let syncLockPid: number | null = null;
  const lockPath = syncLockPath();
  try {
    if (fsSync.existsSync(lockPath)) {
      const content = fsSync.readFileSync(lockPath, 'utf-8').trim();
      const [pidStr, tsStr] = content.split(':');
      const lockPidVal = parseInt(pidStr ?? '', 10);
      const lockTime = parseInt(tsStr ?? '', 10);
      const staleMs = 5 * 60 * 1000;
      const isStaleVal = Date.now() - lockTime > staleMs;
      let isAlive = false;
      if (!isNaN(lockPidVal)) {
        try { process.kill(lockPidVal, 0); isAlive = true; } catch { isAlive = false; }
      }
      syncLockHeld = !isStaleVal && isAlive;
      syncLockPid = syncLockHeld ? lockPidVal : null;
    }
  } catch { /* no lock file */ }

  return {
    dbPath,
    dbSizeBytes,
    journalMode,
    busyTimeout,
    embeddingCount,
    ftsRowCount,
    ftsReady: isFtsReady(),
    vectorReady: vecDb !== null,
    unembeddedIds,
    syncLockHeld,
    syncLockPid,
  };
}

/**
 * Search for the top-k nearest embeddings to a query vector using sqlite-vec KNN.
 * Returns { id, distance } pairs sorted by ascending distance (closest first).
 */
export function searchVectors(queryEmbedding: number[], k: number): Array<{ id: string; distance: number }> {
  if (!vecDb) return [];

  const queryBuf = Buffer.from(new Float32Array(queryEmbedding).buffer);

  try {
    const rows = vecDb.prepare(`
      SELECT m.id, v.distance
      FROM vec_embeddings v
      JOIN vec_map m ON m.vec_rowid = v.rowid
      WHERE v.embedding MATCH ? AND k = ?
      ORDER BY v.distance
    `).all(queryBuf, k) as Array<{ id: string; distance: number }>;

    return rows;
  } catch (err) {
    logger.warn('Vector KNN search failed', { error: String(err) });
    return [];
  }
}

/** Lock file path for coordinating sync across multiple instances. */
function syncLockPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER, 'sync.lock');
}

/**
 * Try to acquire an exclusive sync lock. Returns true if acquired.
 * Uses a PID-based lock file with stale detection (5 min timeout).
 */
function acquireSyncLock(): boolean {
  const lockPath = syncLockPath();
  try {
    // Check for existing lock
    if (fsSync.existsSync(lockPath)) {
      const content = fsSync.readFileSync(lockPath, 'utf-8').trim();
      const [pidStr, tsStr] = content.split(':');
      const lockPid = parseInt(pidStr ?? '', 10);
      const lockTime = parseInt(tsStr ?? '', 10);

      // Check if lock is stale (> 5 minutes old or owning process is dead)
      const staleMs = 5 * 60 * 1000;
      const isStale = Date.now() - lockTime > staleMs;
      let isAlive = false;
      if (!isNaN(lockPid)) {
        try {
          process.kill(lockPid, 0); // signal 0 = check existence
          isAlive = true;
        } catch {
          isAlive = false;
        }
      }

      if (!isStale && isAlive) {
        return false; // lock held by a live process
      }
      // Stale or dead — remove and claim
    }

    // Write our PID and timestamp atomically via rename
    const tmpPath = lockPath + `.${process.pid}.tmp`;
    fsSync.writeFileSync(tmpPath, `${process.pid}:${Date.now()}`);
    fsSync.renameSync(tmpPath, lockPath);
    return true;
  } catch {
    return false;
  }
}

/** Release the sync lock if we own it. */
function releaseSyncLock(): void {
  const lockPath = syncLockPath();
  try {
    const content = fsSync.readFileSync(lockPath, 'utf-8').trim();
    const [pidStr] = content.split(':');
    if (parseInt(pidStr ?? '', 10) === process.pid) {
      fsSync.unlinkSync(lockPath);
    }
  } catch {
    // Lock already gone or not ours — fine
  }
}

/**
 * Background sync: find vault notes missing embeddings and batch-embed them.
 * Non-blocking — caller does not await. Progress logged to stderr.
 * Uses a lock file so only one instance syncs at a time.
 */
export function syncVectorIndex(): void {
  if (!vecDb) return;

  // Run async in background — don't block server startup
  void (async () => {
    try {
      if (!(await isEmbeddingAvailable())) return;

      if (!acquireSyncLock()) {
        logger.info('Vector sync skipped — another instance is syncing');
        return;
      }

      try {
        const wIndex = getWikiIndex();

        // Categorize vec_map rows against the current wiki index.
        const vecRows = vecDb!.prepare('SELECT id, updated FROM vec_map').all() as Array<{ id: string; updated: string }>;
        const orphans: string[] = [];
        const stale: string[] = [];
        for (const row of vecRows) {
          const wEntry = wIndex.get(row.id);
          if (wEntry) {
            if (wEntry.updated && Date.parse(row.updated) < Date.parse(wEntry.updated)) {
              stale.push(row.id);
            }
            continue;
          }
          orphans.push(row.id);
        }

        // 1. Delete orphans — vectors for entries no longer in the index.
        for (const id of orphans) {
          deleteVector(id);
        }
        if (orphans.length > 0) {
          logger.info('Vector index orphans deleted', { count: orphans.length });
        }

        const embeddedIds = new Set(vecRows.map((r) => r.id));
        const missingWiki = [...wIndex.entries()].filter(([id]) => !embeddedIds.has(id));

        if (missingWiki.length === 0 && stale.length === 0) {
          logger.info('Vector index up to date', { count: embeddedIds.size });
          return;
        }

        // 2. Build the work list: missing + stale wiki pages.
        type Work = { id: string; title: string; tags: string[]; bodyFn: () => Promise<string> };
        const work: Work[] = [
          ...missingWiki.map(([id, wEntry]: [string, WikiIndexEntry]) => ({
            id,
            title: wEntry.title,
            tags: wEntry.tags,
            bodyFn: async () => wEntry.body,
          })),
          ...stale.map((id) => {
            const wEntry = wIndex.get(id)!;
            return { id, title: wEntry.title, tags: wEntry.tags, bodyFn: async () => wEntry.body };
          }),
        ];

        logger.info('Syncing vector index', { missing: missingWiki.length, stale: stale.length, total: wIndex.size });

        const texts = await Promise.all(work.map(async (w) => {
          const body = await w.bodyFn();
          return buildEmbedText(w.title, w.tags, body);
        }));

        const embeddings = await embedBatch(texts);

        // 3. Upsert in batch. upsertVector handles both insert and replace.
        let synced = 0;
        for (let i = 0; i < work.length; i++) {
          const embedding = embeddings[i];
          if (embedding) {
            upsertVector(work[i]!.id, embedding);
            synced++;
          }
        }

        logger.info('Vector index sync complete', {
          synced,
          failed: work.length - synced,
          orphansDeleted: orphans.length,
        });
      } finally {
        releaseSyncLock();
      }
    } catch (err) {
      logger.warn('Vector index sync failed', { error: String(err) });
      releaseSyncLock();
    }
  })().catch((err) => {
    logger.warn('syncVectorIndex: unexpected outer error', { error: String(err) });
    releaseSyncLock();
  });
}
