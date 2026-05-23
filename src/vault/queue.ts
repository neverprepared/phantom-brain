/**
 * File-based queue for Phase 0 brain synthesis.
 *
 * Queue items live in <vault>/_queue/pending/<timestamp>-<nanoid>.json.
 * Claiming an item atomically renames .json -> .claimed so a single
 * synthesize call processes it. Done items move to _queue/done/.
 *
 * No external lock service: the rename itself is the lock. Two concurrent
 * claimNextItem() calls cannot both rename the same file — one wins, the
 * other gets ENOENT and skips to the next pending entry.
 */
import fs from 'node:fs/promises';
import path from 'node:path';
import { randomBytes } from 'node:crypto';
import { CONFIG } from '../config.js';
import { writeAtomicFile } from './filesystem.js';
import { logger } from '../shared/logger.js';

export interface QueueItem {
  raw_path?: string;        // relative to vault; absent for deferred-fetch items
  source: 'curated' | 'gathered';
  captured_at: string;      // ISO
  title: string;
  source_url?: string;
  format: 'markdown' | 'html' | 'text' | 'pdf';
  content_hash?: string;    // SHA256 hex; absent for deferred-fetch items
  deferred_fetch?: boolean; // if true, brain_synthesize fetches source_url before processing
}

function pendingDir(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.QUEUE_PENDING);
}

function doneDir(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.QUEUE_DONE);
}

function nanoid(): string {
  return randomBytes(6).toString('hex');
}

/**
 * Write a queue item to _queue/pending/<timestamp>-<nanoid>.json.
 * Returns the absolute path of the written file.
 */
export async function enqueueItem(item: QueueItem): Promise<string> {
  await fs.mkdir(pendingDir(), { recursive: true });
  // Use compact ISO timestamp so lexicographic ordering matches chronological
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const name = `${stamp}-${nanoid()}.json`;
  const filePath = path.join(pendingDir(), name);
  await writeAtomicFile(filePath, JSON.stringify(item, null, 2) + '\n');
  logger.debug('Enqueued item', { name, raw_path: item.raw_path });
  return filePath;
}

/**
 * Claim the next pending item atomically by renaming .json -> .claimed.
 * Returns [claimedPath, item] or null if the queue is empty.
 *
 * Concurrent callers race on rename(); the loser sees ENOENT and tries the
 * next candidate, so multiple synthesizers can drain the queue safely.
 */
export async function claimNextItem(): Promise<[string, QueueItem] | null> {
  const dir = pendingDir();
  let entries: string[];
  try {
    entries = await fs.readdir(dir);
  } catch {
    return null;
  }

  // Sort so the oldest pending item is tried first.
  const pending = entries.filter((n) => n.endsWith('.json')).sort();

  for (const name of pending) {
    const fromPath = path.join(dir, name);
    const toPath = path.join(dir, name.replace(/\.json$/, '.claimed'));
    try {
      await fs.rename(fromPath, toPath);
    } catch {
      // Another worker grabbed this one (ENOENT) — try the next candidate.
      continue;
    }
    try {
      const raw = await fs.readFile(toPath, 'utf-8');
      const item = JSON.parse(raw) as QueueItem;
      return [toPath, item];
    } catch (err) {
      // The claim file is corrupt. Move it out of the way so we don't loop on it.
      logger.warn('Corrupt queue item; moving to done as poison pill', { name, error: String(err) });
      try {
        await fs.mkdir(doneDir(), { recursive: true });
        await fs.rename(toPath, path.join(doneDir(), `${name}.corrupt`));
      } catch { /* best-effort */ }
      continue;
    }
  }

  return null;
}

/**
 * Mark a claimed item as done by moving it into _queue/done/.
 */
export async function markDone(claimedPath: string): Promise<void> {
  const name = path.basename(claimedPath).replace(/\.claimed$/, '.done.json');
  const target = path.join(doneDir(), name);
  await fs.mkdir(doneDir(), { recursive: true });
  await fs.rename(claimedPath, target);
  logger.debug('Marked queue item done', { name });
}

/**
 * Unclaim a previously claimed item by renaming .claimed back to .json,
 * making it visible to future claimNextItem() calls. Used when synthesis
 * fails after a successful claim.
 */
export async function unclaimItem(claimedPath: string): Promise<void> {
  const restored = claimedPath.replace(/\.claimed$/, '.json');
  await fs.rename(claimedPath, restored);
  logger.debug('Unclaimed queue item', { path: restored });
}
