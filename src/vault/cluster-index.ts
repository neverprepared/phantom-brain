import path from 'node:path';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';
import { writeAtomicFile } from './filesystem.js';
import type { IndexEntry } from './search.js';
import { todayDateString } from '../shared/utils.js';

const MAX_CLUSTERS = 40;
let rebuildTimer: ReturnType<typeof setTimeout> | null = null;
let storeCount = 0;
let inFlightRebuild: Promise<void> | null = null;

/** Cancel any pending cluster index rebuild. Call before vault teardown (e.g. in tests). */
export function cancelClusterRebuild(): void {
  if (rebuildTimer) {
    clearTimeout(rebuildTimer);
    rebuildTimer = null;
  }
  storeCount = 0;
}

/** Wait for any in-flight cluster rebuild to finish. Useful in tests before teardown. */
export async function waitForClusterRebuild(): Promise<void> {
  if (inFlightRebuild) {
    await inFlightRebuild;
    inFlightRebuild = null;
  }
}

/** Trigger a debounced cluster index rebuild. Called after each memory_store. */
export function scheduleClusterRebuild(getIndex: () => Map<string, IndexEntry>): void {
  storeCount++;
  if (storeCount >= 10) {
    storeCount = 0;
    inFlightRebuild = buildClusterIndex(getIndex()).finally(() => { inFlightRebuild = null; });
    return;
  }
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => {
    rebuildTimer = null;
    inFlightRebuild = buildClusterIndex(getIndex()).finally(() => { inFlightRebuild = null; });
  }, 5000); // 5-second debounce
}

/** Immediately rebuild the cluster index. Called after task_complete and memory_cleanup. */
export async function buildClusterIndex(index: Map<string, IndexEntry>): Promise<void> {
  try {
    const indexPath = path.join(CONFIG.VAULT_PATH, CONFIG.MEMORY_FOLDER, CONFIG.INDEX_FILE);
    const content = generateClusterContent(index);
    await writeAtomicFile(indexPath, content);
    logger.info('Cluster index rebuilt', { clusters: countClusters(content) });
  } catch (err) {
    logger.warn('Failed to rebuild cluster index', { error: String(err) });
  }
}

function countClusters(content: string): number {
  return (content.match(/^## /gm) ?? []).length;
}

function generateClusterContent(index: Map<string, IndexEntry>): string {
  const atoms = [...index.values()];
  if (atoms.length === 0) {
    return `# Memory Index\n_Generated: ${todayDateString()} — 0 atoms_\n\nNo atoms yet.\n`;
  }

  // Build tag co-occurrence: for each unique tag, find all atoms with that tag
  const tagAtoms = new Map<string, string[]>(); // tag -> [slug]
  for (const entry of atoms) {
    for (const tag of entry.frontmatter.tags) {
      const norm = tag.toLowerCase();
      const list = tagAtoms.get(norm) ?? [];
      list.push(entry.slug);
      tagAtoms.set(norm, list);
    }
  }

  // Sort tags by atom count descending, take top MAX_CLUSTERS
  const sortedTags = [...tagAtoms.entries()]
    .sort((a, b) => b[1].length - a[1].length)
    .slice(0, MAX_CLUSTERS);

  // Build a slug -> id lookup for efficient index access
  const slugToId = new Map<string, string>();
  for (const [id, entry] of index.entries()) {
    slugToId.set(entry.slug, id);
  }

  // Track which atoms are assigned to avoid double-listing
  const assigned = new Set<string>();
  const clusters: Array<{ tag: string; slugs: string[]; latestUpdated: string }> = [];

  for (const [tag, slugs] of sortedTags) {
    const unassigned = slugs.filter((s) => !assigned.has(s));
    if (unassigned.length === 0) continue;

    for (const s of unassigned) assigned.add(s);

    const latestUpdated = unassigned.reduce((best, slug) => {
      const id = slugToId.get(slug);
      const entry = id ? index.get(id) : undefined;
      const updated = entry?.frontmatter.updated ?? '';
      return updated > best ? updated : best;
    }, '');

    clusters.push({ tag, slugs: unassigned, latestUpdated });
  }

  // Collect unassigned atoms into an "Other" cluster
  const unassignedAtoms = atoms.filter((e) => !assigned.has(e.slug));

  const now = todayDateString();
  const totalAtoms = atoms.length;
  const clusterCount = clusters.length + (unassignedAtoms.length > 0 ? 1 : 0);

  let content = `# Memory Index\n_Generated: ${now} — ${totalAtoms} atoms across ${clusterCount} clusters_\n\n`;

  for (const cluster of clusters) {
    const representatives = cluster.slugs.slice(0, 3).map((s) => `[[${s}]]`).join(', ');
    const updated = cluster.latestUpdated ? cluster.latestUpdated.split('T')[0] : now;
    content += `## ${titleCase(cluster.tag)}\n`;
    content += `Representative: ${representatives}\n`;
    content += `Tags: \`${cluster.tag}\` | Count: ${cluster.slugs.length} | Updated: ${updated}\n\n`;
  }

  if (unassignedAtoms.length > 0) {
    const representatives = unassignedAtoms.slice(0, 3).map((e) => `[[${e.slug}]]`).join(', ');
    content += `## Unclustered\n`;
    content += `Representative: ${representatives}\n`;
    content += `Count: ${unassignedAtoms.length}\n\n`;
  }

  return content;
}

function titleCase(str: string): string {
  return str.replace(/-/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
}
