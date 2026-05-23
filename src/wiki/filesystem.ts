import fs from 'node:fs/promises';
import path from 'node:path';
import matter from 'gray-matter';
import { CONFIG } from '../config.js';
import { VaultError, ValidationError } from '../shared/errors.js';
import { writeAtomicFile, withFileLock, appendToLog } from '../vault/filesystem.js';
import { indexWikiEntry, removeWikiFromIndex } from '../vault/search.js';
import { nowISO } from '../shared/utils.js';
import type { WikiEntry } from './types.js';

// ---------------------------------------------------------------------------
// Path helpers
// ---------------------------------------------------------------------------

function wikiRoot(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER);
}

function absoluteWikiPath(relPath: string): string {
  return path.join(wikiRoot(), relPath);
}

function subfolderIndexPath(subfolder: string): string {
  return path.join(wikiRoot(), subfolder, CONFIG.INDEX_FILE);
}

function rootIndexPath(): string {
  return path.join(wikiRoot(), CONFIG.INDEX_FILE);
}

// ---------------------------------------------------------------------------
// Path validation
// ---------------------------------------------------------------------------

function validateRelPath(relPath: string): void {
  if (path.isAbsolute(relPath)) {
    throw new ValidationError('Wiki relPath must not be absolute', { relPath });
  }
  if (relPath.includes('..')) {
    throw new ValidationError('Wiki relPath must not contain ..', { relPath });
  }
  if (relPath.startsWith('_attachments')) {
    throw new ValidationError('Wiki relPath must not start with _attachments', { relPath });
  }
}

function relPathToWikiLink(relPath: string): string {
  // Strip .md extension for wiki link
  return relPath.replace(/\.md$/, '');
}

function extractSlug(relPath: string): string {
  return path.basename(relPath, '.md');
}

function extractSubfolder(relPath: string): string {
  const parts = relPath.split('/');
  return parts.length > 1 ? parts[0]! : '';
}

// ---------------------------------------------------------------------------
// Parse frontmatter from wiki file
// ---------------------------------------------------------------------------

function parseWikiEntry(relPath: string, raw: string): Partial<WikiEntry> {
  try {
    const { data } = matter(raw);
    return {
      relPath,
      subfolder: extractSubfolder(relPath),
      slug: extractSlug(relPath),
      title: typeof data['title'] === 'string' ? data['title'] : extractSlug(relPath),
      kind: data['kind'] ?? 'reference',
      tags: Array.isArray(data['tags']) ? data['tags'] : [],
      created: typeof data['created'] === 'string' ? data['created'] : '',
      updated: typeof data['updated'] === 'string' ? data['updated'] : '',
      sources: Array.isArray(data['sources']) ? data['sources'] : [],
    };
  } catch {
    return {
      relPath,
      subfolder: extractSubfolder(relPath),
      slug: extractSlug(relPath),
    };
  }
}

// ---------------------------------------------------------------------------
// Index management
// ---------------------------------------------------------------------------

const INDEX_HEADER = `| File | Kind | Title | Updated |
| ---- | ---- | ----- | ------- |`;

function buildIndexRow(relPath: string, kind: string, title: string, updated: string): string {
  const link = relPathToWikiLink(relPath);
  const date = updated ? updated.substring(0, 10) : nowISO().substring(0, 10);
  return `| [[${link}]] | ${kind} | ${title} | ${date} |`;
}

/**
 * Parse index table from existing _index.md content.
 * Returns the preamble (everything before the table), the table rows (excluding header),
 * and the postamble (everything after the table).
 */
function parseIndexContent(content: string): {
  preamble: string;
  rows: string[];
  postamble: string;
} {
  const lines = content.split('\n');
  let tableStart = -1;
  let tableEnd = -1;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!.trim();
    if (line.startsWith('| File |') || line.startsWith('| [[')) {
      if (tableStart === -1) {
        tableStart = i;
      }
      tableEnd = i;
    } else if (tableStart !== -1 && line.startsWith('|')) {
      tableEnd = i;
    } else if (tableStart !== -1 && tableEnd !== -1 && line !== '' && !line.startsWith('|')) {
      break;
    }
  }

  if (tableStart === -1) {
    // No table found
    return { preamble: content, rows: [], postamble: '' };
  }

  const preamble = lines.slice(0, tableStart).join('\n');
  const tableLines = lines.slice(tableStart, tableEnd + 1);
  const postamble = lines.slice(tableEnd + 1).join('\n');

  // Extract data rows (skip header row and separator row)
  const rows = tableLines.filter((l) => {
    const trimmed = l.trim();
    return trimmed.startsWith('| [[') || (trimmed.startsWith('|') && !trimmed.includes('File |') && !trimmed.match(/^\|[-| ]+\|$/));
  });

  return { preamble, rows, postamble };
}

