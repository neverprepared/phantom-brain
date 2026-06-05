import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';
import { todayDateString } from '../shared/utils.js';

// ---------------------------------------------------------------------------
// Per-file mutex — prevents read-modify-write races on concurrent tool calls
// ---------------------------------------------------------------------------

const fileLocks = new Map<string, Promise<void>>();

export async function withFileLock<T>(filePath: string, fn: () => Promise<T>): Promise<T> {
  const prev = fileLocks.get(filePath) ?? Promise.resolve();
  let resolve!: () => void;
  const next = new Promise<void>((r) => { resolve = r; });
  fileLocks.set(filePath, next);
  try {
    await prev;
    return await fn();
  } finally {
    resolve();
    if (fileLocks.get(filePath) === next) fileLocks.delete(filePath);
  }
}

// ---------------------------------------------------------------------------
// Atomic file write — write to .tmp then rename (crash-safe)
// ---------------------------------------------------------------------------

export async function writeAtomicFile(filePath: string, content: string): Promise<void> {
  const dir = path.dirname(filePath);
  await fs.mkdir(dir, { recursive: true });
  const tmp = `${filePath}.tmp`;
  await fs.writeFile(tmp, content, 'utf-8');
  await fs.rename(tmp, filePath);
}

export interface WikiFileEntry {
  filePath: string;
  relPath: string; // relative to Wiki/, e.g. "HowTos/oauth-setup.md"
}

/**
 * List all wiki page files in Wiki/ subfolders. Excludes _index.md,
 * CLAUDE.md, and non-.md files. Never walks _attachments/.
 */
export async function listAllWikiFiles(): Promise<WikiFileEntry[]> {
  const entries: WikiFileEntry[] = [];
  const root = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER);

  try {
    const dirs = await fs.readdir(root, { withFileTypes: true });
    for (const dir of dirs) {
      if (!dir.isDirectory() || dir.name.startsWith('_')) continue;
      const subDir = path.join(root, dir.name);
      try {
        const files = await fs.readdir(subDir);
        for (const file of files) {
          if (!file.endsWith('.md') || file === CONFIG.INDEX_FILE) continue;
          entries.push({
            filePath: path.join(subDir, file),
            relPath: `${dir.name}/${file}`,
          });
        }
      } catch { /* skip unreadable subdir */ }
    }
  } catch { /* Wiki/ may not exist yet */ }

  return entries;
}

// ---------------------------------------------------------------------------
// Provenance log — daily-rotated, append-only, server-internal
// ---------------------------------------------------------------------------

/**
 * Append one line to today's _log/<date>.md.
 * Uses fs.appendFile (single syscall, no read-modify-write race).
 * This is server-internal — not exposed as an MCP tool.
 */
export async function appendToLog(line: string): Promise<void> {
  const today = todayDateString();
  const logDir = path.join(CONFIG.VAULT_PATH, CONFIG.LOG_FOLDER);
  const logPath = path.join(logDir, `${today}.md`);

  try {
    await fs.mkdir(logDir, { recursive: true });
    const timestamp = new Date().toISOString().substring(11, 16) + 'Z'; // HH:MMZ
    await fs.appendFile(logPath, `${timestamp} — ${line}\n`, 'utf-8');
  } catch (err) {
    logger.warn('Failed to append to log', { error: String(err) });
  }
}

// ---------------------------------------------------------------------------
// Process liveness check (for wm-<PID>.sqlite orphan detection)
// ---------------------------------------------------------------------------

export function isProcessAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}
