# mcp-phantom-brain

A [Model Context Protocol](https://modelcontextprotocol.io) server that gives Claude a structured, validated long-term memory backed by an Obsidian vault on disk.

Content enters through a **Raw → Gate → Wiki** pipeline. Sources are validated for reliability and classified by subject-matter topic before being written to the knowledge base. The host LLM drives the pipeline; the server enforces structure and provides the reference material.

## How it works

1. **Ingest** — `brain_perceive` (web content), `brain_learn` (curated docs), or `brain_attach` (binary files: PDF, Word, images) writes raw content to `Raw/` and enqueues it for processing. Both `brain_learn` and `brain_perceive` accept a single item or a batch of up to 100 via `items[]`.
2. **Gate** — `brain_synthesize` claims the next queue item and runs the Gate: a `claude` CLI call that scores the source for reliability (`high | medium | low | contested`), flags the failure category if unreliable, and classifies the subject-matter topic (`agents | memory | governance | tools | ...`).
3. **Synthesize** — the raw content is distilled into a concise prose summary via LLM, written to `Wiki/summaries/`, named entities are extracted from the raw content and fanned out to `Wiki/entities/`, and the `Raw → Wiki` mapping is recorded in `provenance.json`.
4. **Recall** — `brain_recall` searches summaries and entity pages via hybrid FTS5 + vector RRF, with optional topic pre-filtering.

## Tools

| Tool | Purpose |
|---|---|
| `brain_perceive` | Ingest a gathered web source (URL + content). Single item or batch via `items[]` (up to 100) |
| `brain_learn` | Ingest a curated document (human-trusted content). Single item or batch via `items[]` (up to 100) |
| `brain_attach` | Ingest a binary file (PDF, Word, image). Stores raw binary in `Raw/attachments/`, extracts text, queues for synthesis |
| `brain_synthesize` | Process 1–20 queued items: run Gate, distill summary via LLM, write summary + entity pages |
| `brain_recall` | Hybrid FTS5 + vector search; optional `topic` filter |
| `brain_reflect` | Maintenance pass: orphan detection, stale gate re-scoring, broken provenance cleanup, duplicate URL flagging, done/ pruning, log rotation, dead shard reaping |
| `brain_trace` | Query the synthesis audit trail (`_log.md`) by text, reliability, or date |
| `task_start` | Create a working memory task, auto-seeded from vault context |
| `task_update` | Append findings, steps, artifacts, and open questions to an active task |
| `task_complete` | Promote medium/high findings to the curated queue, then clear the task |
| `task_get` | Read current task state or list active tasks |

## The Gate

The Gate evaluates each gathered source before it enters the wiki. It never throws — any failure degrades to a `medium` fallback.

**Verdict fields:**
- `reliability` — `high | medium | low | contested`
- `category` — failure type when reliability is low/contested: `source | formal | informal | philosophical`
- `topic` — subject-matter bucket: `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

Curated sources (`brain_learn`) skip the LLM — human curation is the quality signal.

The `topic` field is stored in summary frontmatter and used by `brain_recall` for scoped retrieval.

## Vault structure

```
<vault>/
  Raw/
    curated/         ← brain_learn writes here
    gathered/        ← brain_perceive writes here
    attachments/     ← brain_attach stores raw binaries here (immutable, never deleted)
  Wiki/
    summaries/       ← one page per synthesized source
    entities/        ← one page per extracted entity, appended across sources
    _log.md          ← append-only synthesis audit trail
    _index.md        ← entity graduation index (Primary / Emerging / Notes tiers)
  _index/
    vectors.db       ← SQLite: FTS5 full-text index + sqlite-vec vector index
    provenance.json  ← Raw path → Wiki pages + reliability + topic
    queue/           ← pending/ and done/ QueueItem JSON files
    wm-<pid>.sqlite  ← per-process working memory (tasks, findings, artifacts)
```

### Obsidian drilldown

Summary and entity pages produced by `brain_attach` ingests include `[[wiki links]]` back to the original binary in `Raw/attachments/`. Clicking the link in Obsidian opens the PDF, image, or document inline.

### Batch ingest

`brain_learn` and `brain_perceive` accept either a single item (existing behaviour) or a batch of up to 100 via the `items` array:

```json
{
  "items": [
    { "content": "...", "title": "Doc A", "filename": "doc-a.md" },
    { "content": "...", "title": "Doc B", "filename": "doc-b.md" }
  ]
}
```

Provenance is read once per call; file writes are serialized to avoid slug collisions. Returns `{ status: "batch_complete", queued: N, duplicates: N, results: [...] }`.

## Search

`brain_recall` uses **hybrid RRF** (Reciprocal Rank Fusion) combining BM25 full-text and cosine vector similarity when Ollama is available. Falls back to FTS5-only otherwise.

The optional `topic` parameter pre-filters results to a subject-matter bucket before ranking, giving scoped recall without changing the query.

## Multi-agent support

Multiple MCP instances can safely share the same vault:

- Queue claiming uses atomic `rename()` — two agents cannot claim the same item
- All provenance writes (`upsertProvenanceEntry`, `deleteProvenanceEntry`) read inside a file lock
- `_index.md` updates read provenance inside the lock — no stale overwrites
- Entity pages use `upsertEntityPage()` — existence check and create/append in one lock
- `vectors.db` runs WAL mode with a 5s busy timeout
- Working memory is per-PID sharded — task spaces are naturally isolated

## Setup

**Prerequisites:** Node.js ≥ 18. Optionally [Ollama](https://ollama.ai) with `nomic-embed-text` for vector search.

```bash
git clone https://github.com/mindmorass/mcp-phantom-brain
cd mcp-phantom-brain
npm install
cp .env.example .env  # edit BRAIN_VAULT_PATH
npm run build
```

**Claude Code / Claude Desktop** — add to your MCP config:

```json
{
  "phantom-brain": {
    "command": "node",
    "args": ["/path/to/mcp-phantom-brain/dist/index.js"],
    "env": {
      "BRAIN_VAULT_PATH": "/path/to/your/vault"
    }
  }
}
```

> **Note:** Do not use nested shell fallback syntax (`${VAR:-${OTHER}}`) in the MCP env block — Claude Code partially expands it, leaving a trailing `}`. Use plain `${VAR}` references only.

## Configuration

| Var | Default | Purpose |
|---|---|---|
| `BRAIN_VAULT_PATH` | `~/...memory` | Vault root directory |
| `GATE_ENABLED` | `true` | Set to `false` to bypass the LLM gate (all gathered sources default to medium) |
| `GATE_MODEL` | `claude-haiku-4-5-20251001` | Model used for gate evaluation via the `claude` CLI |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Embeddings endpoint |
| `EMBEDDING_MODEL` | `nomic-embed-text` | Ollama model name |
| `EMBEDDING_DIMS` | `768` | Vector dimensions |
| `MCP_BRAIN_LOG_LEVEL` | `info` | Log verbosity (`debug|info|warn|error`) |

## Development

```bash
npm run dev       # run with tsx (no build required)
npm run typecheck # type-check without emitting
npm run build     # compile to dist/
```

There are no tests. `npm run typecheck` is the primary verification step.

## License

MIT
