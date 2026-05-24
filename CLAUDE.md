# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Development (runs via tsx, no build required)
npm run dev

# Build (TypeScript → dist/, copies seed files)
npm run build

# Type check only
npm run typecheck

# Run the built server
npm start

# Initialize vault structure (one-time setup)
npm run init
```

There are no tests. `npm run typecheck` is the primary verification step.

## Architecture

mcp-phantom-brain is a **Model Context Protocol server** implementing a Raw → Gate → Wiki synthesis pipeline backed by an Obsidian vault on disk. It communicates over stdio using the `@modelcontextprotocol/sdk`.

### Entry points

- `src/index.ts` — process entry; calls `startServer()`
- `src/server.ts` — registers MCP tools and connects the stdio transport
- `src/core/index.ts` — `initialize()` / `shutdown()` lifecycle; called once at startup

### MCP tools

| Tool | File | Purpose |
|---|---|---|
| `brain_learn` | `tools/brain-learn.ts` | Ingest a curated document into `Raw/curated/` and enqueue for synthesis |
| `brain_perceive` | `tools/brain-perceive.ts` | Ingest a gathered web source into `Raw/gathered/` and enqueue for gate + synthesis |
| `brain_synthesize` | `tools/brain-synthesize.ts` | Claim 1–20 queue items (`count` param), run the Gate, distill summary via LLM, write summary + entity pages, append to `_log.md` |
| `brain_recall` | `tools/brain-recall.ts` | FTS5 + vector hybrid search over Wiki summaries and entity pages; supports `topic` filter |
| `brain_reflect` | `tools/brain-reflect.ts` | Maintenance pass: orphan detection, stale gate re-scoring, broken provenance auto-cleanup, duplicate URL flagging, done/ pruning (30d), log rotation (5000-line cap), dead WM shard reaping |
| `brain_trace` | `tools/brain-trace.ts` | Query `Wiki/_log.md` synthesis audit trail; filter by query, reliability, or date |
| `task_start` | `tools/task.ts` | Create a working memory task, auto-seeded from vault context |
| `task_update` | `tools/task.ts` | Append findings, steps, artifacts, and open questions to an active task |
| `task_complete` | `tools/task.ts` | Promote medium/high findings to `Raw/curated/` queue, then clear the task |
| `task_get` | `tools/task.ts` | Read current task state or list active tasks |

### Ingest → Synthesis pipeline

1. **`brain_learn`** (curated) or **`brain_perceive`** (gathered) writes raw content to `Raw/` and enqueues a `QueueItem` in `_index/queue/`.
2. **`brain_synthesize`** claims 1–20 queue items (optional `count` parameter, default 1), runs the Gate (`src/gate/evaluate.ts`), distills the raw content into concise prose via `summarizeContent()` (falls back to raw content on failure), writes the summary page to `Wiki/summaries/`, fans out into entity pages under `Wiki/entities/` (entity extraction still runs on raw content for full coverage), appends a log line to `Wiki/_log.md`, and records the `Raw → Wiki` mapping in `_index/provenance.json`.
3. **`brain_recall`** searches the indexed summaries and entity pages via hybrid RRF (FTS5 + vector). Supports optional `topic` filter to scope results to a subject-matter bucket.
4. **`brain_trace`** queries the append-only `_log.md` for audit and debugging.

Duplicate detection is by SHA256 at ingest time — re-submitting the same byte-for-byte content is a no-op.

### The Gate (`src/gate/evaluate.ts`)

`runGate()` never throws. It returns a `GateVerdict` with fields:

- `reliability` — `high | medium | low | contested`
- `category?` — `source | formal | informal | philosophical` (required when reliability is low/contested)
- `topic?` — subject-matter classification: `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

**Curated sources** (`brain_learn`) skip the LLM. Human curation is the quality signal; verdict is fixed `medium`.

**Gathered sources** (`brain_perceive`) run the Phase 2 LLM gate: combines domain tier (`src/validation/source-tiers.ts`) with content preview and calls the `claude` CLI for a JSON verdict. Uses the subscription credentials already active in the running Claude Code session — no separate API key required. Falls back to `medium` on any failure (CLI unavailable, timeout, parse error).

Domain tiers: `authoritative | credible | unknown | low_quality`.

The `topic` field is written to summary page frontmatter (`topic: agents`) and used by `brain_recall` for pre-filter scoping.

`summarizeContent()` is also exported from `evaluate.ts`. It reuses `callClaudeCLI` with a 45s timeout and generates a 3–5 paragraph prose distillation of the raw content. Returns `null` on any failure (gate disabled, CLI error, timeout) so callers can fall back to raw content.

### Vault structure

