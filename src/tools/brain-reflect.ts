import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { handleCleanup } from './cleanup.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainReflectSchema = z.object({
  scope: z.enum(['full', 'cleanup_only', 'conflicts_only']).optional().default('full'),
});

export const brainReflectToolDefinition = {
  name: 'brain_reflect',
  description:
    'Periodic brain maintenance: prune stale atoms, surface conflicts, synthesize concepts. ' +
    'Currently implements stale-atom cleanup; conflict resolution and concept synthesis are ' +
    'planned for future phases.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      scope: {
        type: 'string',
        enum: ['full', 'cleanup_only', 'conflicts_only'],
        description: 'Which reflection passes to run (default: full)',
      },
    },
  },
};

function extractText(result: CallToolResult): string {
  const first = result.content[0];
  if (!first || first.type !== 'text') return '';
  return first.text;
}

export async function runBrainReflect(input: z.infer<typeof BrainReflectSchema>) {
  const results: Record<string, unknown> = {};

  if (input.scope === 'full' || input.scope === 'cleanup_only') {
    const cleanup = await handleCleanup({
      target: 'stale',
      action: 'delete',
      dry_run: false,
      limit: 50,
      confirm: true,
    });
    results['cleanup'] = {
      isError: cleanup.isError ?? false,
      output: extractText(cleanup),
    };
  }

  results['note'] = 'Conflict resolution and concept synthesis are planned for a future phase.';

  return results;
}

export async function handleBrainReflect(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainReflectSchema.parse(args);
    const result = await runBrainReflect(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_reflect failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_reflect: ${formatError(err)}` }],
      isError: true,
    };
  }
}
