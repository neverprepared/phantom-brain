import fs from 'node:fs/promises';
import path from 'node:path';
import matter from 'gray-matter';
import type { LifecycleStatus, Status } from '../schemas/frontmatter.js';
import { listAllWikiFiles } from './filesystem.js';
import { logger } from '../shared/logger.js';
import { embedText, buildEmbedText } from './embeddings.js';
import { isVectorIndexReady, searchVectors, upsertVector, deleteVector } from './vector-index.js';
import { isFtsReady, rebuildFts, searchFts, upsertFts, deleteFts } from './fts-index.js';

export interface WikiIndexEntry {
  relPath: string;
  title: string;
  kind: string;
  tags: string[];
  created: string;
  updated: string;
  body: string;
}

let wikiIndex: Map<string, WikiIndexEntry> = new Map();

export async function buildIndex(): Promise<void> {
  const newWikiIndex = new Map<string, WikiIndexEntry>();
  const ftsEntries: Array<{ id: string; title: string; tags: string[]; body: string }> = [];

  const wikiFiles = await listAllWikiFiles();
  for (const { filePath, relPath } of wikiFiles) {
    try {
      const raw = await fs.readFile(filePath, 'utf-8');
      const { data, content } = matter(raw);
      const wEntry: WikiIndexEntry = {
        relPath,
        title: typeof data['title'] === 'string' ? data['title'] : path.basename(relPath, '.md'),
        kind: typeof data['kind'] === 'string' ? data['kind'] : 'reference',
        tags: Array.isArray(data['tags']) ? (data['tags'] as string[]) : [],
        created: typeof data['created'] === 'string' ? data['created'] : '',
        updated: typeof data['updated'] === 'string' ? data['updated'] : '',
        body: content.trim(),
      };
      const id = `wiki:${relPath}`;
      newWikiIndex.set(id, wEntry);
      ftsEntries.push({ id, title: wEntry.title, tags: wEntry.tags, body: wEntry.body });
    } catch (err) {
      logger.warn('Failed to index wiki file', { relPath, error: String(err) });
    }
  }

  wikiIndex = newWikiIndex;

  if (isFtsReady()) {
    rebuildFts(ftsEntries);
  }

  logger.info('Wiki index built', { wiki: newWikiIndex.size });
}

export function getWikiIndex(): Map<string, WikiIndexEntry> {
  return wikiIndex;
}

export function indexWikiEntry(relPath: string, title: string, kind: string, tags: string[], body: string, updated: string, created: string): void {
  const id = `wiki:${relPath}`;
  wikiIndex.set(id, { relPath, title, kind, tags, created, updated, body });
  upsertFts(id, title, tags, body);
  void embedText(buildEmbedText(title, tags, body)).then((embedding) => {
    if (embedding) upsertVector(id, embedding);
  });
}

export function removeWikiFromIndex(relPath: string): void {
  const id = `wiki:${relPath}`;
  wikiIndex.delete(id);
  deleteFts(id);
  deleteVector(id);
}

export interface DateFilters {
  created_after?: string;
  created_before?: string;
  updated_after?: string;
  updated_before?: string;
}

export interface SearchOptions extends DateFilters {
  query?: string;
  tags?: string[];
  tag_mode?: 'and' | 'or';
  exclude_tags?: string[];
  lifecycle_status?: LifecycleStatus;
  status?: Status;
  freshness?: 'all' | 'fresh' | 'stale';
  sort_by?: 'relevance' | 'created' | 'updated' | 'title';
  limit: number;
  search_mode?: 'auto' | 'keyword' | 'vector';
}

export type SearchResultKind = 'atom' | 'wiki';

export interface SearchResult {
  resultKind: SearchResultKind;
  wikiEntry?: WikiIndexEntry;
  score: number;
  snippet?: string;
  stale: boolean;
}

function passesWikiFilters(wEntry: WikiIndexEntry, options: Omit<SearchOptions, 'query' | 'limit'>): boolean {
  if (options.freshness === 'stale') return false;
  if (options.lifecycle_status) return false;
  if (options.status) return false;

  if (options.tags && options.tags.length > 0) {
    const mode = options.tag_mode ?? 'and';
    const entryTagsLower = wEntry.tags.map((t) => t.toLowerCase());
    const filterTagsLower = options.tags.map((t) => t.toLowerCase());
    if (mode === 'and' && !filterTagsLower.every((t) => entryTagsLower.includes(t))) return false;
    if (mode === 'or' && !filterTagsLower.some((t) => entryTagsLower.includes(t))) return false;
  }

  if (options.exclude_tags && options.exclude_tags.length > 0) {
    const entryTagsLower = wEntry.tags.map((t) => t.toLowerCase());
    const excludeLower = options.exclude_tags.map((t) => t.toLowerCase());
    if (excludeLower.some((t) => entryTagsLower.includes(t))) return false;
  }

  if (options.created_after && wEntry.created < options.created_after) return false;
  if (options.created_before && wEntry.created > options.created_before) return false;
  if (options.updated_after && wEntry.updated < options.updated_after) return false;
  if (options.updated_before && wEntry.updated > options.updated_before) return false;

  return true;
}

