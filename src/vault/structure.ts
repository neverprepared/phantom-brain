import fs from 'node:fs/promises';
import path from 'node:path';
import { CONFIG } from '../config.js';
import { logger } from '../shared/logger.js';
import {
  inputIndexTemplate,
  memoryIndexTemplate,
  wikiIndexTemplate,
  wikiSubfolderIndexTemplate,
  outputIndexTemplate,
  outputSubfolderIndexTemplate,
  wikiClaudeMdTemplate,
} from './vault-templates.js';

async function seedFileIfAbsent(filePath: string, content: string): Promise<void> {
  try {
    await fs.access(filePath);
  } catch {
    await fs.mkdir(path.dirname(filePath), { recursive: true });
    await fs.writeFile(filePath, content, 'utf-8');
  }
}

export async function ensureVaultStructure(): Promise<void> {
  const vaultPath = CONFIG.VAULT_PATH;

  // Create all required directories
  const dirs = [
    // Memory layer — flat, no subfolders
    path.join(vaultPath, CONFIG.MEMORY_FOLDER),
    // Input layer
    path.join(vaultPath, CONFIG.INPUT_FOLDER),
    ...CONFIG.INPUT_SUBFOLDERS.map((s) => path.join(vaultPath, CONFIG.INPUT_FOLDER, s)),
    // Wiki layer
    path.join(vaultPath, CONFIG.WIKI_FOLDER),
    ...CONFIG.WIKI_SUBFOLDERS.map((s) => path.join(vaultPath, CONFIG.WIKI_FOLDER, s)),
    // Output layer
    path.join(vaultPath, CONFIG.OUTPUT_FOLDER),
    path.join(vaultPath, CONFIG.OUTPUT_FOLDER, '_attachments'),
    ...CONFIG.OUTPUT_SUBFOLDERS.map((s) => path.join(vaultPath, CONFIG.OUTPUT_FOLDER, s)),
    // System folders
    path.join(vaultPath, CONFIG.DAILY_FOLDER),
    path.join(vaultPath, CONFIG.INDEX_FOLDER),
    path.join(vaultPath, CONFIG.TEMPLATE_FOLDER),
    path.join(vaultPath, CONFIG.LOG_FOLDER),
  ];

  for (const dir of dirs) {
    await fs.mkdir(dir, { recursive: true });
  }

  // Seed index files if absent
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.MEMORY_FOLDER, CONFIG.INDEX_FILE),
    memoryIndexTemplate(),
  );
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.INPUT_FOLDER, CONFIG.INDEX_FILE),
    inputIndexTemplate(),
  );
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.WIKI_FOLDER, CONFIG.INDEX_FILE),
    wikiIndexTemplate(),
  );
  for (const sub of CONFIG.WIKI_SUBFOLDERS) {
    await seedFileIfAbsent(
      path.join(vaultPath, CONFIG.WIKI_FOLDER, sub, CONFIG.INDEX_FILE),
      wikiSubfolderIndexTemplate(sub),
    );
  }
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.OUTPUT_FOLDER, CONFIG.INDEX_FILE),
    outputIndexTemplate(),
  );
  for (const sub of CONFIG.OUTPUT_SUBFOLDERS) {
    await seedFileIfAbsent(
      path.join(vaultPath, CONFIG.OUTPUT_FOLDER, sub, CONFIG.INDEX_FILE),
      outputSubfolderIndexTemplate(sub),
    );
  }

  // Seed Wiki/CLAUDE.md with behavioral contract
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.WIKI_FOLDER, 'CLAUDE.md'),
    wikiClaudeMdTemplate(),
  );

  logger.info('Vault structure ensured', { vaultPath });
}
