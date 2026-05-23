import { searchMemories } from '../vault/search.js';

/**
 * Returns the atom ID of a near-duplicate if one exists, otherwise undefined.
 * Uses the hybrid FTS5/vector search with a high-score cutoff. The threshold
 * was picked empirically — FTS5 BM25 scores well over 8 generally indicate
 * the candidate's title and body overlap heavily with the query.
 */
export async function findNearDuplicate(content: string, title: string): Promise<string | undefined> {
  const query = (title + ' ' + content).slice(0, 120).replace(/[^a-zA-Z0-9\s]/g, ' ').trim();
  if (!query) return undefined;
  const results = await searchMemories({ query, limit: 3 });
  for (const r of results) {
    if (r.resultKind === 'atom' && r.entry && r.score >= 8) {
      return r.entry.frontmatter.id;
    }
  }
  return undefined;
}
