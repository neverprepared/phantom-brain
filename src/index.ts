#!/usr/bin/env node

import { startServer } from './server.js';
import { logger } from './shared/logger.js';

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