function applySortBy(results: SearchResult[], sortBy: string | undefined): void {
  if (!sortBy || sortBy === 'relevance') return;
  const getField = (r: SearchResult, field: 'created' | 'updated' | 'title'): string =>
    r.wikiEntry?.[field] ?? '';
  const sortFns: Record<string, (a: SearchResult, b: SearchResult) => number> = {
    created: (a, b) => getField(b, 'created').localeCompare(getField(a, 'created')),
    updated: (a, b) => getField(b, 'updated').localeCompare(getField(a, 'updated')),
    title: (a, b) => getField(a, 'title').localeCompare(getField(b, 'title')),
  };
  const fn = sortFns[sortBy];
  if (fn) results.sort(fn);
}

export async function searchMemories(options: SearchOptions): Promise<SearchResult[]> {
  const mode = options.search_mode ?? 'auto';
  const useVector = mode !== 'keyword' && Boolean(options.query) && isVectorIndexReady();

  let results: SearchResult[];
  if (useVector && options.query) {
    results = await hybridSearch(options, options.query);
  } else {
    results = keywordSearch(options);
  }

  applySortBy(results, options.sort_by);
  return results;
}

async function hybridSearch(options: SearchOptions, query: string): Promise<SearchResult[]> {
  const t0 = performance.now();
  const queryEmbedding = await embedText(query);
  const tEmbed = performance.now();
  if (!queryEmbedding) return keywordSearch(options);

  const WINDOW = Math.max(options.limit * 10, 50);
  const RRF_K = 10;

  const vectorHits = searchVectors(queryEmbedding, WINDOW);
  const tVector = performance.now();
  const ftsHits = isFtsReady() ? searchFts(query, WINDOW) : [];
  const tFts = performance.now();

  const vectorRank = new Map(vectorHits.map((r, i) => [r.id, i + 1]));
  const ftsRank = new Map(ftsHits.map((r, i) => [r.id, i + 1]));
  const ftsSnippet = new Map(ftsHits.map((r) => [r.id, r.snippet]));

  const candidateIds = new Set<string>([
    ...vectorHits.map((r) => r.id),
    ...ftsHits.map((r) => r.id),
  ]);

  const candidates: SearchResult[] = [];

  for (const id of candidateIds) {
    const vr = vectorRank.get(id);
    const fr = ftsRank.get(id);
    let rrf = 0;
    if (vr) rrf += 1 / (RRF_K + vr);
    if (fr) rrf += 1 / (RRF_K + fr);
    const score = Math.round(rrf * 10000) / 10000;
    const snippet = ftsSnippet.get(id) || undefined;

    const wEntry = wikiIndex.get(id);
    if (wEntry) {
      if (!passesWikiFilters(wEntry, options)) continue;
      candidates.push({ resultKind: 'wiki', wikiEntry: wEntry, score, snippet, stale: false });
    }
  }

  candidates.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score;
    return (b.wikiEntry?.updated ?? '').localeCompare(a.wikiEntry?.updated ?? '');
  });

  logger.debug('hybridSearch timing', {
    query,
    embedMs: Math.round(tEmbed - t0),
    vectorMs: Math.round(tVector - tEmbed),
    ftsMs: Math.round(tFts - tVector),
    mergeMs: Math.round(performance.now() - tFts),
    candidates: candidates.length,
    vectorHits: vectorHits.length,
    ftsHits: ftsHits.length,
  });

  return candidates.slice(0, options.limit);
}

function keywordSearch(options: SearchOptions): SearchResult[] {
  if (options.query && isFtsReady()) {
    return ftsKeywordSearch(options, options.query);
  }

  const results: SearchResult[] = [];
  const queryLower = options.query?.toLowerCase();

  for (const wEntry of wikiIndex.values()) {
    if (!passesWikiFilters(wEntry, options)) continue;
    if (queryLower) {
      let score = 0;
      if (wEntry.title.toLowerCase().includes(queryLower)) score += 10;
      if (wEntry.tags.some((t) => t.toLowerCase().includes(queryLower))) score += 5;
      if (wEntry.body.toLowerCase().includes(queryLower)) score += 1;
      if (score === 0) continue;
      results.push({ resultKind: 'wiki', wikiEntry: wEntry, score, stale: false });
    } else {
      results.push({ resultKind: 'wiki', wikiEntry: wEntry, score: 1, stale: false });
    }
  }

  results.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score;
    return (b.wikiEntry?.updated ?? '').localeCompare(a.wikiEntry?.updated ?? '');
  });

  return results.slice(0, options.limit);
}

function ftsKeywordSearch(options: SearchOptions, query: string): SearchResult[] {
  const ftsHits = searchFts(query, options.limit * 5);
  const results: SearchResult[] = [];

  for (const hit of ftsHits) {
    const wEntry = wikiIndex.get(hit.id);
    if (wEntry) {
      if (!passesWikiFilters(wEntry, options)) continue;
      results.push({ resultKind: 'wiki', wikiEntry: wEntry, score: hit.rank, snippet: hit.snippet || undefined, stale: false });
      if (results.length >= options.limit) break;
    }
  }

  return results;
}
