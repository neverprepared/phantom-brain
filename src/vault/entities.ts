/**
 * Entity pages — the primary knowledge surface of the brain.
 *
 * One source fans out into a summary page AND updates to one or more entity
 * pages in Wiki/entities/. Knowledge compounds: each new source enriches
 * existing entity pages rather than creating duplicates.
 *
 * Phase 1 extracts entities with simple heuristics:
 *   - ## level-2 headings
 *   - **Bold** terms (capitalized, 3-50 chars)
 *
 * Generic section names are filtered out. Phase 2 will replace this with a
 * real extraction + gate pass.
 */
import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { writeAtomicFile, withFileLock } from './filesystem.js';
import { slugFromTitle } from './naming.js';
import type { GateVerdict } from '../gate/evaluate.js';

const GENERIC_SECTIONS = new Set<string>([
  'overview',
  'summary',
  'introduction',
  'conclusion',
  'references',
  'notes',
  'background',
  'context',
  'example',
  'examples',
]);

const MIN_ENTITY_LEN = 3;
const MAX_ENTITY_LEN = 50;
const SNIPPET_LEN = 1500;
const SNIPPET_BEFORE = 600;
const SNIPPET_AFTER = 900;

/**
 * Extract candidate entity names from raw markdown content.
 *
 * Heuristics:
 *  1. `## ` level-2 headings (section names that look like entities)
 *  2. `**Bold**` terms that are capitalized and 3-50 chars
 *
 * Generic section names ("Overview", "Summary", ...) are filtered out.
 * Returns a de-duplicated list preserving first-seen order.
 */
