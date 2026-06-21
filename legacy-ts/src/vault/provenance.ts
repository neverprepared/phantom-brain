/**
 * Read/write _index/provenance.json — the Raw -> Wiki mapping that lets the
 * brain answer "where did this summary come from?" and skip already-known
 * sources by content hash.
 *
 * Format: { [raw_path]: ProvenanceEntry }
 *   raw_path is vault-relative, e.g. "Raw/curated/2026-05-23-foo.md".
 *
 * Writes are atomic and serialized through withFileLock so concurrent
 * synthesizers don't clobber each other.
 */
import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { writeAtomicFile, withFileLock } from './filesystem.js';
import { logger } from '../shared/logger.js';

export interface ProvenanceEntry {
  wiki_pages: string[];       // relative paths, e.g. ["Wiki/summaries/foo.md"]
  synthesized_at: string;     // ISO
  reliability: 'high' | 'medium' | 'low' | 'contested' | 'pending';
  category?: string;
  content_hash: string;
}

// Key: raw_path (e.g. "Raw/curated/2026-05-23-foo.md")
export type ProvenanceMap = Record<string, ProvenanceEntry>;

export function provenanceFilePath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER, CONFIG.PROVENANCE_FILE);
}

export async function readProvenance(): Promise<ProvenanceMap> {
  try {
    const raw = await fs.readFile(provenanceFilePath(), 'utf-8');
    const trimmed = raw.trim();
    if (!trimmed) return {};
    const parsed = JSON.parse(trimmed) as unknown;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as ProvenanceMap;
    }
    logger.warn('provenance.json is not an object; ignoring');
    return {};
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code !== 'ENOENT') {
      logger.warn('Failed to read provenance.json', { error: String(err) });
    }
    return {};
  }
}

export async function writeProvenance(map: ProvenanceMap): Promise<void> {
  const filePath = provenanceFilePath();
  await withFileLock(filePath, async () => {
    await writeAtomicFile(filePath, JSON.stringify(map, null, 2) + '\n');
  });
}

/**
 * Atomically add or replace a single entry in provenance.json.
 * Reads the current file inside the lock so concurrent agents cannot
 * overwrite each other's entries.
 */
export async function upsertProvenanceEntry(rawPath: string, entry: ProvenanceEntry): Promise<ProvenanceMap> {
  const filePath = provenanceFilePath();
  let merged: ProvenanceMap = {};
  await withFileLock(filePath, async () => {
    merged = await readProvenance();
    merged[rawPath] = entry;
    await writeAtomicFile(filePath, JSON.stringify(merged, null, 2) + '\n');
  });
  return merged;
}

/**
 * Atomically delete a single entry from provenance.json.
 * Reads the current file inside the lock so concurrent agents are not affected.
 */
export async function deleteProvenanceEntry(rawPath: string): Promise<void> {
  const filePath = provenanceFilePath();
  await withFileLock(filePath, async () => {
    const map = await readProvenance();
    if (!(rawPath in map)) return;
    delete map[rawPath];
    await writeAtomicFile(filePath, JSON.stringify(map, null, 2) + '\n');
  });
}

/**
 * Check whether a content hash already appears in the provenance map.
 * Async to match the broader provenance API surface (callers will often
 * pair this with reads/writes that are async).
 */
export async function isHashKnown(hash: string, map: ProvenanceMap): Promise<boolean> {
  for (const key of Object.keys(map)) {
    const entry = map[key];
    if (entry && entry.content_hash === hash) return true;
  }
  return false;
}
