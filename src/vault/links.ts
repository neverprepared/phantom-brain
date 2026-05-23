import { readMemoryFile, writeMemoryFile } from './filesystem.js';
import { getIndex, findBySlug, indexEntry } from './search.js';
import { parseMemoryFile, serializeMemory } from './frontmatter.js';
import { logger } from '../shared/logger.js';
import { escapeRegex } from '../shared/utils.js';
import { CONFIG } from '../config.js';

const WIKI_LINK_RE = /\[\[([^\]]+)\]\]/g;

export function extractWikiLinks(content: string): string[] {
  const links: string[] = [];
  let match;
  while ((match = WIKI_LINK_RE.exec(content)) !== null) {
    const link = match[1];
    if (link) {
      links.push(link);
    }
  }
  return links;
}

export function buildRelatedSection(slugs: string[]): string {
  if (slugs.length === 0) return '';
  const links = slugs.map((s) => `- [[${s}]]`).join('\n');
  return `\n\n## Related\n\n${links}`;
}

export interface LinkGraph {
  outgoing: string[]; // Slugs this memory links to
  incoming: string[]; // Slugs that link to this memory
}

export async function discoverLinks(slug: string): Promise<LinkGraph> {
  const index = getIndex();
  const outgoing: string[] = [];
  const incoming: string[] = [];

  // Find outgoing links: frontmatter.related (authoritative) plus wiki-links read from disk
  const entry = findBySlug(slug);
  if (entry) {
    const relatedSet = new Set(entry.frontmatter.related);
    try {
      const raw = await readMemoryFile(entry.filePath);
      const parsed = parseMemoryFile(raw, entry.filePath);
      for (const link of extractWikiLinks(parsed.content)) {
        relatedSet.add(link);
      }
    } catch (err) {
      logger.warn('Failed to read body for link discovery', { slug, error: String(err) });
    }
    outgoing.push(...relatedSet);
  }

  // Find incoming links from all other memories using in-memory index
  for (const other of index.values()) {
    if (other.slug === slug) continue;
    if (other.frontmatter.related.includes(slug)) {
      incoming.push(other.slug);
    }
  }

  return { outgoing, incoming };
}

export function addRelatedLink(content: string, targetSlug: string): string {
  const relatedHeader = '## Related';
  const newLink = `- [[${targetSlug}]]`;

  if (content.includes(relatedHeader)) {
    // Check if link already exists
    if (content.includes(`[[${targetSlug}]]`)) {
      return content;
    }
    // Append to existing Related section
    return content.replace(relatedHeader, `${relatedHeader}\n${newLink}`);
  }

  // Add new Related section
  return content.trimEnd() + `\n\n${relatedHeader}\n\n${newLink}\n`;
}

/**
 * Find existing memories with Jaccard tag similarity >= MIN_TAG_JACCARD.
 * Returns their slugs, sorted by Jaccard score descending (most related first).
 */
export function findRelatedByTags(tags: string[], excludeSlug?: string): string[] {
  const index = getIndex();
  const tagSet = new Set(tags.map((t) => t.toLowerCase()));

  const matches: Array<{ slug: string; jaccard: number }> = [];

  for (const entry of index.values()) {
    if (entry.slug === excludeSlug) continue;
    const entryTags = new Set(entry.frontmatter.tags.map((t) => t.toLowerCase()));
    const intersection = [...tagSet].filter((t) => entryTags.has(t)).length;
    const union = new Set([...tagSet, ...entryTags]).size;
    const jaccard = union === 0 ? 0 : intersection / union;
    if (jaccard >= CONFIG.MIN_TAG_JACCARD) {
      matches.push({ slug: entry.slug, jaccard });
    }
  }

  return matches
    .sort((a, b) => b.jaccard - a.jaccard)
    .map((m) => m.slug);
}

export interface AutoLinkResult {
  linked: string[];
  failed: string[];
}

/**
 * Auto-link a newly stored memory to related memories (bidirectional).
 * - Adds [[wiki-links]] from the new memory to related ones
 * - Adds backlinks from related memories back to the new one
 * - Updates frontmatter `related` arrays on both sides
 * - Applies per-store cap (MAX_AUTO_LINKS_PER_STORE) and per-atom cap (MAX_AUTO_LINKS_PER_ATOM)
 *
 * Returns { linked, failed } — errors are logged but don't fail the store.
 */
