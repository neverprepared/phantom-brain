/**
 * Rename journal: a single JSON file at `_index/rename-journal.json` recording
 * an in-flight slug rename. Written before any disk changes; deleted after the
 * backlink rewrite batch completes. If the process crashes between those two
 * points, the next startup re-runs the rename via the recovery path.
 *
 * The journal only stores `{from, to, started}` — recovery just re-invokes
 * renameSlugReferences, which is idempotent (files without `[[from]]` refs are
 * skipped). No need to track per-file completion state.
 */

import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';

export interface RenameJournal {
  from: string;
  to: string;
  started: string;
}

function journalPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INDEX_FOLDER, 'rename-journal.json');
}

export async function writeRenameJournal(from: string, to: string): Promise<void> {
  const journal: RenameJournal = { from, to, started: new Date().toISOString() };
  const target = journalPath();
  const tmp = `${target}.tmp`;
  await fs.mkdir(path.dirname(target), { recursive: true });
  await fs.writeFile(tmp, JSON.stringify(journal, null, 2), 'utf-8');
  await fs.rename(tmp, target);
}

export async function readRenameJournal(): Promise<RenameJournal | null> {
  try {
    const raw = await fs.readFile(journalPath(), 'utf-8');
    const parsed = JSON.parse(raw) as Partial<RenameJournal>;
    if (typeof parsed.from === 'string' && typeof parsed.to === 'string' && typeof parsed.started === 'string') {
      return parsed as RenameJournal;
    }
    logger.warn('Rename journal malformed, ignoring', { contents: raw.slice(0, 200) });
    return null;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null;
    logger.warn('Failed to read rename journal', { error: String(err) });
    return null;
  }
}

export async function deleteRenameJournal(): Promise<void> {
  try {
    await fs.unlink(journalPath());
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== 'ENOENT') {
      logger.warn('Failed to delete rename journal', { error: String(err) });
    }
  }
}
