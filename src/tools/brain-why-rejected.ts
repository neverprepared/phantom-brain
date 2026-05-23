import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { queryRejections } from '../log/rejection-log.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainWhyRejectedSchema = z.object({
  query: z.string().describe('Topic or keyword to look up in the rejection log'),
  limit: z.number().optional().default(10),
});

export const brainWhyRejectedToolDefinition = {
  name: 'brain_why_rejected',
  description:
    'Query the rejection log to find out why a claim was previously rejected or disputed. ' +
    'Searches reasoning text, source domains, fallacy names, and conflicting atom IDs.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      query: { type: 'string', description: 'Topic or keyword to look up in the rejection log' },
      limit: { type: 'number', description: 'Max rejections to return (default 10)' },
    },
    required: ['query'],
  },
};

export async function runBrainWhyRejected(input: z.infer<typeof BrainWhyRejectedSchema>) {
  const entries = await queryRejections(input.query, input.limit);

  if (entries.length === 0) {
    return { found: 0, message: 'No rejections found matching that query.' };
  }

  return {
    found: entries.length,
    rejections: entries.map((e) => ({
      timestamp: e.timestamp,
      source_url: e.source_url,
      domain: e.domain,
      domain_tier: e.domain_tier,
      verdict: e.verdict,
      reasoning: e.reasoning,
      fallacies_detected: e.fallacies_detected,
      conflicting_atom_ids: e.conflicting_atom_ids,
    })),
  };
}

export async function handleBrainWhyRejected(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainWhyRejectedSchema.parse(args);
    const result = await runBrainWhyRejected(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_why_rejected failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_why_rejected: ${formatError(err)}` }],
      isError: true,
    };
  }
}
