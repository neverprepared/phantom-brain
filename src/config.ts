import path from 'node:path';

function resolveVaultPath(): string {
  const home = process.env['HOME'] || '';
  const fromBrain = process.env['BRAIN_VAULT_PATH'];
  const fromObsidian = process.env['OBSIDIAN_VAULT_PATH'];
  const raw = fromBrain || fromObsidian || path.join(home, 'workspaces/profiles/personal/obsidian/vaults/memory');
  // Expand a leading ~ to $HOME so users can write ~/foo in env files
  if (raw.startsWith('~')) {
    return path.join(home, raw.slice(1));
  }
  return raw;
}

export const CONFIG = {
  VAULT_PATH: resolveVaultPath(),
  // Layer folders
  MEMORY_FOLDER: 'Memory',
  INPUT_FOLDER: 'Input',
  INPUT_SUBFOLDERS: ['articles', 'docs', 'transcripts', 'notes'] as const,
  WIKI_FOLDER: 'Wiki',
  WIKI_SUBFOLDERS: ['HowTos', 'Runbooks', 'References', 'Scratch'] as const,
  OUTPUT_FOLDER: 'Output',
  OUTPUT_SUBFOLDERS: ['articles', 'reports', 'decks'] as const,
  // System folders
  DAILY_FOLDER: '_daily',
  INDEX_FOLDER: '_index',
  TEMPLATE_FOLDER: '_templates',
  LOG_FOLDER: '_log',
  // Index file name used at each layer root and subfolder
  INDEX_FILE: '_index.md',
  // Limits
  MAX_TITLE_LENGTH: 200,
  MAX_SLUG_LENGTH: 60,
  DEFAULT_SEARCH_LIMIT: 10,
  MAX_SEARCH_LIMIT: 50,
  DEFAULT_LIST_LIMIT: 20,
  MAX_LIST_LIMIT: 100,
  // Auto-linking
  MIN_TAG_JACCARD: 0.4,
  MAX_AUTO_LINKS_PER_STORE: 10,
  MAX_AUTO_LINKS_PER_ATOM: 25,
  // Embeddings
  OLLAMA_BASE_URL: process.env['OLLAMA_BASE_URL'] || 'http://localhost:11434',
  EMBEDDING_MODEL: process.env['EMBEDDING_MODEL'] || 'nomic-embed-text',
  EMBEDDING_DIMS: parseInt(process.env['EMBEDDING_DIMS'] || '768', 10),
  EMBEDDING_BATCH_SIZE: parseInt(process.env['EMBEDDING_BATCH_SIZE'] || '50', 10),
} as const;

/** Default TTL by lifecycle status (days). Used when ttl_days not set on atom. */
export const DEFAULT_TTL_DAYS: Record<string, number> = {
  active: 90,
  reference: 180,
  archive: 365,
};

export function memoryFolderPath(): string {
  return path.join(CONFIG.VAULT_PATH, CONFIG.MEMORY_FOLDER);
}

export function getVaultPath(): string {
  return CONFIG.VAULT_PATH;
}
