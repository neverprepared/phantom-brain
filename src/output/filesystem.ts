import fs from 'node:fs/promises';
import path from 'node:path';
import matter from 'gray-matter';
import { CONFIG } from '../config.js';
import { ValidationError, VaultError } from '../shared/errors.js';
import { nowISO, todayDateString } from '../shared/utils.js';
import { writeAtomicFile, withFileLock, appendToLog } from '../vault/filesystem.js';
import type { OutputEntry, OutputFrontmatter } from './types.js';
import { OutputFrontmatterSchema } from './types.js';

// ---------------------------------------------------------------------------
// Path validation
// ---------------------------------------------------------------------------

const VALID_SUBFOLDERS = CONFIG.OUTPUT_SUBFOLDERS as readonly string[];

export function validateRelPath(relPath: string): { subfolder: string; slug: string } {
  if (path.isAbsolute(relPath)) {
    throw new ValidationError(`relPath must be relative, got: ${relPath}`);
  }
  if (relPath.includes('..')) {
    throw new ValidationError(`relPath must not contain '..', got: ${relPath}`);
  }
  if (relPath.startsWith('_attachments')) {
    throw new ValidationError(`relPath must not start with '_attachments', got: ${relPath}`);
  }

  const parts = relPath.split('/');
  if (parts.length !== 2) {
    throw new ValidationError(`relPath must be <subfolder>/<file>.md, got: ${relPath}`);
  }

  const [subfolder, filename] = parts as [string, string];
  if (!VALID_SUBFOLDERS.includes(subfolder)) {
    throw new ValidationError(
      `subfolder must be one of: ${VALID_SUBFOLDERS.join(', ')}, got: ${subfolder}`,
    );
  }

  if (!filename.endsWith('.md')) {
    throw new ValidationError(`file must end with .md, got: ${filename}`);
  }

  const slug = filename.replace(/\.md$/, '');
  if (!slug) {
    throw new ValidationError(`slug must not be empty in relPath: ${relPath}`);
  }

  return { subfolder, slug };
}

function outputFilePath(relPath: string): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, relPath);
}

function subfolderIndexPath(subfolder: string): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, subfolder, CONFIG.INDEX_FILE);
}

function rootIndexPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, CONFIG.INDEX_FILE);
}

// ---------------------------------------------------------------------------
// Index helpers
// ---------------------------------------------------------------------------

function indexRow(relPath: string, fm: OutputFrontmatter): string {
  const date = fm.updated.split('T')[0] ?? fm.updated;
  // relPath without .md extension for wiki-link: 'articles/oauth-guide.md' -> 'articles/oauth-guide'
  const wikiTarget = relPath.replace(/\.md$/, '');
  return `| [[${wikiTarget}]] | ${fm.kind} | ${fm.status} | ${date} |`;
}

const INDEX_HEADER = `| File | Kind | Status | Updated |\n|------|------|--------|---------|`;

async function rebuildSubfolderIndex(subfolder: string): Promise<void> {
  const dir = path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, subfolder);
  let files: string[] = [];
  try {
    const entries = await fs.readdir(dir);
    files = entries.filter((f) => f.endsWith('.md') && f !== CONFIG.INDEX_FILE);
  } catch {
    // directory may not exist yet
  }

  const rows: string[] = [];
  for (const file of files.sort()) {
    const relPath = `${subfolder}/${file}`;
    try {
      const { frontmatter } = await readOutputFile(relPath);
      rows.push(indexRow(relPath, frontmatter));
    } catch {
      // skip unreadable files
    }
  }

  const content = `# ${subfolder}\n\n${INDEX_HEADER}\n${rows.join('\n')}\n`;
  const idxPath = subfolderIndexPath(subfolder);
  await fs.mkdir(path.dirname(idxPath), { recursive: true });
  await writeAtomicFile(idxPath, content);
}

async function rebuildRootIndex(): Promise<void> {
  const rows: string[] = [];

  for (const subfolder of VALID_SUBFOLDERS) {
    const dir = path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, subfolder);
    let files: string[] = [];
    try {
      const entries = await fs.readdir(dir);
      files = entries.filter((f) => f.endsWith('.md') && f !== CONFIG.INDEX_FILE);
    } catch {
      continue;
    }

    for (const file of files.sort()) {
      const relPath = `${subfolder}/${file}`;
      try {
        const { frontmatter } = await readOutputFile(relPath);
        rows.push(indexRow(relPath, frontmatter));
      } catch {
        // skip unreadable files
      }
    }
  }

  const content = `# Output\n\n${INDEX_HEADER}\n${rows.join('\n')}\n`;
  const idxPath = rootIndexPath();
  await fs.mkdir(path.dirname(idxPath), { recursive: true });
  await writeAtomicFile(idxPath, content);
}

