/**
 * Wiki/_index.md graduated hierarchy.
 *
 * Tracks how often each entity page is referenced across all synthesized
 * sources (via provenance.json) and graduates them into tiers:
 *
 *   Primary Topics  — referenced by 5+ sources
 *   Emerging Topics — referenced by 2-4 sources
 *   Notes           — referenced by exactly 1 source
 *
 * Only entity pages (Wiki/entities/*) are graduated. Other Wiki content
 * (HowTos, Runbooks, References, summaries) is not tracked here.
 */
import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { writeAtomicFile, withFileLock } from './filesystem.js';
import type { ProvenanceMap } from './provenance.js';

const PRIMARY_HEADING = '## Primary Topics';
const EMERGING_HEADING = '## Emerging Topics';
const NOTES_HEADING = '## Notes';

export interface EntityGraduation {
  primary: string[];   // 5+ sources
  emerging: string[];  // 2-4 sources
  notes: string[];     // exactly 1 source
}

function wikiIndexPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, CONFIG.INDEX_FILE);
}

function entityPrefix(): string {
  // posix join because provenance stores forward-slash paths
  return path.posix.join(CONFIG.WIKI_FOLDER, CONFIG.WIKI_ENTITIES) + '/';
}

/**
 * Tally how many sources reference each entity page.
 * Returns a map of entity name -> reference count.
 */
export function tallyEntityReferences(provenance: ProvenanceMap): Map<string, number> {
  const counts = new Map<string, number>();
  const prefix = entityPrefix();

  for (const key of Object.keys(provenance)) {
    const entry = provenance[key];
    if (!entry || !Array.isArray(entry.wiki_pages)) continue;
    // Deduplicate per source — a single source referencing the same entity
    // page twice still counts once.
    const seen = new Set<string>();
    for (const p of entry.wiki_pages) {
      if (!p.startsWith(prefix)) continue;
      const base = path.posix.basename(p, '.md');
      if (!base || seen.has(base)) continue;
      seen.add(base);
      counts.set(base, (counts.get(base) ?? 0) + 1);
    }
  }

  return counts;
}

/**
 * Bucket entity counts into the three graduation tiers.
 * Names within each tier are sorted by count desc, then alphabetically.
 */
export function graduateEntities(counts: Map<string, number>): EntityGraduation {
  const primary: Array<[string, number]> = [];
  const emerging: Array<[string, number]> = [];
  const notes: Array<[string, number]> = [];

  for (const [name, count] of counts) {
    if (count >= 5) primary.push([name, count]);
    else if (count >= 2) emerging.push([name, count]);
    else if (count === 1) notes.push([name, count]);
  }

  const sortFn = (a: [string, number], b: [string, number]): number => {
    if (b[1] !== a[1]) return b[1] - a[1];
    return a[0].localeCompare(b[0]);
  };
  primary.sort(sortFn);
  emerging.sort(sortFn);
  notes.sort(sortFn);

  return {
    primary: primary.map(([n]) => n),
    emerging: emerging.map(([n]) => n),
    notes: notes.map(([n]) => n),
  };
}

function renderGraduatedSections(g: EntityGraduation): string {
  const renderList = (names: string[]): string =>
    names.length === 0 ? '_(none yet)_\n' : names.map((n) => `- ${n}`).join('\n') + '\n';

  return (
    `${PRIMARY_HEADING}\n\n` +
    renderList(g.primary) +
    `\n${EMERGING_HEADING}\n\n` +
    renderList(g.emerging) +
    `\n${NOTES_HEADING}\n\n` +
    renderList(g.notes)
  );
}

/**
 * Rewrite the graduated sections of Wiki/_index.md.
 *
 * Preserves any content above the first "## Primary Topics" heading. If the
 * file doesn't exist or has no Primary Topics heading, the graduated sections
 * are appended to whatever is there.
 */
export async function updateWikiIndex(provenance: ProvenanceMap): Promise<void> {
  const counts = tallyEntityReferences(provenance);
  const graduated = graduateEntities(counts);
  const newSections = renderGraduatedSections(graduated);

  const filePath = wikiIndexPath();
  await withFileLock(filePath, async () => {
    let existing = '';
    try {
      existing = await fs.readFile(filePath, 'utf-8');
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== 'ENOENT') throw err;
      existing = '';
    }

    let preserved: string;
    const primaryIdx = existing.indexOf(PRIMARY_HEADING);
    if (primaryIdx >= 0) {
      preserved = existing.slice(0, primaryIdx).replace(/\s+$/, '');
    } else {
      preserved = existing.replace(/\s+$/, '');
    }

    const next = preserved.length > 0
      ? `${preserved}\n\n${newSections}`
      : newSections;

    await writeAtomicFile(filePath, next);
  });
}
