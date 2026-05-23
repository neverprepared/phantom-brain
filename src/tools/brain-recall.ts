import { z } from 'zod';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { searchMemories } from '../vault/search.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainRecallSchema = z.object({
  query: z.string().describe('What to look up in memory'),
  freshness: z.enum(['all', 'fresh', 'stale']).optional().default('all'),
  limit: z.number().optional().default(10),
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
    },
    required: ['query'],
  },
};

export async function runBrainRecall(input: z.infer<typeof BrainRecallSchema>) {
  const results = await searchMemories({
    query: input.query,
    limit: input.limit,
    freshness: input.freshness,
  });

  const atomResults = results.filter((r) => r.resultKind === 'atom');
  const fresh = atomResults.filter((r) => !r.stale);
  const stale = atomResults.filter((r) => r.stale);

  let suggested_action: 'use_cached' | 'refresh' | 'learn_first';
  if (results.length === 0) {
    suggested_action = 'learn_first';
  } else if (stale.length > 0 && fresh.length === 0) {
    suggested_action = 'refresh';
  } else {
    suggested_action = 'use_cached';
  }

  return {
    results: results.map((r) => ({
      kind: r.resultKind,
      id: r.resultKind === 'atom' ? r.entry?.frontmatter.id : undefined,
      path: r.resultKind === 'wiki' ? r.wikiEntry?.relPath : undefined,
      title: r.resultKind === 'atom' ? r.entry?.frontmatter.title : r.wikiEntry?.title,
      excerpt: r.snippet ?? '',
      confidence: r.resultKind === 'atom' ? r.entry?.frontmatter.confidence : undefined,
      freshness: r.resultKind === 'atom' ? (r.stale ? 'stale' : 'fresh') : 'fresh',
      score: r.score,
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