Path is resolved from `BRAIN_VAULT_PATH` env var (falls back to `~/workspaces/profiles/personal/obsidian/vaults/memory`).

```
<vault>/
  Raw/
    curated/         ← brain_learn writes here (human-curated docs)
    gathered/        ← brain_perceive writes here (web content)
  Wiki/
    summaries/       ← one summary page per synthesized source
    entities/        ← one page per extracted entity, appended across sources
    _log.md          ← append-only synthesis audit trail (brain_trace reads this)
    _index.md        ← graduated index by entity reference count (auto-refreshed)
  _index/
    vectors.db       ← SQLite DB containing both FTS5 and sqlite-vec indexes
    provenance.json  ← Raw path → Wiki pages + reliability + topic mapping
    queue/           ← pending/ and done/ QueueItem JSON files
    wm-<pid>.sqlite  ← per-process working memory DB (tasks, findings, artifacts)
```

### Indexing pipeline (`src/vault/search.ts`)

On startup, `buildIndex()` reads every Wiki file and populates:

1. **In-memory map** — `wikiIndex` (id → `WikiIndexEntry`), caches parsed frontmatter + body including `topic`
2. **SQLite FTS5** — BM25 full-text search (stored in `vectors.db`)
3. **sqlite-vec vector index** — cosine similarity via Ollama embeddings (also in `vectors.db`)

`searchMemories()` uses **hybrid RRF** (Reciprocal Rank Fusion) combining FTS5 and vector ranks when Ollama is available; falls back to FTS5-only or in-memory keyword scan otherwise.

On a cache miss, `resolveWikiEntry()` reads the file from disk and self-heals the map — this ensures entries written by a concurrent MCP instance are always reachable.

New summary and entity pages are indexed incrementally via `indexWikiEntry()` after each `brain_synthesize` call — no full rebuild needed.

### Working memory (`src/working/`)

Per-process SQLite DB (`_index/wm-<pid>.sqlite`) tracks in-progress tasks with findings, steps, and artifacts. The PID-sharded filename provides natural isolation between concurrent MCP instances.

- `db.ts` — schema, CRUD operations; WAL mode + 5s busy_timeout enabled
- `retrieval.ts` — seeds new tasks with relevant vault context via `brain_recall`
- `promotion.ts` — on `task_complete`, promotes medium/high findings to `Raw/curated/` queue for synthesis

Dead-process shards are detected at startup (orphaned active tasks collected, shard deleted) and also reaped by `brain_reflect` on every maintenance pass.

### Multi-agent safety

All shared writes are atomic under file locks or SQLite WAL:

- **Queue claiming** — atomic `rename()` (pending → claimed); two agents cannot claim the same item
- **Entity pages** — `upsertEntityPage()` holds the file lock across the existence check and create/append
- **Provenance** — `upsertProvenanceEntry()` and `deleteProvenanceEntry()` read inside the lock; no stale overwrites
- **`_index.md`** — `updateWikiIndex()` reads provenance inside the lock; entire tally-write is atomic
- **FTS5 + vectors** — WAL mode + 5s busy_timeout on `vectors.db`
- **In-memory map** — cache misses fall back to disk reads so entries from other instances are found

### Configuration (`src/config.ts`)

All tunables are in `CONFIG`. Key env vars:

| Var | Default | Purpose |
|---|---|---|
| `BRAIN_VAULT_PATH` | `~/workspaces/.../memory` | Vault root |
| `GATE_ENABLED` | `true` | Set to `false` to disable Phase 2 gate; all gathered sources default to medium |
| `GATE_MODEL` | `claude-haiku-4-5-20251001` | Model passed to the `claude` CLI for gate evaluation |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Embeddings endpoint |
| `EMBEDDING_MODEL` | `nomic-embed-text` | Ollama model |
| `EMBEDDING_DIMS` | `768` | Vector dimensions |
| `MCP_BRAIN_LOG_LEVEL` | `info` | Log verbosity |

Copy `.env.example` to `.env` and adjust before running.

**Gotcha — Claude Code MCP env expansion**: Claude Code partially evaluates shell fallback syntax (`${VAR:-${OTHER}}`), expanding inner `${OTHER}` but leaving the outer `}` as a literal character in the resolved value. `resolveVaultPath()` strips trailing `}` characters to compensate. Do not use nested fallback syntax in the MCP server config env block; use simple `${VAR}` references only.

### Seed files

`src/seed/wiki/References/` contains reference wiki pages (logical fallacies, philosophical razors, etc.) that are copied into the vault on first startup. These are never overwritten once they exist.

### Utility scripts

`scripts/backfill-topics.mjs` — one-shot script to classify existing summary pages that predate the `topic` field. Calls the claude CLI per summary, patches frontmatter in-place, skips already-classified files. Safe to re-run.
