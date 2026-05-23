#!/usr/bin/env node
/**
 * Seed the vault: create folder structure.
 * Safe to re-run — existing files are not overwritten.
 */
import { ensureVaultStructure } from '../vault/structure.js';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';

async function main(): Promise<void> {
  logger.info('Initializing brain vault', { vaultPath: CONFIG.VAULT_PATH });
  await ensureVaultStructure();
  logger.info('Brain vault initialized');
}

main().catch((err) => {
  logger.error('Initialization failed', { error: String(err) });
  process.exit(1);
});
