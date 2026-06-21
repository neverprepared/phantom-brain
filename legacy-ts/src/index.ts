#!/usr/bin/env node

import { startServer } from './server.js';
import { logger } from './shared/logger.js';

// Keep the process alive on unhandled async errors rather than crashing.
// Tool handlers already catch their own errors and return isError responses;
// these handlers are a last-resort net for background tasks (embedding sync,
// fire-and-forget vector writes) that have no other catch path.
process.on('uncaughtException', (err) => {
  logger.error('Uncaught exception', {
    error: err.message,
    stack: err.stack,
  });
});

process.on('unhandledRejection', (reason) => {
  logger.error('Unhandled rejection', {
    reason: reason instanceof Error ? reason.message : String(reason),
    stack: reason instanceof Error ? reason.stack : undefined,
  });
});

async function main(): Promise<void> {
  try {
    await startServer();
  } catch (error) {
    logger.error('Failed to start server', {
      error: error instanceof Error ? error.message : String(error),
      stack: error instanceof Error ? error.stack : undefined,
    });
    process.exit(1);
  }
}

main();
