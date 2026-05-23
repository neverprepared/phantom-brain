import fs from 'node:fs/promises';
import path from 'node:path';
import matter from 'gray-matter';
import { CONFIG } from '../config.js';
import { withFileLock, writeAtomicFile, appendToLog } from '../vault/filesystem.js';
import { nowISO } from '../shared/utils.js';
import { logger } from '../shared/logger.js';
import type { InputEntry } from './types.js';

function inputRoot(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.INPUT_FOLDER);
}

function validateRelPath(relPath: string): void {
  if (relPath.includes('..') || path.isAbsolute(relPath)) {
    throw new Error(`Invalid Input relPath: ${relPath}`);
  }
}

/**
 * Ingest a new source file. Rejects if the file already exists (immutability contract).
 * Updates Input/_index.md. Appends to _log/.
 */
export async function ingestInput(params: {
  relPath: string;       // e.g. 'articles/oauth-rfc.md'
  content: string;       // body text (markdown or plain text)
  kind: InputEntry['kind'];
  sourceUrl?: string;
  description?: string;
}): Promise<void> {
  validateRelPath(params.relPath);

  const filePath = path.join(inputRoot(), params.relPath);

  // Reject if file already exists (immutability contract)
  try {
    await fs.access(filePath);
    throw new Error(`Input file already exists: ${params.relPath}`);
  } catch (err) {
    if (err instanceof Error && err.message.startsWith('Input file already exists:')) {
      throw err;
    }
    // File doesn't exist — proceed
  }

  const capturedAt = nowISO();
  const frontmatter: Record<string, unknown> = {
    kind: params.kind,
    captured_at: capturedAt,
  };
  if (params.sourceUrl) frontmatter['source_url'] = params.sourceUrl;
  if (params.description) frontmatter['description'] = params.description;

  const fileContent = matter.stringify(params.content, frontmatter);

  await fs.mkdir(path.dirname(filePath), { recursive: true });
  await writeAtomicFile(filePath, fileContent);

  logger.info('Ingested input file', { relPath: params.relPath, kind: params.kind });

  // Update Input/_index.md
  const indexPath = path.join(inputRoot(), CONFIG.INDEX_FILE);
  await withFileLock(indexPath, async () => {
    let existing = '';
    try {
      existing = await fs.readFile(indexPath, 'utf-8');
    } catch {
      existing = `# Input Index\n\n| Path | Kind | Date | Description |\n|------|------|------|-------------|\n`;
    }

    // Ensure the table exists; if not, append it
    const tableHeader = '| Path | Kind | Date | Description |';
    let updated: string;
    if (!existing.includes(tableHeader)) {
      updated = existing.trimEnd() + `\n\n${tableHeader}\n|------|------|------|-------------|\n`;
    } else {
      updated = existing.trimEnd();
    }

    const date = capturedAt.split('T')[0]!;
    const desc = params.description ?? '';
    const row = `| ${params.relPath} | ${params.kind} | ${date} | ${desc} |`;
    updated = updated + '\n' + row + '\n';

    await writeAtomicFile(indexPath, updated);
  });

  await appendToLog(`input_ingest: ${params.relPath} (${params.kind})`);
}

/**
 * Read a source file by relPath. Path-validates (no ..).
 */
export async function readInputFile(relPath: string): Promise<{ content: string; metadata: Partial<InputEntry> }> {
  validateRelPath(relPath);

  const filePath = path.join(inputRoot(), relPath);
  let raw: string;
  try {
    raw = await fs.readFile(filePath, 'utf-8');
  } catch (err) {
    throw new Error(`Input file not found: ${relPath}`);
  }

  const parsed = matter(raw);
  const fm = parsed.data as Record<string, unknown>;
  const slug = path.basename(relPath, '.md');

  // Derive kind from subfolder if frontmatter doesn't have it
  const subfolderPart = relPath.split('/')[0] ?? '';
  const kindFromPath = (CONFIG.INPUT_SUBFOLDERS as readonly string[]).includes(subfolderPart)
    ? (subfolderPart.replace(/s$/, '') as InputEntry['kind'])
    : undefined;

  const metadata: Partial<InputEntry> = {
    relPath,
    slug,
    kind: (fm['kind'] as InputEntry['kind']) ?? kindFromPath,
    capturedAt: (fm['captured_at'] as string) ?? '',
    sourceUrl: fm['source_url'] as string | undefined,
    description: fm['description'] as string | undefined,
    supersededBy: fm['superseded_by'] as string | undefined,
  };

  return { content: parsed.content, metadata };
}

/**
 * List all Input files, optionally filtered by subfolder.
 * Returns basic metadata from frontmatter.
 */
export async function listInput(subfolder?: string): Promise<InputEntry[]> {
  const root = inputRoot();
  const entries: InputEntry[] = [];

  const subfolders = subfolder
    ? [subfolder]
    : [...CONFIG.INPUT_SUBFOLDERS];

  for (const sub of subfolders) {
    const dirPath = path.join(root, sub);
    let files: string[];
    try {
      files = await fs.readdir(dirPath);
    } catch {
      continue;
    }

    for (const file of files) {
      if (!file.endsWith('.md')) continue;
      if (file === CONFIG.INDEX_FILE) continue;

      const relPath = `${sub}/${file}`;
      const filePath = path.join(dirPath, file);
      let raw: string;
      try {
        raw = await fs.readFile(filePath, 'utf-8');
      } catch {
        continue;
      }

      const parsed = matter(raw);
      const fm = parsed.data as Record<string, unknown>;
      const slug = file.replace(/\.md$/, '');

      // Extract first non-empty line of body as description fallback
      const descriptionFallback = parsed.content
        .split('\n')
        .map((l) => l.trim())
        .find((l) => l.length > 0);

      entries.push({
        relPath,
        slug,
        kind: (fm['kind'] as InputEntry['kind']) ?? (sub.replace(/s$/, '') as InputEntry['kind']),
        capturedAt: (fm['captured_at'] as string) ?? '',
        sourceUrl: fm['source_url'] as string | undefined,
        description: (fm['description'] as string | undefined) ?? descriptionFallback,
        supersededBy: fm['superseded_by'] as string | undefined,
      });
    }
  }

  // Sort by capturedAt descending
  entries.sort((a, b) => b.capturedAt.localeCompare(a.capturedAt));

  return entries;
}

/**
 * Mark a source file as superseded. Writes superseded_by frontmatter, _log/ entry.
 * Never deletes the original.
 */
export async function supersededInput(relPath: string, supersededByRelPath: string): Promise<void> {
  validateRelPath(relPath);
  validateRelPath(supersededByRelPath);

  const filePath = path.join(inputRoot(), relPath);

  await withFileLock(filePath, async () => {
    let raw: string;
    try {
      raw = await fs.readFile(filePath, 'utf-8');
    } catch {
      throw new Error(`Input file not found: ${relPath}`);
    }

    const parsed = matter(raw);
    const fm = parsed.data as Record<string, unknown>;
    fm['superseded_by'] = supersededByRelPath;

    const updated = matter.stringify(parsed.content, fm);
    await writeAtomicFile(filePath, updated);
  });

  logger.info('Marked input as superseded', { relPath, supersededBy: supersededByRelPath });
  await appendToLog(`input_supersede: ${relPath} → ${supersededByRelPath}`);
}
