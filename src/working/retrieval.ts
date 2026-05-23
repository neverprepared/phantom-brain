import fs from 'node:fs/promises';
import path from 'node:path';
import { searchMemories } from '../vault/search.js';
import { addFinding } from './db.js';
import { logger } from '../shared/logger.js';

const STOP_WORDS = new Set([
  'a', 'an', 'the', 'in', 'on', 'at', 'for', 'to', 'of', 'and', 'or',
  'is', 'are', 'was', 'were', 'be', 'been', 'being', 'have', 'has', 'had',
  'do', 'does', 'did', 'will', 'would', 'could', 'should', 'may', 'might',
  'can', 'with', 'by', 'from', 'as', 'this', 'that', 'these', 'those',
  'it', 'its', 'we', 'our', 'my', 'i', 'you', 'your',
]);

function extractKeywords(text: string): string[] {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9\s]/g, ' ')
    .split(/\s+/)
    .filter((w) => w.length > 3 && !STOP_WORDS.has(w))
    .slice(0, 6);
}

function parseH2Sections(md: string): Array<{ title: string; content: string }> {
  const sections: Array<{ title: string; content: string }> = [];
  const lines = md.split('\n');
  let current: { title: string; lines: string[] } | null = null;
  for (const line of lines) {
    if (line.startsWith('## ')) {
      if (current) sections.push({ title: current.title, content: current.lines.join('\n').trim() });
      current = { title: line.replace(/^##\s+/, '').trim(), lines: [] };
    } else if (current) {
      current.lines.push(line);
    }
  }
  if (current) sections.push({ title: current.title, content: current.lines.join('\n').trim() });
  return sections;
}

/**
 * Searches the Wiki for context relevant to the given goal and seeds the task
 * with findings. Called automatically by task_start.
 */
export async function seedTaskFromVault(
  task_id: string,
  goal: string,
): Promise<{ seeded: number; orphanedWarning?: string }> {
  const keywords = extractKeywords(goal);
  if (keywords.length === 0) return { seeded: 0 };

  let seeded = 0;

  try {
    const seen = new Set<string>();
    const results = [];
    for (const kw of keywords) {
      const hits = await searchMemories({ query: kw, limit: 5, freshness: 'fresh' });
      for (const hit of hits) {
        if (hit.resultKind === 'wiki' && hit.wikiEntry) {
          const key = `wiki:${hit.wikiEntry.relPath}`;
          if (!seen.has(key)) { seen.add(key); results.push(hit); }
        }
      }
    }
    results.sort((a, b) => b.score - a.score);
    const top = results.slice(0, 6);

    for (const result of top) {
      if (result.resultKind === 'wiki' && result.wikiEntry) {
        const we = result.wikiEntry;
        const snippet = result.snippet ? `\n\n> ${result.snippet}` : '';
        const content = `[Wiki/${we.kind}] **${we.title}** (score: ${result.score})${snippet}`;
        addFinding(task_id, content, 'high', 'semantic');
        seeded++;
      }
    }

    if (seeded > 0) {
      logger.info('Seeded task from vault', { task_id, seeded, keywords });
    }
  } catch (err) {
    logger.warn('Failed to seed task from vault', { task_id, error: String(err) });
  }

  // Read top-2 matching sections from MEMORY.md (project-level notes)
  const memoryMdPath = path.join(process.cwd(), 'MEMORY.md');
  try {
    const memoryMd = await fs.readFile(memoryMdPath, 'utf-8');
    const sections = parseH2Sections(memoryMd);
    const goalWords = new Set(goal.toLowerCase().split(/\W+/).filter((w) => w.length > 3));
    const scored = sections
      .map((s) => ({
        ...s,
        score: [...goalWords].filter((w) => s.content.toLowerCase().includes(w)).length,
      }))
      .filter((s) => s.score > 0)
      .sort((a, b) => b.score - a.score)
      .slice(0, 2);

    for (const s of scored) {
      addFinding(
        task_id,
        `From MEMORY.md — ${s.title}:\n${s.content.slice(0, 500)}`,
        'medium',
        'semantic',
      );
    }
  } catch {
    // MEMORY.md doesn't exist or isn't readable — skip
  }

  return { seeded };
}