async function updateIndexes(subfolder: string): Promise<void> {
  await withFileLock(subfolderIndexPath(subfolder), () => rebuildSubfolderIndex(subfolder));
  await withFileLock(rootIndexPath(), () => rebuildRootIndex());
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

export async function readOutputFile(
  relPath: string,
): Promise<{ content: string; frontmatter: OutputFrontmatter }> {
  validateRelPath(relPath);
  const filePath = outputFilePath(relPath);

  let raw: string;
  try {
    raw = await fs.readFile(filePath, 'utf-8');
  } catch (err) {
    throw new VaultError(`Output file not found: ${relPath}`, { path: filePath, error: String(err) });
  }

  const parsed = matter(raw);
  const frontmatter = OutputFrontmatterSchema.parse(parsed.data);
  return { content: parsed.content.trim(), frontmatter };
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

export async function listOutput(subfolder?: string): Promise<OutputEntry[]> {
  const subfolders = subfolder ? [subfolder] : [...VALID_SUBFOLDERS];
  const entries: OutputEntry[] = [];

  for (const sf of subfolders) {
    if (!VALID_SUBFOLDERS.includes(sf)) {
      throw new ValidationError(
        `subfolder must be one of: ${VALID_SUBFOLDERS.join(', ')}, got: ${sf}`,
      );
    }
    const dir = path.join(CONFIG.VAULT_PATH, CONFIG.OUTPUT_FOLDER, sf);
    let files: string[] = [];
    try {
      const dirEntries = await fs.readdir(dir);
      files = dirEntries.filter((f) => f.endsWith('.md') && f !== CONFIG.INDEX_FILE);
    } catch {
      continue;
    }

    for (const file of files.sort()) {
      const relPath = `${sf}/${file}`;
      try {
        const { frontmatter } = await readOutputFile(relPath);
        entries.push({
          relPath,
          subfolder: sf,
          slug: file.replace(/\.md$/, ''),
          frontmatter,
        });
      } catch {
        // skip unparseable files
      }
    }
  }

  return entries;
}

// ---------------------------------------------------------------------------
// Write (create new)
// ---------------------------------------------------------------------------

export async function writeOutputFile(params: {
  relPath: string;
  content: string;
  frontmatter: Omit<OutputFrontmatter, 'created' | 'updated'>;
}): Promise<void> {
  const { relPath, content, frontmatter: fmInput } = params;
  validateRelPath(relPath);
  const { subfolder } = validateRelPath(relPath);
  const filePath = outputFilePath(relPath);

  // Reject if file already exists
  try {
    await fs.access(filePath);
    throw new ValidationError(`Output file already exists: ${relPath}`);
  } catch (err) {
    if (err instanceof ValidationError) throw err;
    // file does not exist — proceed
  }

  const now = nowISO();
  const frontmatter: OutputFrontmatter = OutputFrontmatterSchema.parse({
    ...fmInput,
    created: now,
    updated: now,
  });

  const fileContent = matter.stringify(content, frontmatter as Record<string, unknown>);
  await writeAtomicFile(filePath, fileContent);

  await updateIndexes(subfolder);

  const today = todayDateString();
  await appendToLog(`output_write ${relPath} kind=${frontmatter.kind} status=${frontmatter.status} date=${today}`);
}

// ---------------------------------------------------------------------------
// Update (patch existing)
// ---------------------------------------------------------------------------

export async function updateOutputFile(
  relPath: string,
  updates: { content?: string; frontmatter?: Partial<OutputFrontmatter> },
): Promise<void> {
  validateRelPath(relPath);
  const { subfolder } = validateRelPath(relPath);
  const filePath = outputFilePath(relPath);

  // Reject if file does NOT exist
  try {
    await fs.access(filePath);
  } catch {
    throw new VaultError(`Output file not found: ${relPath}`, { path: filePath });
  }

  const { content: existingContent, frontmatter: existingFm } = await readOutputFile(relPath);

  const now = nowISO();
  const mergedFm: OutputFrontmatter = OutputFrontmatterSchema.parse({
    ...existingFm,
    ...(updates.frontmatter ?? {}),
    updated: now,
  });

  const newContent = updates.content !== undefined ? updates.content : existingContent;
  const fileContent = matter.stringify(newContent, mergedFm as Record<string, unknown>);
  await writeAtomicFile(filePath, fileContent);

  await updateIndexes(subfolder);

  const today = todayDateString();
  await appendToLog(`output_update ${relPath} status=${mergedFm.status} date=${today}`);
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

export async function deleteOutputFile(relPath: string): Promise<void> {
  validateRelPath(relPath);
  const { subfolder } = validateRelPath(relPath);
  const filePath = outputFilePath(relPath);

  try {
    await fs.unlink(filePath);
  } catch (err) {
    throw new VaultError(`Failed to delete output file: ${relPath}`, {
      path: filePath,
      error: String(err),
    });
  }

  await updateIndexes(subfolder);

  const today = todayDateString();
  await appendToLog(`output_delete ${relPath} date=${today}`);
}
