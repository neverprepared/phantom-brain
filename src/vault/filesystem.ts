import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { VaultError } from '../shared/errors.js';
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

// ---------------------------------------------------------------------------
// Memory file helpers
// ---------------------------------------------------------------------------

export function memoryFilePath(slug: string): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.MEMORY_FOLDER, `${slug}.md`);
}

export async function writeMemoryFile(filePath: string, content: string): Promise<void> {
  await writeAtomicFile(filePath, content);
  logger.debug('Wrote memory file', { path: filePath });
}

export async function readMemoryFile(filePath: string): Promise<string> {
  try {
    return await fs.readFile(filePath, 'utf-8');
  } catch (err) {
    throw new VaultError(`Failed to read memory file: ${filePath}`, {
      path: filePath,
      error: String(err),
    });
  }
}

export async function deleteMemoryFile(filePath: string): Promise<void> {
  try {
    await fs.unlink(filePath);
    logger.debug('Deleted memory file', { path: filePath });
  } catch (err) {
    throw new VaultError(`Failed to delete memory file: ${filePath}`, {
      path: filePath,
      error: String(err),
    });
  }
}

export async function moveMemoryFile(oldPath: string, newPath: string): Promise<void> {
  const dir = path.dirname(newPath);
  await fs.mkdir(dir, { recursive: true });
  await fs.rename(oldPath, newPath);
  logger.debug('Moved memory file', { from: oldPath, to: newPath });
}

export interface MemoryFileEntry {
  filePath: string;
  slug: string;
  paraFolder: string; // kept for backward compat; always 'Memory' now
}

/**
 * List all atom files in Memory/. Excludes _index.md and any non-.md files.
 * Never walks Input/, Wiki/, Output/, _log/, or _index/.
 */
export async function listAllMemoryFiles(): Promise<MemoryFileEntry[]> {
  const entries: MemoryFileEntry[] = [];
  const dirPath = path.join(CONFIG.VAULT_PATH, CONFIG.MEMORY_FOLDER);

  try {
    const files = await fs.readdir(dirPath);
    for (const file of files) {
      if (!file.endsWith('.md')) continue;
      if (file === CONFIG.INDEX_FILE) continue;
      entries.push({
        filePath: path.join(dirPath, file),
        slug: file.replace(/\.md$/, ''),
        paraFolder: CONFIG.MEMORY_FOLDER,
      });
    }
  } catch {
    // Memory/ may not exist yet on first run
  }

  return entries;
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
// Daily note append (atom creation log — unchanged)
// ---------------------------------------------------------------------------

export async function appendToDaily(line: string): Promise<void> {
  const today = todayDateString();
  const dailyPath = path.join(CONFIG.VAULT_PATH, CONFIG.DAILY_FOLDER, `${today}.md`);

  await withFileLock(dailyPath, async () => {
    let existing = '';
    try {
      existing = await fs.readFile(dailyPath, 'utf-8');
    } catch {
      existing = `# ${today}\n\n`;
    }
    const updated = existing.trimEnd() + '\n' + line + '\n';
    await fs.mkdir(path.dirname(dailyPath), { recursive: true });
    await writeAtomicFile(dailyPath, updated);
  });
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