function buildIndexContent(title: string, rows: string[]): string {
  const header = `# ${title}\n\n`;
  if (rows.length === 0) {
    return header + INDEX_HEADER + '\n';
  }
  return header + INDEX_HEADER + '\n' + rows.join('\n') + '\n';
}

async function readIndexContent(indexPath: string): Promise<string> {
  try {
    return await fs.readFile(indexPath, 'utf-8');
  } catch {
    return '';
  }
}

/**
 * Upsert a row in the given _index.md for the given relPath.
 */
async function upsertIndexRow(
  indexPath: string,
  indexTitle: string,
  relPath: string,
  kind: string,
  title: string,
  updated: string,
): Promise<void> {
  await withFileLock(indexPath, async () => {
    const existing = await readIndexContent(indexPath);
    const newRow = buildIndexRow(relPath, kind, title, updated);
    const link = relPathToWikiLink(relPath);

    const { rows } = existing.trim() === '' ? { rows: [] as string[] } : parseIndexContent(existing);

    // Remove any existing row for this file (match by [[link]])
    const filtered = rows.filter((r) => !r.includes(`[[${link}]]`));
    filtered.push(newRow);
    filtered.sort();

    const tableContent = buildIndexContent(indexTitle, filtered);

    await fs.mkdir(path.dirname(indexPath), { recursive: true });
    await writeAtomicFile(indexPath, tableContent);
  });
}

/**
 * Remove a row from the given _index.md for the given relPath.
 */
async function removeIndexRow(indexPath: string, indexTitle: string, relPath: string): Promise<void> {
  await withFileLock(indexPath, async () => {
    const existing = await readIndexContent(indexPath);
    if (!existing.trim()) return;

    const link = relPathToWikiLink(relPath);
    const { rows } = parseIndexContent(existing);
    const filtered = rows.filter((r) => !r.includes(`[[${link}]]`));

    await writeAtomicFile(indexPath, buildIndexContent(indexTitle, filtered));
  });
}

/**
 * Rename a row in the given _index.md (from old relPath to new relPath).
 */
