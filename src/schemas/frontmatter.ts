import { z } from 'zod';
import { logger } from '../shared/logger.js';

export const LifecycleStatusSchema = z.enum(['active', 'reference', 'archive']);
export type LifecycleStatus = z.infer<typeof LifecycleStatusSchema>;

export const ConfidenceSchema = z.enum(['low', 'medium', 'high']);
export type Confidence = z.infer<typeof ConfidenceSchema>;

/** Active status for the memory file within the vault (stale = past TTL, archived = manually archived). */
export const StatusSchema = z.enum(['active', 'stale', 'archived']);
export type Status = z.infer<typeof StatusSchema>;

export const SourceSchema = z.enum(['conversation', 'manual', 'import']);
export type Source = z.infer<typeof SourceSchema>;

/**
 * Phase 1 superset schema: accepts both legacy `para` and new `status` fields.
 * The normalization shim maps para → lifecycle_status when status is absent.
 * Phase 2 (after migration): remove para field entirely.
 */
export const FrontmatterSchema = z.object({
  id: z.string(),
  title: z.string().min(1).max(200),
  // New lifecycle field — replaces para
  lifecycle_status: LifecycleStatusSchema.optional(),
  // Legacy para field — Phase 1 only, will be removed in Phase 2
  para: z.enum(['projects', 'areas', 'resources', 'archives']).optional(),
  tags: z.array(z.string()).default([]),
  created: z.string(),
  updated: z.string(),
  source: SourceSchema.default('conversation'),
  related: z.array(z.string()).default([]),
  confidence: ConfidenceSchema.default('medium'),
  status: StatusSchema.default('active'),
  last_accessed: z.string().optional(),
  source_urls: z.array(z.string()).default([]),
  ttl_days: z.number().optional(),
  deadline: z.string().optional(),
  // New: vault-local Input/ sources this atom was synthesized from
  input_sources: z.array(z.string()).default([]),
  // New: Wiki pages this atom grounds (via wiki-links in body; also stored here for reverse discovery)
  wiki_refs: z.array(z.string()).default([]),
});

export type Frontmatter = z.infer<typeof FrontmatterSchema>;

/** TTL defaults by lifecycle_status when ttl_days not set. */
const LEGACY_PARA_TO_LIFECYCLE: Record<string, LifecycleStatus> = {
  projects: 'active',
  areas: 'active',
  resources: 'reference',
  archives: 'archive',
};

const LEGACY_PARA_TO_TTL: Record<string, number> = {
  projects: 30,
  areas: 90,
  resources: 180,
  archives: 365,
};

/**
 * Normalize a parsed frontmatter object for Phase 1 migration compatibility.
 * Maps legacy `para` → `lifecycle_status` + explicit `ttl_days`.
 * Warns on every normalization so stale atoms are visible in logs.
 * Throws on invalid normalized state so migration gaps surface clearly.
 */
export function normalizeFrontmatter(fm: Frontmatter, filePath?: string): Frontmatter {
  const location = filePath ? ` (${filePath})` : '';

  if (fm.lifecycle_status) {
    // Already on new schema — ensure ttl_days is present if not set
    return fm;
  }

  if (fm.para) {
    const mapped = LEGACY_PARA_TO_LIFECYCLE[fm.para];
    if (!mapped) {
      throw new Error(`Invalid legacy para value '${fm.para}'${location} — cannot normalize`);
    }
    logger.warn('Normalizing legacy para field to lifecycle_status', { para: fm.para, lifecycle_status: mapped, file: filePath });
    return {
      ...fm,
      lifecycle_status: mapped,
      ttl_days: fm.ttl_days ?? LEGACY_PARA_TO_TTL[fm.para],
    };
  }

  // Neither present — default to reference
  logger.warn('Memory file missing both para and lifecycle_status; defaulting to reference', { file: filePath });
  return {
    ...fm,
    lifecycle_status: 'reference',
    ttl_days: fm.ttl_days ?? 180,
  };
}
