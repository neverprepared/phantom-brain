import path from 'node:path';

function resolveVaultPath(): string {
  const home = process.env['HOME'] || '';
  const fromBrain = process.env['BRAIN_VAULT_PATH'];
  const fromObsidian = process.env['OBSIDIAN_VAULT_PATH'];
  const raw = fromBrain || fromObsidian || path.join(home, 'workspaces/profiles/personal/obsidian/vaults/memory');
  // Expand a leading ~ to $HOME so users can write ~/foo in env files
  let resolved = raw.startsWith('~') ? path.join(home, raw.slice(1)) : raw;
  // Claude Code's MCP env expansion partially evaluates shell fallback syntax
  // (e.g. ${VAR:-${OTHER}}) leaving a trailing '}'. Strip it.
  resolved = resolved.replace(/}+$/, '');
  return resolved;
}

export const CONFIG = {
  VAULT_PATH: resolveVaultPath(),
  // Layer folders
  WIKI_FOLDER: 'Wiki',
  WIKI_SUBFOLDERS: [] as const,
  // System folders
  DAILY_FOLDER: '_daily',
  INDEX_FOLDER: '_index',
  TEMPLATE_FOLDER: '_templates',
  LOG_FOLDER: '_log',
  // Brain model Phase 0 — Raw / queue / wiki extensions
  RAW_FOLDER: 'Raw',
  RAW_GATHERED: 'Raw/gathered',
  RAW_CURATED: 'Raw/curated',
  RAW_ATTACHMENTS: 'Raw/attachments',
  QUEUE_FOLDER: '_queue',
  QUEUE_PENDING: '_queue/pending',
  QUEUE_DONE: '_queue/done',
  WIKI_SUMMARIES: 'summaries',   // subfolder under Wiki/
  WIKI_ENTITIES: 'entities',     // subfolder under Wiki/
  WIKI_LOG_FILE: '_log.md',      // under Wiki/
  PROVENANCE_FILE: 'provenance.json',  // under _index/
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
  // Gate (Phase 2) — LLM source-reliability judgment
  GATE_MODEL: process.env['GATE_MODEL'] || 'claude-haiku-4-5-20251001',
  GATE_ENABLED: process.env['GATE_ENABLED'] !== 'false',  // default on
} as const;

export function getVaultPath(): string {
  return CONFIG.VAULT_PATH;
}