async function renameIndexRow(
  indexPath: string,
  indexTitle: string,
  oldRelPath: string,
  newRelPath: string,
  kind: string,
  title: string,
  updated: string,
): Promise<void> {
  await withFileLock(indexPath, async () => {
    const existing = await readIndexContent(indexPath);
    const oldLink = relPathToWikiLink(oldRelPath);
    const newRow = buildIndexRow(newRelPath, kind, title, updated);

    const { rows } = existing.trim() ? parseIndexContent(existing) : { rows: [] as string[] };

    const filtered = rows.filter((r) => !r.includes(`[[${oldLink}]]`));
    const newLink = relPathToWikiLink(newRelPath);
    const existingNew = filtered.filter((r) => !r.includes(`[[${newLink}]]`));
    existingNew.push(newRow);
    existingNew.sort();

    await fs.mkdir(path.dirname(indexPath), { recursive: true });
    await writeAtomicFile(indexPath, buildIndexContent(indexTitle, existingNew));
  });
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export async function readWikiFile(relPath: string): Promise<{ content: string; entry: Partial<WikiEntry> }> {
  validateRelPath(relPath);
  const absPath = absoluteWikiPath(relPath);
  let raw: string;
  try {
    raw = await fs.readFile(absPath, 'utf-8');
  } catch (err) {
    throw new VaultError(`Wiki file not found: ${relPath}`, { relPath, error: String(err) });
  }
  const { content } = matter(raw);
  const entry = parseWikiEntry(relPath, raw);
  return { content: content.trim(), entry };
}

export async function listWiki(subfolder?: string): Promise<WikiEntry[]> {
  const entries: WikiEntry[] = [];
  const root = wikiRoot();

  // Try reading from _index.md first for speed (root index only when listing all)
  // For subfolder listing, read the subfolder _index.md
  // Fall back to directory scan if index doesn't exist or is empty

  if (subfolder) {
    validateRelPath(subfolder + '/placeholder');
    const subDir = path.join(root, subfolder);
    try {
      const files = await fs.readdir(subDir);
      for (const file of files) {
        if (!file.endsWith('.md') || file === CONFIG.INDEX_FILE) continue;
        const relPath = `${subfolder}/${file}`;
        const absPath = path.join(subDir, file);
        try {
          const raw = await fs.readFile(absPath, 'utf-8');
          const entry = parseWikiEntry(relPath, raw);
          if (isCompleteEntry(entry)) {
            entries.push(entry as WikiEntry);
          }
        } catch {
          // Skip unreadable files
        }
      }
    } catch {
      // Subfolder may not exist
    }
  } else {
    // List all subfolders
    try {
      const topLevelEntries = await fs.readdir(root, { withFileTypes: true });
      for (const dirent of topLevelEntries) {
        if (!dirent.isDirectory() || dirent.name.startsWith('_')) continue;
        const subDir = path.join(root, dirent.name);
        try {
          const files = await fs.readdir(subDir);
          for (const file of files) {
            if (!file.endsWith('.md') || file === CONFIG.INDEX_FILE) continue;
            const relPath = `${dirent.name}/${file}`;
            const absPath = path.join(subDir, file);
            try {
              const raw = await fs.readFile(absPath, 'utf-8');
              const entry = parseWikiEntry(relPath, raw);
              if (isCompleteEntry(entry)) {
                entries.push(entry as WikiEntry);
              }
            } catch {
              // Skip unreadable files
            }
          }
        } catch {
          // Skip unreadable subdirs
        }
      }
    } catch {
      // Wiki root may not exist
    }
  }

  return entries;
}

function isCompleteEntry(entry: Partial<WikiEntry>): entry is WikiEntry {
  return (
    typeof entry.relPath === 'string' &&
    typeof entry.subfolder === 'string' &&
    typeof entry.slug === 'string' &&
    typeof entry.title === 'string' &&
    typeof entry.kind === 'string' &&
    Array.isArray(entry.tags) &&
    typeof entry.created === 'string' &&
    typeof entry.updated === 'string' &&
    Array.isArray(entry.sources)
  );
}

export async function searchWiki(query: string): Promise<string[]> {
  const lowerQuery = query.toLowerCase();
  const results: string[] = [];
  const root = wikiRoot();

  // Title-only search: check _index.md rows first (fast path)
  const rootIndex = await readIndexContent(rootIndexPath());
  if (rootIndex.trim()) {
    const { rows } = parseIndexContent(rootIndex);
    for (const row of rows) {
      // Row format: | [[HowTos/oauth-setup]] | kind | Title | date |
      const match = row.match(/\[\[([^\]]+)\]\]/);
      const titleMatch = row.match(/\| [^|]+ \| [^|]+ \| ([^|]+) \|/);
      if (match) {
        const link = match[1]!;
        const title = titleMatch ? titleMatch[1]!.trim() : '';
        const slug = path.basename(link);
        if (
          slug.toLowerCase().includes(lowerQuery) ||
          title.toLowerCase().includes(lowerQuery)
        ) {
          results.push(`${link}.md`);
        }
      }
    }
    if (results.length > 0) {
      return results;
    }
  }

  // Fallback: directory scan with filename matching
  try {
    const topLevelEntries = await fs.readdir(root, { withFileTypes: true });
    for (const dirent of topLevelEntries) {
      if (!dirent.isDirectory() || dirent.name.startsWith('_')) continue;
      const subDir = path.join(root, dirent.name);
      try {
        const files = await fs.readdir(subDir);
        for (const file of files) {
          if (!file.endsWith('.md') || file === CONFIG.INDEX_FILE) continue;
          const slug = file.replace(/\.md$/, '');
          const relPath = `${dirent.name}/${file}`;
          if (slug.toLowerCase().includes(lowerQuery)) {
            results.push(relPath);
            continue;
          }
          // Read title from frontmatter
          try {
            const absPath = path.join(subDir, file);
            const raw = await fs.readFile(absPath, 'utf-8');
            const { data } = matter(raw);
            const title = typeof data['title'] === 'string' ? data['title'] : '';
            if (title.toLowerCase().includes(lowerQuery)) {
              results.push(relPath);
            }
          } catch {
            // Skip unreadable
          }
        }
      } catch {
        // Skip
      }
    }
  } catch {
    // Wiki root may not exist
  }

  return results;
}

export interface WriteWikiParams {
  relPath: string;
  mode: 'create' | 'update';
  content: string;
  title: string;
  kind: 'howto' | 'runbook' | 'reference' | 'scratch';
  tags?: string[];
  sources?: string[];
}