export async function autoLinkRelated(
  newSlug: string,
  newFilePath: string,
  tags: string[],
): Promise<AutoLinkResult> {
  const linked: string[] = [];
  const failed: string[] = [];

  try {
    const allRelated = findRelatedByTags(tags, newSlug);
    // Per-store cap: new atom gets at most MAX_AUTO_LINKS_PER_STORE outgoing links
    const relatedSlugs = allRelated.slice(0, CONFIG.MAX_AUTO_LINKS_PER_STORE);
    if (relatedSlugs.length === 0) return { linked: [], failed: [] };

    // 1. Update the new memory file: add outgoing links
    const newRaw = await readMemoryFile(newFilePath);
    const newParsed = parseMemoryFile(newRaw, newFilePath);

    const existingRelated = new Set(newParsed.frontmatter.related);
    for (const slug of relatedSlugs) existingRelated.add(slug);
    newParsed.frontmatter.related = [...existingRelated];

    let newContent = newParsed.content;
    for (const slug of relatedSlugs) {
      newContent = addRelatedLink(newContent, slug);
    }

    await writeMemoryFile(newFilePath, serializeMemory(newParsed.frontmatter, newContent));
    indexEntry(
      newParsed.frontmatter.id,
      { frontmatter: newParsed.frontmatter, filePath: newFilePath, slug: newSlug },
      newContent,
    );

    // 2. Update each related memory: add backlink (with per-atom cap)
    await Promise.all(relatedSlugs.map(async (slug) => {
      try {
        const entry = findBySlug(slug);
        if (!entry) { failed.push(slug); return; }

        const raw = await readMemoryFile(entry.filePath);
        const parsed = parseMemoryFile(raw, entry.filePath);

        if (parsed.frontmatter.related.includes(newSlug)) {
          linked.push(slug);
          return;
        }

        // Per-atom cap: don't add backlink if atom is already at max incoming links
        if (parsed.frontmatter.related.length >= CONFIG.MAX_AUTO_LINKS_PER_ATOM) {
          logger.debug('Skipping backlink — target atom at MAX_AUTO_LINKS_PER_ATOM', { slug, newSlug });
          linked.push(slug);
          return;
        }

        parsed.frontmatter.related.push(newSlug);
        const updatedContent = addRelatedLink(parsed.content, newSlug);

        await writeMemoryFile(entry.filePath, serializeMemory(parsed.frontmatter, updatedContent));
        indexEntry(
          parsed.frontmatter.id,
          { frontmatter: parsed.frontmatter, filePath: entry.filePath, slug: entry.slug },
          updatedContent,
        );

        linked.push(slug);
      } catch (err) {
        logger.warn('Failed to add backlink', { slug, newSlug, error: String(err) });
        failed.push(slug);
      }
    }));

    logger.info('Auto-linked related memories', { newSlug, linkedCount: linked.length, failedCount: failed.length });
  } catch (err) {
    logger.warn('Auto-linking failed', { newSlug, error: String(err) });
  }

  return { linked, failed };
}

export interface GraphNode {
  slug: string;
  title: string;
  depth: number;
}

/**
 * BFS traversal of the link graph starting from a given slug.
 * Returns all transitively related memories within `maxDepth` hops.
 * Uses frontmatter `related` arrays for O(1) neighbor lookup (no file reads).
 */
export function traverseGraph(startSlug: string, maxDepth: number): GraphNode[] {
  const index = getIndex();
  const visited = new Set<string>();
  const result: GraphNode[] = [];
  const queue: Array<{ slug: string; depth: number }> = [{ slug: startSlug, depth: 0 }];

  visited.add(startSlug);

  while (queue.length > 0) {
    const { slug, depth } = queue.shift()!;

    // Don't include the start node in results
    if (depth > 0) {
      const entry = findBySlug(slug);
      if (entry) {
        result.push({ slug, title: entry.frontmatter.title, depth });
      }
    }

    if (depth >= maxDepth) continue;

    // Get neighbors from the index (frontmatter.related + find entries that reference this slug)
    const entry = findBySlug(slug);
    if (!entry) continue;

    // Outgoing: this memory's related slugs
    const neighbors = new Set(entry.frontmatter.related);

    // Incoming: other memories that list this slug in their related
    for (const other of index.values()) {
      if (other.frontmatter.related.includes(slug)) {
        neighbors.add(other.slug);
      }
    }

    for (const neighbor of neighbors) {
      if (!visited.has(neighbor)) {
        visited.add(neighbor);
        queue.push({ slug: neighbor, depth: depth + 1 });
      }
    }
  }

  return result;
}

