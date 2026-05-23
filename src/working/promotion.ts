import { searchMemories, findByTitle } from '../vault/search.js';
import { handleStore } from '../tools/store.js';
import { handleUpdate } from '../tools/update.js';
import { isFtsReady, searchFts } from '../vault/fts-index.js';
import { appendToLog } from '../vault/filesystem.js';
import { writeWikiFile } from '../wiki/filesystem.js';
import { slugFromTitle } from '../vault/naming.js';
import type { Finding, MemoryType, TaskState } from './db.js';
import type { LifecycleStatus } from '../schemas/frontmatter.js';
import { logger } from '../shared/logger.js';

/** Lifecycle routing per memory_type */
const LIFECYCLE_FOR_TYPE: Record<MemoryType, LifecycleStatus> = {
  semantic: 'reference',
  episodic: 'active',
  procedural: 'reference',
};

const TTL_FOR_TYPE: Record<MemoryType, number> = {
  semantic: 180,
  episodic: 90,
  procedural: 180,
};

/** Tags appended per memory_type to aid future retrieval */
const EXTRA_TAGS: Record<MemoryType, string[]> = {
  semantic: ['fact', 'semantic-memory'],
  episodic: ['episode', 'working-memory-log'],
  procedural: ['procedure', 'runbook'],
};

/**
 * Derives a short title from the first sentence of content.
 * Falls back to the first 80 chars if no sentence boundary is found.
 */
function titleFromContent(content: string): string {
  const clean = content
    .replace(/\[From long-term memory\]/g, '')
    .replace(/\*\*/g, '')
    .trim();

  const boundary = clean.search(/[.!?\n]/);
  const raw = boundary > 0 ? clean.slice(0, boundary) : clean;
  return raw.trim().slice(0, 80).trim() || 'Promoted finding';
}

/**
 * Extracts simple keywords from a goal string for tag generation.
 */
function keywordsFromGoal(goal: string): string[] {
  const stopWords = new Set(['a', 'an', 'the', 'in', 'on', 'at', 'for', 'to', 'of', 'and', 'or', 'is', 'are', 'was', 'with', 'by', 'from', 'this', 'that']);
  return goal
    .toLowerCase()
    .replace(/[^a-z0-9\s]/g, ' ')
    .split(/\s+/)
    .filter((w) => w.length > 3 && !stopWords.has(w))
    .slice(0, 5);
}

/**
 * Attempts to find an existing Obsidian note that matches a finding's content.
 * Uses multiple strategies to prevent near-duplicate creation:
 * 1. Exact/partial title match via index
 * 2. FTS5 ranked search (when available)
 * 3. Keyword search fallback with score threshold
 * 4. Tag overlap (2+ shared tags)
 * Returns the memory ID if found, otherwise undefined.
 */
async function findMatchingNote(title: string, tags: string[], content: string): Promise<string | undefined> {
  try {
    // 1. Direct title match (exact or partial via index)
    const titleMatch = findByTitle(title);
    if (titleMatch) {
      return titleMatch.frontmatter.id;
    }

    // 2. FTS5 search on content for near-duplicate detection
    if (isFtsReady()) {
      // Search using first sentence of content for semantic match
      const contentQuery = content.slice(0, 100).replace(/[^a-zA-Z0-9\s]/g, ' ').trim();
      if (contentQuery.length > 10) {
        const ftsHits = searchFts(contentQuery, 3);
        for (const hit of ftsHits) {
          // High FTS rank indicates strong content overlap
          if (hit.rank > 5) {
            logger.info('Dedup: found near-duplicate via FTS content match', { id: hit.id, rank: hit.rank });
            return hit.id;
          }
        }
      }
    }

    // 3. Search by title words
    const titleQuery = title.slice(0, 40);
    const results = await searchMemories({ query: titleQuery, limit: 5 });

    for (const result of results) {
      if (result.resultKind !== 'atom' || !result.entry) continue;

      // Strong title match
      if (result.score >= 10) {
        return result.entry.frontmatter.id;
      }

      // Tag overlap match (2+ shared tags)
      const entryTags = result.entry.frontmatter.tags.map((t) => t.toLowerCase());
      const sharedTags = tags.filter((t) => entryTags.includes(t.toLowerCase()));
      if (sharedTags.length >= 2) {
        return result.entry.frontmatter.id;
      }
    }
  } catch (err) {
    logger.warn('Match search failed during promotion', { error: String(err) });
  }

  return undefined;
}

