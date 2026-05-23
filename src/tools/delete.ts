import { formatError } from '../shared/errors.js';
import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import { DeleteInputSchema } from '../schemas/tools.js';
import { findById, removeFromIndex } from '../vault/search.js';
import { deleteMemoryFile } from '../vault/filesystem.js';
import { removeBacklinks } from '../vault/links.js';
import { logger } from '../shared/logger.js';

export const deleteToolDefinition = {
  name: 'memory_delete',
  description:
    'Permanently delete a memory. Removes the file from the vault. Consider archiving instead.',
  inputSchema: {
    type: 'object' as const,
    properties: {
      id: { type: 'string', description: 'Memory ID to delete' },
      confirm: { type: 'boolean', description: 'Must be true to confirm deletion' },
    },
    required: ['id', 'confirm'],
  },
};

export async function handleDelete(args: unknown): Promise<CallToolResult> {
  try {
    const input = DeleteInputSchema.parse(args);

    const entry = findById(input.id);
    if (!entry) {
      return {
        content: [{ type: 'text', text: `Memory not found: ${input.id}` }],
        isError: true,
      };
    }

    const deletedSlug = entry.slug;
    const deletedTitle = entry.frontmatter.title;

    await deleteMemoryFile(entry.filePath);
    removeFromIndex(input.id);

    // Clean up backlinks in other memories
    const { cleaned, failed } = await removeBacklinks(deletedSlug);
    if (failed.length > 0) {
      logger.warn('Some backlinks could not be cleaned after delete', { deletedSlug, failed });
    }

    logger.info('Deleted memory', { id: input.id, slug: deletedSlug, cleanedBacklinks: cleaned.length });

    const cleanedInfo = cleaned.length > 0 ? `\nCleaned backlinks from: ${cleaned.length} memor${cleaned.length === 1 ? 'y' : 'ies'}` : '';

    return {
      content: [
        {
          type: 'text',
          text: `Deleted memory: "${deletedTitle}" (${input.id})${cleanedInfo}`,
        },
      ],
    };
  } catch (error) {
    logger.error('Failed to delete memory', { error: String(error) });
    return {
      content: [
        {
          type: 'text',
          text: `Error deleting memory: ${formatError(error)}`,
        },
      ],
      isError: true,
    };
  }
}
