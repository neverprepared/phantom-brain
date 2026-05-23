import { todayDateString } from '../shared/utils.js';

export function memoryIndexTemplate(): string {
  return `# Memory Index
_Generated: ${todayDateString()} — 0 atoms_

Machine-generated cluster map. Updated after task_complete and every 10 memory_store calls.

<!-- clusters will be generated here -->
`;
}

export function wikiIndexTemplate(): string {
  return `# Wiki Index
_Updated: ${todayDateString()} — 0 pages_

Synthesized long-form knowledge. Human-readable. Start here for procedures, frameworks, and runbooks.

| File | Kind | Description | Updated |
|---|---|---|---|
`;
}

export function wikiSubfolderIndexTemplate(subfolder: string): string {
  return `# ${subfolder} Index
_Updated: ${todayDateString()} — 0 files_

| File | Description | Updated |
|---|---|---|
`;
}

export function wikiClaudeMdTemplate(): string {
  return `# Vault Behavioral Contract

## Layers

Every piece of content has exactly one home:

| Content type | Layer | Tool |
|---|---|---|
| Raw source material | Raw/gathered or Raw/curated | \`brain_learn\` / \`brain_perceive\` |
| Factual finding, conclusion | Memory/ atom | \`memory_store\` |
| Synthesized summary | Wiki/summaries/ | \`brain_synthesize\` |
| Entity extracted from source | Wiki/entities/ | \`brain_synthesize\` |

## Synthesis Loop

Source arrives in Raw/ → queued → \`brain_synthesize\` gates and writes summary + entity pages → \`brain_recall\` retrieves across Memory atoms and Wiki pages.

## Retrieval Protocol

1. \`brain_recall(query)\` — always first. Searches Memory atoms and Wiki pages together.
2. Summary pages live in Wiki/summaries/, entity pages in Wiki/entities/.
3. \`brain_reflect\` — maintenance pass: orphan detection, stale gate re-scoring, duplicate URL flagging.
`;
}