/**
 * Derive input_sources from artifact references that start with 'Input/'.
 */
function extractInputSources(artifacts: TaskState['artifacts']): string[] {
  return artifacts
    .map((a) => a.reference)
    .filter((ref) => ref.startsWith('Input/'));
}

async function promoteFinding(
  finding: Finding,
  goalTags: string[],
  inputSources: string[],
): Promise<'created' | 'appended' | 'skipped'> {
  const memoryType = (finding.memory_type ?? 'episodic') as MemoryType;

  // Skip seeded long-term memory findings — they already live in the vault
  if (finding.content.startsWith('[From long-term memory]')) {
    return 'skipped';
  }

  const title = titleFromContent(finding.content);
  const lifecycleStatus = LIFECYCLE_FOR_TYPE[memoryType];
  const ttlDays = TTL_FOR_TYPE[memoryType];
  const tags = [...goalTags, ...EXTRA_TAGS[memoryType]];

  // --- Procedural findings: write to Wiki/, create stub atom ---
  if (memoryType === 'procedural') {
    const wikiSlug = slugFromTitle(title);
    const wikiRelPath = `Runbooks/${wikiSlug}.md`;
    const wikiContent = `## Steps\n\n${finding.content}`;

    try {
      await writeWikiFile({
        relPath: wikiRelPath,
        mode: 'create',
        content: wikiContent,
        title,
        kind: 'runbook',
        tags,
        sources: [],
      });
    } catch {
      // May already exist — ignore creation errors, still create stub atom
    }

    // Create stub atom in Memory/ pointing to the wiki page
    const stubContent = `Procedure captured as wiki runbook. See [[Wiki/${wikiRelPath.replace(/\.md$/, '')}]].`;
    const result = await handleStore({
      title,
      content: stubContent,
      lifecycle_status: lifecycleStatus,
      tags,
      source: 'conversation',
      confidence: finding.importance === 'high' ? 'high' : 'medium',
      related: [],
      source_urls: [],
      ttl_days: ttlDays,
      ...(inputSources.length > 0 && { input_sources: inputSources }),
    });

    if (result.isError) {
      logger.warn('Failed to create stub atom for procedural finding', { title });
      return 'skipped';
    }

    await appendToLog(`promoted procedural: "${title}" → Wiki/${wikiRelPath}`);
    logger.info('Promoted procedural finding to Wiki + stub atom', { title });
    return 'created';
  }

  // --- Semantic / episodic findings ---
  const content = finding.content;

  const existingId = await findMatchingNote(title, tags, content);

  if (existingId) {
    const result = await handleUpdate({
      id: existingId,
      content: `\n\n---\n\n${content}`,
      append: true,
      add_tags: tags,
    });

    if (result.isError) {
      logger.warn('Failed to append finding to existing note', { id: existingId });
      return 'skipped';
    }

    await appendToLog(`promoted ${memoryType} (appended): "${title}" → id:${existingId}`);
    logger.info('Appended finding to existing note', { id: existingId, title });
    return 'appended';
  }

  const storeArgs: Record<string, unknown> = {
    title,
    content,
    lifecycle_status: lifecycleStatus,
    tags,
    source: 'conversation',
    confidence: finding.importance === 'high' ? 'high' : 'medium',
    related: [],
    source_urls: [],
    ttl_days: ttlDays,
  };

  if (inputSources.length > 0) {
    storeArgs['input_sources'] = inputSources;
  }

  const result = await handleStore(storeArgs);

  if (result.isError) {
    logger.warn('Failed to create note for finding', { title });
    return 'skipped';
  }

  await appendToLog(`promoted ${memoryType} (created): "${title}"`);
  logger.info('Created new note from finding', { title, lifecycleStatus, memoryType });
  return 'created';
}

/**
 * Promotes all medium/high importance findings from a completed task to Obsidian.
 * Called automatically by task_complete.
 */
export async function promoteTaskToVault(state: TaskState): Promise<{ created: number; appended: number; skipped: number }> {
  const counts = { created: 0, appended: 0, skipped: 0 };
  const goalTags = keywordsFromGoal(state.task.goal);
  const inputSources = extractInputSources(state.artifacts);

  for (const finding of state.findings) {
    if (finding.importance === 'low') continue;

    const outcome = await promoteFinding(finding, goalTags, inputSources);
    counts[outcome]++;
  }

  logger.info('Task promotion complete', {
    task_id: state.task.task_id,
    ...counts,
  });

  return counts;
}
