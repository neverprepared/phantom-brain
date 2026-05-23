import { logger } from '../shared/logger.js';
import type Database from 'better-sqlite3';

let db: Database.Database | null = null;

/**
 * Initialize FTS5 virtual table in the given SQLite database.
 * Reuses the same DB instance as the vector index.
 */
export function initFts(sqliteDb: Database.Database): void {
  db = sqliteDb;
  try {
    db.exec(`
      CREATE VIRTUAL TABLE IF NOT EXISTS fts_memories
      USING fts5(id UNINDEXED, title, tags, body, tokenize='porter unicode61');
    `);
    logger.info('FTS5 index initialized');
  } catch (err) {
    logger.warn('FTS5 init failed — full-text search disabled', { error: String(err) });
    db = null;
  }
}

export function isFtsReady(): boolean {
  return db !== null;
}

/** Insert or replace an FTS entry. Tags are stored as space-separated for tokenization. */
export function upsertFts(id: string, title: string, tags: string[], body: string): void {
  if (!db) return;
  const tagStr = tags.join(' ');
  // FTS5 doesn't support UPSERT — delete then insert.
  // Wrapped in a transaction so concurrent readers never see a missing entry
  // between the DELETE and INSERT.
  const tx = db.transaction(() => {
    db!.prepare('DELETE FROM fts_memories WHERE id = ?').run(id);
    db!.prepare(
      'INSERT INTO fts_memories (id, title, tags, body) VALUES (?, ?, ?, ?)'
    ).run(id, title, tagStr, body);
  });
  tx();
}

/** Remove an FTS entry by memory id. */
export function deleteFts(id: string): void {
  if (!db) return;
  db.prepare('DELETE FROM fts_memories WHERE id = ?').run(id);
}

export interface FtsResult {
  id: string;
  rank: number;
  snippet: string;
}

/**
 * Search FTS5 index. Returns scored results sorted by relevance.
 * Uses FTS5 rank (BM25) and snippet extraction.
 */
export function searchFts(query: string, limit: number): FtsResult[] {
  if (!db) return [];

  // Sanitize query for FTS5: escape double quotes, wrap terms for prefix matching
  const sanitized = sanitizeFtsQuery(query);
  if (!sanitized) return [];

  try {
    // Weighted BM25: title (10) > tags (5) > body (1) to preserve legacy ordering.
    const stmt = db.prepare(`
      SELECT id, bm25(fts_memories, 4.0, 3.0, 1.0) as rank,
        snippet(fts_memories, 3, '>>>', '<<<', '...', 64) as snippet
      FROM fts_memories
      WHERE fts_memories MATCH ?
      ORDER BY rank
      LIMIT ?
    `);
    const rows = stmt.all(sanitized, limit) as Array<{
      id: string;
      rank: number;
      snippet: string;
    }>;

    return rows.map((r) => ({
      id: r.id,
      rank: -r.rank, // FTS5 rank is negative (closer to 0 = better), flip for intuitive scoring
      snippet: r.snippet,
    }));
  } catch (err) {
    logger.warn('FTS5 search failed, falling back', { query, error: String(err) });
    return [];
  }
}

/** Rebuild the entire FTS index from scratch. Called during buildIndex(). */
export function rebuildFts(
  entries: Array<{ id: string; title: string; tags: string[]; body: string }>
): void {
  if (!db) return;

  const tx = db.transaction(() => {
    db!.prepare('DELETE FROM fts_memories').run();
    const insert = db!.prepare(
      'INSERT INTO fts_memories (id, title, tags, body) VALUES (?, ?, ?, ?)'
    );
    for (const e of entries) {
      insert.run(e.id, e.title, e.tags.join(' '), e.body);
    }
  });

  tx();
  logger.info('FTS5 index rebuilt', { count: entries.length });
}

/**
 * Sanitize a user query for FTS5 MATCH syntax.
 * Joins terms with implicit AND. Prefix matching is opt-in via trailing *.
 * Strips FTS5 special characters to prevent syntax errors.
 */
function sanitizeFtsQuery(query: string): string {
  // Preserve trailing * on terms for explicit prefix matching, strip everything else
  const cleaned = query.replace(/[":(){}[\]^~+\-!/\\]/g, ' ').trim();
  if (!cleaned) return '';

  const terms = cleaned
    .split(/\s+/)
    .filter((t) => t.length > 0)
    .map((t) => {
      const isPrefix = t.endsWith('*');
      const word = isPrefix ? t.slice(0, -1) : t;
      if (!word) return '';
      // Quote for safety, add * only if user explicitly requested prefix
      return isPrefix ? `"${word}"*` : `"${word}"`;
    })
    .filter((t) => t.length > 0);

  return terms.join(' ');
}
