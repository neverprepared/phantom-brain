import { formatError } from '../shared/errors.js';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { CleanupInputSchema } from '../schemas/tools.js';
import { getIndex } from '../vault/search.js';
import type { IndexEntry } from '../vault/search.js';
import { buildIncomingLinkCount } from '../vault/links.js';
import { isStale } from '../shared/utils.js';
import { handleUpdate } from './update.js';
import { handleDelete } from './delete.js';
import { logger } from '../shared/logger.js';

export const cleanupToolDefinition = {
  name: 'memory_cleanup',
  description:
    'Find and clean up stale, archived, or orphaned memories. Defaults to dry_run:true which only lists candidates without making changes.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      action: {
        type: 'string',
        enum: ['list', 'archive', 'delete'],
        description: 'Action to take: "list" (show candidates), "archive" (set status=archived), "delete" (permanently remove). Default: list',
      },
      target: {
        type: 'string',
        enum: ['stale', 'archived', 'orphan'],
        description: 'Which memories to target: "stale" (past TTL), "archived" (status=archived), "orphan" (no links). Default: stale',
      },
      dry_run: {
        type: 'boolean',
        description: 'If true (default), only list candidates without making changes',
      },
      limit: {
        type: 'number',
        description: 'Max candidates to process (default: 20, max: 50)',
      },
      confirm: {
        type: 'boolean',
        description: 'Required to be true when action is "delete"',
      },
    },
  },
};

export async function handleCleanup(args: unknown): Promise<CallToolResult> {
  try {
    const input = CleanupInputSchema.parse(args);
    const index = getIndex();

    // Collect candidates based on target
    let candidates: IndexEntry[] = [];

    if (input.target === 'stale') {
      candidates = [...index.values()].filter(
        (e) =>
          isStale(e.frontmatter.updated, e.frontmatter.ttl_days, e.frontmatter.lifecycle_status) &&
          e.frontmatter.status !== 'archived'
      );
    } else if (input.target === 'archived') {
      candidates = [...index.values()].filter((e) => e.frontmatter.status === 'archived');
    } else if (input.target === 'orphan') {
      const incomingCount = buildIncomingLinkCount(index);
      candidates = [...index.values()].filter((e) => {
        const hasOutgoing = e.frontmatter.related.length > 0;
        const hasIncoming = (incomingCount.get(e.slug) ?? 0) > 0;
        return !hasOutgoing && !hasIncoming;
      });
    }

    candidates = candidates.slice(0, input.limit);

    // List or dry run — show candidates without acting
    if (input.action === 'list' || input.dry_run) {
      if (candidates.length === 0) {
        const label = input.dry_run && input.action !== 'list' ? `DRY RUN — no ${input.target} memories found` : `No ${input.target} memories found`;
        return { content: [{ type: 'text', text: label }] };
      }

      const lines = candidates.map((e) => {
        const fm = e.frontmatter;
        const lc = fm.lifecycle_status ?? fm.para ?? 'unknown';
        return `- **${fm.title}** [${lc}/${fm.status}] — ID: ${fm.id}`;
      });

      const prefix = input.dry_run && input.action !== 'list'
        ? `DRY RUN — would ${input.action} ${candidates.length} ${input.target} memor${candidates.length === 1 ? 'y' : 'ies'}:\n\n`
        : `Found ${candidates.length} ${input.target} memor${candidates.length === 1 ? 'y' : 'ies'}:\n\n`;

      return { content: [{ type: 'text', text: prefix + lines.join('\n') }] };
    }

    // Perform action
    let succeeded = 0;
    let failed = 0;
    const failures: string[] = [];

    for (const candidate of candidates) {
      try {
        if (input.action === 'archive') {
          const result = await handleUpdate({ id: candidate.frontmatter.id, status: 'archived' });
          if (result.isError) {
            failed++;
            failures.push(`${candidate.frontmatter.id}: ${result.content[0]?.type === 'text' ? result.content[0].text : 'unknown error'}`);
          } else {
            succeeded++;
          }
        } else if (input.action === 'delete') {
          // confirm:true is safe here — already enforced by CleanupInputSchema refine above
          const result = await handleDelete({ id: candidate.frontmatter.id, confirm: input.confirm });
          if (result.isError) {
            failed++;
            failures.push(`${candidate.frontmatter.id}: ${result.content[0]?.type === 'text' ? result.content[0].text : 'unknown error'}`);
          } else {
            succeeded++;
          }
        }
      } catch (err) {
        failed++;
        failures.push(`${candidate.frontmatter.id}: ${String(err)}`);
        logger.warn('Cleanup action failed', { id: candidate.frontmatter.id, action: input.action, error: String(err) });
      }
    }

    const parts = [`Cleanup complete: ${succeeded} ${input.action}d, ${failed} failed.`];
    if (failures.length > 0) {
      parts.push(`\nFailures:\n${failures.join('\n')}`);
    }

    return { content: [{ type: 'text', text: parts.join('') }] };
  } catch (error) {
    logger.error('Failed to run cleanup', { error: String(error) });
    return {
      content: [
        {
          type: 'text',
          text: `Error running cleanup: ${formatError(error)}`,
        },
      ],
      isError: true,
    };
  }
}
