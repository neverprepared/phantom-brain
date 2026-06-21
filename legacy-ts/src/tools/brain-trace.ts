/**
 * brain_trace — query the Wiki/_log.md synthesis audit trail.
 *
 * Parses the append-only log written by brain_synthesize and returns
 * matching entries in reverse-chronological order (newest first).
 *
 * Filters: free-text query (title/source), reliability tier, date range,
 * and a limit cap.
 */
import { z } from 'zod';
import fs from 'node:fs/promises';
import path from 'node:path';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';
import { formatError } from '../shared/errors.js';

export const BrainTraceSchema = z.object({
  query: z.string().optional().describe('Filter by title or source path (case-insensitive substring)'),
  reliability: z.enum(['high', 'medium', 'low', 'contested', 'pending']).optional()
    .describe('Filter by gate verdict'),
  since: z.string().optional().describe('ISO date string — only entries at or after this timestamp'),
  limit: z.number().int().min(1).max(200).optional().default(20),
});

export const brainTraceToolDefinition = {
  name: 'brain_trace',
  description:
    'Query the Wiki/_log.md synthesis audit trail. Returns synthesis events in reverse-chronological ' +
    'order (newest first). Filter by free-text query (title/source), reliability tier, or date. ' +
    'Use this to audit what has been synthesized, trace a source through the pipeline, or check ' +
    'gate verdicts.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      query: {
        type: 'string',
        description: 'Case-insensitive substring match against entry title or source path',
      },
      reliability: {
        type: 'string',
        enum: ['high', 'medium', 'low', 'contested', 'pending'],
        description: 'Filter to entries with this gate reliability verdict',
      },
      since: {
        type: 'string',
        description: 'ISO date string — only return entries at or after this timestamp',
      },
      limit: {
        type: 'number',
        description: 'Max entries to return (default 20, max 200)',
      },
    },
  },
};

interface TraceEntry {
  timestamp: string;
  title: string;
  source: string;
  summary: string;
  entities: string[];
  reliability: string;
  gate_reason: string;
}

function parseLogEntries(content: string): TraceEntry[] {
  const entries: TraceEntry[] = [];

  // Split on `## ` headings — each heading starts a new entry
  const blocks = content.split(/\n(?=## )/);

  for (const block of blocks) {
    const headingMatch = block.match(/^## ([^\n]+)/);
    if (!headingMatch || !headingMatch[1]) continue;

    // Heading format: "<ISO timestamp> — <title>"
    const heading = headingMatch[1].trim();
    const dashIdx = heading.indexOf(' — ');
    if (dashIdx < 0) continue;

    const timestamp = heading.slice(0, dashIdx).trim();
    const title = heading.slice(dashIdx + 3).trim();

    const sourceMatch = block.match(/^- Source: (.+)$/m);
    const summaryMatch = block.match(/^- Summary: (.+)$/m);
    const entitiesMatch = block.match(/^- Entities: (.+)$/m);
    const gateMatch = block.match(/^- Gate: (.+)$/m);

    const source = sourceMatch?.[1]?.trim() ?? '';
    const summary = summaryMatch?.[1]?.trim() ?? '';

    const entitiesRaw = entitiesMatch?.[1]?.trim() ?? '';
    const entities = entitiesRaw === '(none extracted)' || entitiesRaw === ''
      ? []
      : entitiesRaw.split(',').map((e) => e.trim()).filter(Boolean);

    // Gate line format: "<reliability> — <reason>" or "pending (...)"
    const gateRaw = gateMatch?.[1]?.trim() ?? '';
    let reliability = 'unknown';
    let gate_reason = gateRaw;
    const gateDash = gateRaw.indexOf(' — ');
    if (gateDash >= 0) {
      reliability = gateRaw.slice(0, gateDash).trim().toLowerCase();
      gate_reason = gateRaw.slice(gateDash + 3).trim();
    } else if (gateRaw.toLowerCase().startsWith('pending')) {
      reliability = 'pending';
      gate_reason = gateRaw.replace(/^pending\s*/i, '').replace(/^\(|\)$/g, '');
    }

    entries.push({ timestamp, title, source, summary, entities, reliability, gate_reason });
  }

  return entries;
}

export async function runBrainTrace(input: z.infer<typeof BrainTraceSchema>) {
  const logPath = path.join(CONFIG.VAULT_PATH, CONFIG.WIKI_FOLDER, CONFIG.WIKI_LOG_FILE);

  let content: string;
  try {
    content = await fs.readFile(logPath, 'utf-8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
      return { entries: [], total: 0, message: 'No synthesis log found — nothing has been synthesized yet.' };
    }
    throw err;
  }

  let entries = parseLogEntries(content);

  // Apply filters
  if (input.query) {
    const q = input.query.toLowerCase();
    entries = entries.filter((e) =>
      e.title.toLowerCase().includes(q) || e.source.toLowerCase().includes(q),
    );
  }

  if (input.reliability) {
    entries = entries.filter((e) => e.reliability === input.reliability);
  }

  if (input.since) {
    entries = entries.filter((e) => e.timestamp >= input.since!);
  }

  // Reverse to newest-first, then cap
  entries.reverse();
  const total = entries.length;
  const page = entries.slice(0, input.limit);

  const parts: string[] = [`Found ${total} matching synthesis event(s).`];
  if (total > input.limit) parts.push(`Showing ${input.limit} most recent.`);

  logger.info('brain_trace', { total, returned: page.length, query: input.query, reliability: input.reliability });

  return {
    entries: page,
    total,
    message: parts.join(' '),
  };
}

export async function handleBrainTrace(args: unknown): Promise<CallToolResult> {
  try {
    const input = BrainTraceSchema.parse(args);
    const result = await runBrainTrace(input);
    return {
      content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
    };
  } catch (err) {
    logger.error('brain_trace failed', { error: String(err) });
    return {
      content: [{ type: 'text', text: `Error in brain_trace: ${formatError(err)}` }],
      isError: true,
    };
  }
}