export async function writeWikiFile(params: WriteWikiParams): Promise<void> {
  const { relPath, mode, content, title, kind, tags = [], sources = [] } = params;
  validateRelPath(relPath);

  const absPath = absoluteWikiPath(relPath);
  const exists = await fs.access(absPath).then(() => true).catch(() => false);

  if (mode === 'create' && exists) {
    throw new ValidationError(`Wiki file already exists: ${relPath}. Use mode 'update' to modify it.`, { relPath });
  }
  if (mode === 'update' && !exists) {
    throw new VaultError(`Wiki file not found: ${relPath}. Use mode 'create' to create it.`, { relPath });
  }

  const now = nowISO();
  let created = now;

  if (mode === 'update') {
    // Preserve original created date
    try {
      const existing = await fs.readFile(absPath, 'utf-8');
      const { data } = matter(existing);
      if (typeof data['created'] === 'string') {
        created = data['created'];
      }
    } catch {
      // Fall through, use now
    }
  }

  const frontmatter = {
    title,
    kind,
    tags,
    created,
    updated: now,
    sources,
  };

  const fileContent = matter.stringify(content, frontmatter);

  await fs.mkdir(path.dirname(absPath), { recursive: true });
  await writeAtomicFile(absPath, fileContent);

  // Keep search index in sync
  indexWikiEntry(relPath, title, kind, tags, content, now, created);

  const subfolder = extractSubfolder(relPath);

  // Update subfolder _index.md
  if (subfolder) {
    await upsertIndexRow(
      subfolderIndexPath(subfolder),
      subfolder,
      relPath,
      kind,
      title,
      now,
    );
  }

  // Update root Wiki/_index.md
  await upsertIndexRow(
    rootIndexPath(),
    CONFIG.WIKI_FOLDER,
    relPath,
    kind,
    title,
    now,
  );

  // Append to log
  await appendToLog(`wiki ${mode}: ${relPath} — "${title}"`);
}

export async function deleteWikiFile(relPath: string): Promise<void> {
  validateRelPath(relPath);

  const absPath = absoluteWikiPath(relPath);
  try {
    await fs.unlink(absPath);
  } catch (err) {
    throw new VaultError(`Failed to delete wiki file: ${relPath}`, { relPath, error: String(err) });
  }

  removeWikiFromIndex(relPath);

  const subfolder = extractSubfolder(relPath);

  // Remove from subfolder _index.md
  if (subfolder) {
    await removeIndexRow(subfolderIndexPath(subfolder), subfolder, relPath);
  }

  // Remove from root Wiki/_index.md
  await removeIndexRow(rootIndexPath(), CONFIG.WIKI_FOLDER, relPath);

  // Append to log
  await appendToLog(`wiki delete: ${relPath}`);
}

export async function moveWikiFile(fromRelPath: string, toRelPath: string): Promise<void> {
  validateRelPath(fromRelPath);
  validateRelPath(toRelPath);

  const fromAbs = absoluteWikiPath(fromRelPath);
  const toAbs = absoluteWikiPath(toRelPath);

  // Read existing content to get metadata
  let raw: string;
  try {
    raw = await fs.readFile(fromAbs, 'utf-8');
  } catch (err) {
    throw new VaultError(`Wiki file not found: ${fromRelPath}`, { relPath: fromRelPath, error: String(err) });
  }

  const toExists = await fs.access(toAbs).then(() => true).catch(() => false);
  if (toExists) {
    throw new ValidationError(`Destination wiki file already exists: ${toRelPath}`, { toRelPath });
  }

  const { data } = matter(raw);
  const kind = typeof data['kind'] === 'string' ? data['kind'] : 'reference';
  const title = typeof data['title'] === 'string' ? data['title'] : extractSlug(toRelPath);
  const updated = nowISO();

  await fs.mkdir(path.dirname(toAbs), { recursive: true });
  await fs.rename(fromAbs, toAbs);

  // Update search index: remove old path, add new
  removeWikiFromIndex(fromRelPath);
  const { content: movedContent } = matter(raw);
  indexWikiEntry(toRelPath, title, kind, Array.isArray(data['tags']) ? (data['tags'] as string[]) : [], movedContent.trim(), updated, typeof data['created'] === 'string' ? data['created'] : updated);

  const fromSubfolder = extractSubfolder(fromRelPath);
  const toSubfolder = extractSubfolder(toRelPath);

  // If same subfolder, rename row in subfolder index
  if (fromSubfolder === toSubfolder && fromSubfolder) {
    await renameIndexRow(
      subfolderIndexPath(fromSubfolder),
      fromSubfolder,
      fromRelPath,
      toRelPath,
      kind,
      title,
      updated,
    );
  } else {
    // Different subfolders — remove from old, add to new
    if (fromSubfolder) {
      await removeIndexRow(subfolderIndexPath(fromSubfolder), fromSubfolder, fromRelPath);
    }
    if (toSubfolder) {
      await upsertIndexRow(
        subfolderIndexPath(toSubfolder),
        toSubfolder,
        toRelPath,
        kind,
        title,
        updated,
      );
    }
  }

  // Update root index
  await renameIndexRow(
    rootIndexPath(),
    CONFIG.WIKI_FOLDER,
    fromRelPath,
    toRelPath,
    kind,
    title,
    updated,
  );

  await appendToLog(`wiki move: ${fromRelPath} → ${toRelPath}`);
}
