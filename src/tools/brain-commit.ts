import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { handleStore } from './store.js';
import { appendRejection } from '../log/rejection-log.js';
import { domainFromUrl } from '../validation/source-tiers.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainCommitSchema = z.object({
  decision: z.enum(['store', 'ask', 'reject']),
  reasoning: z.string().describe('One paragraph explaining the verdict'),
  confidence: z.enum(['low', 'medium', 'high']).optional().default('medium'),
  fallacies_detected: z.array(z.string()).optional().default([]),
  conflicting_atom_ids: z.array(z.string()).optional().default([]),
  // Fields carried from brain_remember's evaluation package
  content: z.string().describe('The original content being evaluated'),
  source_url: z.string().optional(),
  domain_tier: z.string().optional(),
  content_hash: z.string().optional(),
  // For store decision: optional metadata
  title: z.string().optional().describe('Title for the atom (auto-derived if omitted)'),
  tags: z.array(z.string()).optional().default([]),
  ttl_days: z.number().optional(),
});

export const brainCommitToolDefinition = {
  name: 'brain_commit',
  description:
    'Commit the host LLM\'s verdict on a claim previously surfaced by brain_remember. ' +
    'decision=store creates an atom in Memory/. decision=reject logs the rejection with ' +
    'reasoning. decision=ask asks the user for clarification before recommitting.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      decision: { type: 'string', enum: ['store', 'ask', 'reject'] },
      reasoning: { type: 'string', description: 'One paragraph explaining the verdict' },
      confidence: {
        type: 'string',
        enum: ['low', 'medium', 'high'],
        description: 'Confidence level when decision=store (default medium)',
      },
      fallacies_detected: {
        type: 'array',
        items: { type: 'string' },
        description: 'Logical fallacies the LLM detected in the claim',
      },
      conflicting_atom_ids: {
        type: 'array',
        items: { type: 'string' },
        description: 'IDs of existing atoms this claim conflicts with',
      },
      content: { type: 'string', description: 'The original content being evaluated' },
      source_url: { type: 'string' },
      domain_tier: { type: 'string' },
      content_hash: { type: 'string' },
      title: { type: 'string', description: 'Title for the atom (auto-derived if omitted)' },
      tags: { type: 'array', items: { type: 'string' } },
      ttl_days: { type: 'number' },
    },
    required: ['decision', 'reasoning', 'content'],
  },
};

function extractIdFromStoreResult(result: CallToolResult): string | undefined {
  const first = result.content[0];
  if (!first || first.type !== 'text') return undefined;
  const match = first.text.match(/ID:\s*(\S+)/);
  return match ? match[1] : undefined;
}

export async function runBrainCommit(input: z.infer<typeof BrainCommitSchema>) {
  const {
    decision, reasoning, confidence, fallacies_detected, conflicting_atom_ids,
    content, source_url, domain_tier, content_hash, title, tags, ttl_days,
  } = input;

  if (decision === 'ask') {
    return {
      status: 'awaiting_clarification' as const,
      question: reasoning,
      action: 'Provide clarification and call brain_commit again with store or reject.',
    };
  }

  // Log if rejected, or if storing low-confidence content with fallacies (disputed)
  const isDisputedStore = decision === 'store'
    && confidence === 'low'
    && fallacies_detected.length > 0;

  if (decision === 'reject' || isDisputedStore) {
    await appendRejection({
      timestamp: new Date().toISOString(),
      content_hash: content_hash ?? '',
      ...(source_url !== undefined && { source_url }),
      domain: domainFromUrl(source_url),
      domain_tier: domain_tier ?? 'unknown',
      verdict: decision === 'reject' ? 'rejected' : 'disputed',
      reasoning,
      conflicting_atom_ids,
      fallacies_detected,
    });

    if (decision === 'reject') {
      return {
        status: 'rejected' as const,
        reasoning,
        logged: true,
        message: 'Claim rejected and logged. Query with brain_why_rejected to retrieve the reasoning.',
      };
    }
  }

  if (decision === 'store') {
    const derivedTitle = title ?? content.replace(/\*\*/g, '').trim().slice(0, 80).split(/[.!?\n]/)[0]?.trim() ?? 'Untitled claim';
    const finalTitle = derivedTitle.length > 0 ? derivedTitle : 'Untitled claim';
    const sourceTtl = ttl_days ?? (confidence === 'high' ? 180 : confidence === 'medium' ? 90 : 60);
    const finalTags = [
      ...tags,
      ...(conflicting_atom_ids.length > 0 ? ['disputed'] : []),
    ];

    const storeResult = await handleStore({
      title: finalTitle,
      content,
      lifecycle_status: 'active',
      tags: finalTags,
      source: 'import',
      source_urls: source_url ? [source_url] : [],
      confidence,
      ttl_days: sourceTtl,
    });

    if (storeResult.isError) {
      const firstContent = storeResult.content[0];
      const errText = firstContent && firstContent.type === 'text' ? firstContent.text : 'unknown error';
      return {
        status: 'store_failed' as const,
        error: errText,
        reasoning,
      };
    }

    const id = extractIdFromStoreResult(storeResult);

    return {
      status: 'stored' as const,
      id,
      title: finalTitle,
      confidence,
      reasoning,
      disputed: conflicting_atom_ids.length > 0,
      conflicting_atom_ids,
    };
  }

  // Should never reach — decision was validated by Zod
  return {
    status: 'unknown_decision' as const,
    decision,
  };
}

export async function handleBrainCommit(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainCommitSchema.parse(args);
    const result = await runBrainCommit(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_commit failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_commit: ${formatError(err)}` }],
      isError: true,
    };
  }
}
