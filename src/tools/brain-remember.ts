import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { scoreDomain } from '../validation/source-tiers.js';
import { findNearDuplicate } from '../validation/duplicate.js';
import { checkCoherence } from '../validation/coherence.js';
import { buildEvaluationPackage, type RelevantAtom } from '../prompts/evaluate-claim.js';
import { searchMemories } from '../vault/search.js';
import { hashContent } from '../log/rejection-log.js';
import { readMemoryFile } from '../vault/filesystem.js';
import { parseMemoryFile } from '../vault/frontmatter.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainRememberSchema = z.object({
  content: z.string().describe('The claim or information to evaluate for storage'),
  source_url: z.string().optional().describe('URL where this information came from'),
  context: z.string().optional().describe('Additional context about why this is being remembered'),
});

export const brainRememberToolDefinition = {
  name: 'brain_remember',
  description:
    'Submit a claim to the brain for evaluation. Runs fast deterministic checks (coherence, ' +
    'source-domain tier, near-duplicate). If the claim passes Layer 1, returns a structured ' +
    'evaluation package containing relevant memory atoms and an LLM-facing prompt. The host LLM ' +
    'reads the prompt, decides store|ask|reject, and calls brain_commit with the verdict.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      content: { type: 'string', description: 'The claim or information to evaluate for storage' },
      source_url: { type: 'string', description: 'URL where this information came from' },
      context: { type: 'string', description: 'Additional context about why this is being remembered' },
    },
    required: ['content'],
  },
};

async function loadExcerpt(filePath: string, fallback: string): Promise<string> {
  try {
    const raw = await readMemoryFile(filePath);
    const parsed = parseMemoryFile(raw, filePath);
    return parsed.content.slice(0, 200);
  } catch {
    return fallback;
  }
}

export async function runBrainRemember(input: z.infer<typeof BrainRememberSchema>) {
  const { content, source_url } = input;

  // Layer 1: fast deterministic checks
  const coherence = checkCoherence(content);
  const domainTier = scoreDomain(source_url);
  const contentHash = hashContent(content);

  if (!coherence.passed) {
    return {
      status: 'rejected_layer1' as const,
      reason: coherence.reason,
      action: 'No evaluation needed — content failed structural check.',
    };
  }

  if (domainTier === 'low_quality') {
    return {
      status: 'rejected_layer1' as const,
      reason: 'Source domain is flagged as low quality.',
      domain_tier: domainTier,
      action: 'No evaluation needed — source is low quality. Call brain_commit with decision=reject if you want to log this.',
    };
  }

  const duplicateId = await findNearDuplicate(content, content.slice(0, 80));

  // Pull relevant atoms for evaluation context
  const searchResults = await searchMemories({ query: content.slice(0, 120), limit: 6 });
  const relevantAtoms: RelevantAtom[] = [];
  for (const r of searchResults) {
    if (r.resultKind !== 'atom' || !r.entry) continue;
    const fallbackExcerpt = r.snippet ?? '';
    const excerpt = fallbackExcerpt.length > 40
      ? fallbackExcerpt
      : await loadExcerpt(r.entry.filePath, fallbackExcerpt);
    relevantAtoms.push({
      id: r.entry.frontmatter.id,
      title: r.entry.frontmatter.title,
      excerpt,
      confidence: r.entry.frontmatter.confidence ?? 'medium',
      tags: r.entry.frontmatter.tags ?? [],
    });
  }

  return buildEvaluationPackage({
    content,
    ...(source_url !== undefined && { sourceUrl: source_url }),
    domainTier,
    isDuplicate: !!duplicateId,
    ...(duplicateId !== undefined && { duplicateId }),
    coherencePassed: coherence.passed,
    ...(coherence.reason !== undefined && { coherenceReason: coherence.reason }),
    contentHash,
    relevantAtoms,
  });
}

export async function handleBrainRemember(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainRememberSchema.parse(args);
    const result = await runBrainRemember(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_remember failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_remember: ${formatError(err)}` }],
      isError: true,
    };
  }
}