export function extractEntities(content: string): string[] {
  const seen = new Map<string, string>(); // lowercase -> original casing
  const ordered: string[] = [];

  const consider = (raw: string): void => {
    const candidate = raw.trim();
    if (candidate.length < MIN_ENTITY_LEN || candidate.length > MAX_ENTITY_LEN) return;
    const lower = candidate.toLowerCase();
    if (GENERIC_SECTIONS.has(lower)) return;
    if (seen.has(lower)) return;
    seen.set(lower, candidate);
    ordered.push(candidate);
  };

  // 1. Level-2 headings — `## Heading text`
  const headingRegex = /^##\s+(.+?)\s*$/gm;
  let hmatch: RegExpExecArray | null;
  while ((hmatch = headingRegex.exec(content)) !== null) {
    const text = (hmatch[1] ?? '')
      // strip trailing markdown anchor/id markers like {#foo}
      .replace(/\{#[^}]+\}\s*$/, '')
      .trim();
    if (!text) continue;
    consider(text);
  }

  // 2. **Bold** terms — capitalized words, length 3-50
  const boldRegex = /\*\*([^*\n]+?)\*\*/g;
  let bmatch: RegExpExecArray | null;
  while ((bmatch = boldRegex.exec(content)) !== null) {
    const text = (bmatch[1] ?? '').trim();
    if (!text) continue;
    // Require the first non-whitespace char to be capitalized.
    if (!/^[A-Z]/.test(text)) continue;
    // Reject anything containing newlines or sentence-ending punctuation in the middle.
    if (/[.!?]/.test(text)) continue;
    consider(text);
  }

  return ordered;
}

/**
 * Given an entity name, return its absolute and vault-relative paths.
 */
export function entityPaths(name: string): { absPath: string; relPath: string } {
  const slug = slugFromTitle(name);
  const absPath = path.join(
    CONFIG.VAULT_PATH,
    CONFIG.WIKI_FOLDER,
    CONFIG.WIKI_ENTITIES,
    `${slug}.md`,
  );
  const relPath = path.posix.join(CONFIG.WIKI_FOLDER, CONFIG.WIKI_ENTITIES, `${slug}.md`);
  return { absPath, relPath };
}

/**
 * Read an entity page if it exists. Returns null on ENOENT, throws otherwise.
 */
export async function readEntityPage(absPath: string): Promise<string | null> {
  try {
    return await fs.readFile(absPath, 'utf-8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw err;
  }
}

/**
 * Extract a ~500-char snippet of raw content that most relevantly mentions
 * the entity name. Tries to land on sentence boundaries when possible.
 * Falls back to the first 500 chars if the entity is not found.
 */
export function buildContentSnippet(content: string, entityName: string): string {
  const idx = content.toLowerCase().indexOf(entityName.toLowerCase());
  if (idx < 0) {
    return trimToSentenceBoundary(content.slice(0, SNIPPET_LEN));
  }
  const start = Math.max(0, idx - SNIPPET_BEFORE);
  const end = Math.min(content.length, idx + entityName.length + SNIPPET_AFTER);
  let snippet = content.slice(start, end);

  // Try to start on a sentence boundary if we sliced into the middle of one.
  if (start > 0) {
    const firstSentenceEnd = snippet.search(/[.!?]\s+[A-Z]/);
    if (firstSentenceEnd > 0 && firstSentenceEnd < SNIPPET_BEFORE) {
      snippet = snippet.slice(firstSentenceEnd + 1).trimStart();
    } else {
      snippet = '...' + snippet.trimStart();
    }
  }

  // Trim trailing partial sentence if we sliced in the middle of one.
  return trimToSentenceBoundary(snippet);
}

function trimToSentenceBoundary(text: string): string {
  if (text.length <= SNIPPET_LEN) return text.trim();
  const truncated = text.slice(0, SNIPPET_LEN);
  const lastBoundary = Math.max(
    truncated.lastIndexOf('. '),
    truncated.lastIndexOf('! '),
    truncated.lastIndexOf('? '),
    truncated.lastIndexOf('\n\n'),
  );
  if (lastBoundary > SNIPPET_LEN * 0.6) {
    return truncated.slice(0, lastBoundary + 1).trim();
  }
  return truncated.trim() + '...';
}

function ymdFromISO(iso: string): string {
  return iso.slice(0, 10);
}

function escapeYamlString(s: string): string {
  return s.replace(/"/g, '\\"');
}

function escapeCell(s: string): string {
  // Markdown table cells: escape pipes and collapse newlines so a single row
  // stays on one line.
  return s.replace(/\|/g, '\\|').replace(/\r?\n/g, ' ').trim();
}

function reliabilityRow(
  sourceTitle: string,
  rawPath: string,
  verdict: GateVerdict,
): string {
  const safeTitle = escapeCell(sourceTitle);
  const category = verdict.category ?? '—';
  const reason = escapeCell(verdict.reason);
  return `| General content | ${verdict.reliability} | ${category} | ${reason} | [${safeTitle}](${rawPath}) |`;
}

/**
 * Create a new entity page on disk. Caller is responsible for ensuring the
 * page doesn't already exist (use readEntityPage first).
 */
export async function createEntityPage(opts: {
  name: string;
  absPath: string;
  sourceTitle: string;
  rawPath: string;
  contentSnippet: string;
  now: string;
  verdict: GateVerdict;
}): Promise<void> {
  const { name, absPath, sourceTitle, rawPath, contentSnippet, now, verdict } = opts;
  const ymd = ymdFromISO(now);
  const page =
    `---\n` +
    `title: "${escapeYamlString(name)}"\n` +
    `kind: entity\n` +
    `tags: []\n` +
    `created: "${now}"\n` +
    `updated: "${now}"\n` +
    `source_count: 1\n` +
    `---\n\n` +
    `# ${name}\n\n` +
    `## From: ${sourceTitle} (${ymd})\n\n` +
    `${contentSnippet}\n\n` +
    `## Source Reliability\n\n` +
    `| Claim | Reliability | Category | Reason | Source |\n` +
    `|---|---|---|---|---|\n` +
    `${reliabilityRow(sourceTitle, rawPath, verdict)}\n`;

  await withFileLock(absPath, async () => {
    await writeAtomicFile(absPath, page);
  });
}

/**
 * Append a new source section to an existing entity page.
 *
 * - Increments `source_count` in frontmatter.
 * - Updates `updated` timestamp.
 * - Inserts a new `## From: <title> (<date>)` section immediately before the
 *   "## Source Reliability" heading.
 * - Appends a new row to the reliability table.
 *
 * Handles malformed pages without a reliability table by appending the new
 * source section at the end of the file.
 */
export async function appendToEntityPage(opts: {
  absPath: string;
  sourceTitle: string;
  rawPath: string;
  contentSnippet: string;
  now: string;
  verdict: GateVerdict;
}): Promise<void> {
  const { absPath, sourceTitle, rawPath, contentSnippet, now, verdict } = opts;
  const ymd = ymdFromISO(now);

  await withFileLock(absPath, async () => {
    const existing = await fs.readFile(absPath, 'utf-8');
    const updated = applyAppend(existing, {
      sourceTitle,
      rawPath,
      contentSnippet,
      now,
      ymd,
      verdict,
    });
    await writeAtomicFile(absPath, updated);
  });
}

interface AppendOpts {
  sourceTitle: string;
  rawPath: string;
  contentSnippet: string;
  now: string;
  ymd: string;
  verdict: GateVerdict;
}

function applyAppend(existing: string, opts: AppendOpts): string {
  let content = bumpFrontmatter(existing, opts.now);

  const newSection = `\n## From: ${opts.sourceTitle} (${opts.ymd})\n\n${opts.contentSnippet}\n`;
  const newRow = reliabilityRow(opts.sourceTitle, opts.rawPath, opts.verdict);

  const reliabilityHeader = '## Source Reliability';
  const relIdx = content.indexOf(reliabilityHeader);

  if (relIdx < 0) {
    // Malformed page — no reliability section. Append the source section
    // at the end and also append a fresh reliability section so future
    // synthesizers find one.
    const padded = content.endsWith('\n') ? content : content + '\n';
    return (
      padded +
      newSection +
      `\n## Source Reliability\n\n` +
      `| Claim | Reliability | Category | Reason | Source |\n` +
      `|---|---|---|---|---|\n` +
      `${newRow}\n`
    );
  }

  // Insert the new "From" section immediately before the reliability heading.
  const before = content.slice(0, relIdx).replace(/\s+$/, '');
  const reliabilitySection = content.slice(relIdx);
  const withSection = `${before}\n${newSection}\n${reliabilitySection}`;

  // Append the new row to the reliability table. Find the end of the table
  // (last contiguous line starting with `|`) and insert after it.
  return appendReliabilityRow(withSection, newRow);
}

function bumpFrontmatter(content: string, now: string): string {
  const fmMatch = content.match(/^---\n([\s\S]*?)\n---/);
  if (!fmMatch) return content;
  let fm = fmMatch[1] ?? '';

  // source_count: increment, default to 1 if absent
  if (/^source_count:\s*\d+\s*$/m.test(fm)) {
    fm = fm.replace(/^source_count:\s*(\d+)\s*$/m, (_m, n: string) => `source_count: ${parseInt(n, 10) + 1}`);
  } else {
    fm = fm + `\nsource_count: 2`;
  }

  // updated: replace if present, else add
  if (/^updated:\s*.*$/m.test(fm)) {
    fm = fm.replace(/^updated:\s*.*$/m, `updated: "${now}"`);
  } else {
    fm = fm + `\nupdated: "${now}"`;
  }

  return content.replace(/^---\n[\s\S]*?\n---/, `---\n${fm}\n---`);
}

function appendReliabilityRow(content: string, newRow: string): string {
  // Find the reliability section and the end of its table.
  const lines = content.split('\n');
  let inReliability = false;
  let lastTableLine = -1;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? '';
    if (line.startsWith('## Source Reliability')) {
      inReliability = true;
      continue;
    }
    if (inReliability) {
      if (line.startsWith('## ')) break; // next section
      if (line.startsWith('|')) lastTableLine = i;
    }
  }

  if (lastTableLine < 0) {
    // No table rows found — append a header + row at the end.
    return (
      (content.endsWith('\n') ? content : content + '\n') +
      `\n| Claim | Reliability | Category | Reason | Source |\n` +
      `|---|---|---|---|---|\n` +
      `${newRow}\n`
    );
  }

  lines.splice(lastTableLine + 1, 0, newRow);
  return lines.join('\n');
}
