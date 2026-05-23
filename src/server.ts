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
import { brainLearnToolDefinition, handleBrainLearn } from './tools/brain-learn.js';
import { brainPerceiveToolDefinition, handleBrainPerceive } from './tools/brain-perceive.js';
import { brainSynthesizeToolDefinition, handleBrainSynthesize } from './tools/brain-synthesize.js';
import { brainReflectToolDefinition, handleBrainReflect } from './tools/brain-reflect.js';
import {
  taskStartToolDefinition, handleTaskStart,
  taskUpdateToolDefinition, handleTaskUpdate,
  taskCompleteToolDefinition, handleTaskComplete,
  taskGetToolDefinition, handleTaskGet,
} from './tools/task.js';

const _require = createRequire(import.meta.url);
const { version: pkgVersion } = _require('../package.json') as { version: string };

const SERVER_INFO = {
  name: 'mcp-phantom-brain',
  version: pkgVersion,
};

const toolDefinitions = [
  brainRecallToolDefinition,
  brainLearnToolDefinition,
  brainPerceiveToolDefinition,
  brainSynthesizeToolDefinition,
  brainReflectToolDefinition,
  taskStartToolDefinition,
  taskUpdateToolDefinition,
  taskCompleteToolDefinition,
  taskGetToolDefinition,
];

const handlers: Record<string, (args: unknown) => Promise<CallToolResult>> = {
  brain_recall: handleBrainRecall,
  brain_learn: handleBrainLearn,
  brain_perceive: handleBrainPerceive,
  brain_synthesize: handleBrainSynthesize,
  brain_reflect: handleBrainReflect,
  task_start: handleTaskStart,
  task_update: handleTaskUpdate,
  task_complete: handleTaskComplete,
  task_get: handleTaskGet,
};

export async function startServer(): Promise<void> {
  logger.info('Starting mcp-phantom-brain server', {
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
