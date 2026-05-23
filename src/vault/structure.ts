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
    // Phase 0: Wiki/summaries and Wiki/entities
    path.join(vaultPath, CONFIG.WIKI_FOLDER, CONFIG.WIKI_SUMMARIES),
    path.join(vaultPath, CONFIG.WIKI_FOLDER, CONFIG.WIKI_ENTITIES),
    // Output layer
    path.join(vaultPath, CONFIG.OUTPUT_FOLDER),
    path.join(vaultPath, CONFIG.OUTPUT_FOLDER, '_attachments'),
    ...CONFIG.OUTPUT_SUBFOLDERS.map((s) => path.join(vaultPath, CONFIG.OUTPUT_FOLDER, s)),
    // System folders
    path.join(vaultPath, CONFIG.DAILY_FOLDER),
    path.join(vaultPath, CONFIG.INDEX_FOLDER),
    path.join(vaultPath, CONFIG.TEMPLATE_FOLDER),
    path.join(vaultPath, CONFIG.LOG_FOLDER),
    // Phase 0: Raw / queue
    path.join(vaultPath, CONFIG.RAW_FOLDER),
    path.join(vaultPath, CONFIG.RAW_GATHERED),
    path.join(vaultPath, CONFIG.RAW_CURATED),
    path.join(vaultPath, CONFIG.QUEUE_FOLDER),
    path.join(vaultPath, CONFIG.QUEUE_PENDING),
    path.join(vaultPath, CONFIG.QUEUE_DONE),
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

  // Phase 0: seed provenance.json and Wiki/_log.md if absent
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.INDEX_FOLDER, CONFIG.PROVENANCE_FILE),
    '{}\n',
  );
  await seedFileIfAbsent(
    path.join(vaultPath, CONFIG.WIKI_FOLDER, CONFIG.WIKI_LOG_FILE),
    '# Brain Synthesis Log\n\nAppend-only log of synthesis events. Newest entries at the bottom.\n\n',
  );

  logger.info('Vault structure ensured', { vaultPath });
}
