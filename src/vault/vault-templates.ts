import { todayDateString } from '../shared/utils.js';

export function inputIndexTemplate(): string {
  return `# Input Index
_Updated: ${todayDateString()} — 0 files_

Source material ingested into the vault. Immutable — files are never edited after ingest.

| File | Kind | Captured | Description |
|---|---|---|---|
`;
}

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

export function outputIndexTemplate(): string {
  return `# Output Index
_Updated: ${todayDateString()} — 0 deliverables_

Deliverables produced from memory for external consumption.

| File | Kind | Status | Updated |
|---|---|---|---|
`;
}

export function outputSubfolderIndexTemplate(subfolder: string): string {
  return `# ${subfolder} Index
_Updated: ${todayDateString()} — 0 files_

| File | Description | Status | Updated |
|---|---|---|---|
`;
}

export function wikiClaudeMdTemplate(): string {
  return `# Vault Behavioral Contract

## Three Layers

Every piece of content has exactly one home:

| Content type | Layer | Tool |
|---|---|---|
| Factual finding, conclusion, ages out | Memory/ atom | \`memory_store\` |
| Procedure, framework, runbook, reference | Wiki/ page | \`wiki_write\` + atom linking to it |
| Deliverable for external consumption | Output/ | \`output_write\` |
| Source material (articles, transcripts, docs) | Input/ | \`input_ingest\` first, then synthesize |

## Ingestion Loop

New source arrives → \`input_ingest\` (Input/ first, immutable) → synthesize into atoms (Memory/) and/or Wiki pages → if deliverable needed, \`output_write\` (Output/).

## Index Discipline

Every write updates the containing subfolder \`_index.md\` AND the layer root \`_index.md\` in the same operation. Never leave an index out of sync.

## Log Discipline

Every mutation appends to \`_log/<date>.md\`. Format: \`HH:MMZ — <verb> <relpath> — <rationale>\`.

Verbs: \`ingest\`, \`store\`, \`wiki\`, \`output\`, \`append\`, \`archive\`.

## Input Immutability

Input/ files are never edited. New version = new file with dated suffix + \`_log/\` supersession entry (via \`input_supersede\`).

## Citation Graph

- Atoms cite Input sources via \`input_sources[]\` frontmatter
- Atoms cite Wiki pages via \`[[Wiki/...]]\` wiki-links in body
- Output cites atoms and Wiki pages via \`[[slug]]\` and \`[[Wiki/...]]\` in body
- Obsidian resolves backlinks natively — the graph is the knowledge lineage

## Retrieval Protocol

1. \`memory_search(query)\` — always first. Fast, machine-indexed atoms.
2. If atoms have \`[[Wiki/...]]\` links → \`wiki_read\` for depth
3. For frameworks/runbooks → \`wiki_list(subfolder)\` → \`wiki_read\`
4. For deliverables → \`output_list\` → \`output_read\`
5. For sources → \`input_list\` → \`input_read\`
`;
}
