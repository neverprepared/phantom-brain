import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { searchMemories } from '../vault/search.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainRecallSchema = z.object({
  query: z.string().describe('What to look up in memory'),
  freshness: z.enum(['all', 'fresh', 'stale']).optional().default('all'),
  limit: z.number().optional().default(10),
  topic: z.enum([
    'agents', 'memory', 'governance', 'tools', 'training',
    'infrastructure', 'knowledge', 'multiagent', 'general',
  ]).optional().describe('Restrict results to a specific subject-matter topic'),
});

export const brainRecallToolDefinition = {
  name: 'brain_recall',
  description:
    'Look up information from the brain (memory atoms + wiki pages, ranked together). ' +
    'Returns a structured result set with a suggested_action hint (use_cached | refresh | learn_first).',
  inputSchema: {
    type: 'object' as const,
    properties: {
      query: { type: 'string', description: 'What to look up in memory' },
      freshness: {
        type: 'string',
        enum: ['all', 'fresh', 'stale'],
        description: 'Restrict to fresh (within TTL), stale (past TTL), or all atoms. Wiki pages are always fresh.',
      },
      limit: { type: 'number', description: 'Max results to return (default 10)' },
      topic: {
        type: 'string',
        enum: ['agents', 'memory', 'governance', 'tools', 'training', 'infrastructure', 'knowledge', 'multiagent', 'general'],
        description: 'Restrict results to a specific subject-matter topic',
      },
    },
    required: ['query'],
  },
};

export async function runBrainRecall(input: z.infer<typeof BrainRecallSchema>) {
  const results = await searchMemories({
    query: input.query,
    limit: input.limit,
    freshness: input.freshness,
    ...(input.topic ? { topic: input.topic } : {}),
  });

  const suggested_action: 'use_cached' | 'learn_first' = results.length === 0 ? 'learn_first' : 'use_cached';

  return {
    results: results.map((r) => ({
      kind: r.resultKind,
      path: r.wikiEntry?.relPath,
      title: r.wikiEntry?.title,
      excerpt: r.snippet ?? '',
      freshness: 'fresh',
      score: r.score,
      ...(r.wikiEntry?.source_attachment ? { source_attachment: r.wikiEntry.source_attachment } : {}),
    })),
    suggested_action,
    summary: `Found ${results.length} result(s). Suggested action: ${suggested_action}.`,
  };
}

export async function handleBrainRecall(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainRecallSchema.parse(args);
    const result = await runBrainRecall(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_recall failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_recall: ${formatError(err)}` }],
      isError: true,
    };
  }
}
