import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
  type CallToolRequest,
  type CallToolResult,
} from '@modelcontextprotocol/sdk/types.js';
import { createRequire } from 'node:module';
import { initialize, shutdown, logger, CONFIG } from './core/index.js';

import { brainRecallToolDefinition, handleBrainRecall } from './tools/brain-recall.js';
import { brainRememberToolDefinition, handleBrainRemember } from './tools/brain-remember.js';
import { brainCommitToolDefinition, handleBrainCommit } from './tools/brain-commit.js';
import { brainReflectToolDefinition, handleBrainReflect } from './tools/brain-reflect.js';
import { brainWhyRejectedToolDefinition, handleBrainWhyRejected } from './tools/brain-why-rejected.js';

const _require = createRequire(import.meta.url);
const { version: pkgVersion } = _require('../package.json') as { version: string };

const SERVER_INFO = {
  name: 'mcp-brain',
  version: pkgVersion,
};

const toolDefinitions = [
  brainRecallToolDefinition,
  brainRememberToolDefinition,
  brainCommitToolDefinition,
  brainReflectToolDefinition,
  brainWhyRejectedToolDefinition,
];

const handlers: Record<string, (args: unknown) => Promise<CallToolResult>> = {
  brain_recall: handleBrainRecall,
  brain_remember: handleBrainRemember,
  brain_commit: handleBrainCommit,
  brain_reflect: handleBrainReflect,
  brain_why_rejected: handleBrainWhyRejected,
};

export async function startServer(): Promise<void> {
  logger.info('Starting mcp-brain server', {
    version: SERVER_INFO.version,
    vaultPath: CONFIG.VAULT_PATH,
  });

  await initialize();

  const server = new Server(
    {
      name: SERVER_INFO.name,
      version: SERVER_INFO.version,
    },
    {
      capabilities: {
        tools: {},
      },
    },
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => {
    logger.debug('Listing tools', { count: toolDefinitions.length });
    return { tools: toolDefinitions };
  });

  server.setRequestHandler(CallToolRequestSchema, async (request: CallToolRequest): Promise<CallToolResult> => {
    const toolName = request.params.name;
    logger.debug('Tool call received', { tool: toolName });
    const handler = handlers[toolName];
    if (!handler) {
      return {
        content: [{ type: 'text', text: `Unknown tool: ${toolName}` }],
        isError: true,
      };
    }
    return handler(request.params.arguments ?? {});
  });

  const transport = new StdioServerTransport();
  await server.connect(transport);

  logger.info('Server connected and ready', {
    transport: 'stdio',
    tools: toolDefinitions.length,
  });

  process.on('SIGINT', async () => {
    logger.info('Received SIGINT, shutting down');
    shutdown();
    await server.close();
    process.exit(0);
  });

  process.on('SIGTERM', async () => {
    logger.info('Received SIGTERM, shutting down');
    shutdown();
    await server.close();
    process.exit(0);
  });
}