export interface RemoveBacklinksResult {
  cleaned: string[];
  failed: string[];
}

/**
 * Remove all references to deletedSlug from other memories after a delete.
 * Cleans both the `related` frontmatter array and [[wiki-links]] in body content.
 * Best-effort: processes each file independently, failures are collected not thrown.
 */
export async function removeBacklinks(deletedSlug: string): Promise<RemoveBacklinksResult> {
  const index = getIndex();
  const cleaned: string[] = [];
  const failed: string[] = [];

  for (const entry of index.values()) {
    const fm = entry.frontmatter;
    if (!fm.related.includes(deletedSlug)) continue;

    try {
      const raw = await readMemoryFile(entry.filePath);
      const parsed = parseMemoryFile(raw, entry.filePath);

      // Remove from related array
      parsed.frontmatter.related = parsed.frontmatter.related.filter((s) => s !== deletedSlug);

      // Remove [[deletedSlug]] wiki-links from body
      const wikiLinkPattern = new RegExp(`- \\[\\[${escapeRegex(deletedSlug)}\\]\\]\\n?`, 'g');
      const updatedContent = parsed.content.replace(wikiLinkPattern, '');

      await writeMemoryFile(entry.filePath, serializeMemory(parsed.frontmatter, updatedContent));

      indexEntry(
        parsed.frontmatter.id,
        { frontmatter: parsed.frontmatter, filePath: entry.filePath, slug: entry.slug },
        updatedContent,
      );

      cleaned.push(entry.slug);
    } catch (err) {
      logger.warn('Failed to remove backlink', { from: entry.slug, deletedSlug, error: String(err) });
      failed.push(entry.slug);
    }
  }

  return { cleaned, failed };
}

/**
 * Rename a slug across all memories: update `related` arrays and [[wiki-links]] in body content.
 * Called when a memory's title changes and its slug changes.
 */
export async function renameSlugReferences(oldSlug: string, newSlug: string): Promise<{ updated: string[]; failed: string[] }> {
  const index = getIndex();
  const updated: string[] = [];
  const failed: string[] = [];

  for (const entry of index.values()) {
    try {
      const raw = await readMemoryFile(entry.filePath);
      const parsed = parseMemoryFile(raw, entry.filePath);

      const hasRelatedRef = parsed.frontmatter.related.includes(oldSlug);
      const hasBodyRef = parsed.content.includes(`[[${oldSlug}]]`);
      if (!hasRelatedRef && !hasBodyRef) continue;

      parsed.frontmatter.related = parsed.frontmatter.related.map((s) => s === oldSlug ? newSlug : s);
      const wikiLinkPattern = new RegExp(`\\[\\[${escapeRegex(oldSlug)}\\]\\]`, 'g');
      const updatedContent = parsed.content.replace(wikiLinkPattern, `[[${newSlug}]]`);

      await writeMemoryFile(entry.filePath, serializeMemory(parsed.frontmatter, updatedContent));

      indexEntry(
        parsed.frontmatter.id,
        { frontmatter: parsed.frontmatter, filePath: entry.filePath, slug: entry.slug },
        updatedContent,
      );

      updated.push(entry.slug);
    } catch (err) {
      logger.warn('Failed to rename slug reference', { from: entry.slug, oldSlug, newSlug, error: String(err) });
      failed.push(entry.slug);
    }
  }

  if (updated.length > 0) {
    logger.info('Renamed slug references', { oldSlug, newSlug, updated: updated.length, failed: failed.length });
  }

  return { updated, failed };
}

/**
 * Build a map of slug -> count of incoming links from other memories' `related` arrays.
 * Used by stats and cleanup tools for orphan detection.
 */
export function buildIncomingLinkCount(index: Map<string, { slug: string; frontmatter: { related: string[] } }>): Map<string, number> {
  const incomingCount = new Map<string, number>();
  for (const entry of index.values()) {
    for (const relatedSlug of entry.frontmatter.related) {
      incomingCount.set(relatedSlug, (incomingCount.get(relatedSlug) ?? 0) + 1);
    }
  }
  return incomingCount;
}
