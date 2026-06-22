# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build (CGO + sqlite_fts5 build tag — both required)
make build

# Run every test (uses sqlite_fts5 tag automatically)
make test

# Race tests on the concurrency-sensitive packages
make test-race

# Tidy + vet + build + test in one shot before pushing
make check
```

Plain `go build`/`go test` won't work — `internal/index` panics at init() without the `sqlite_fts5` build tag. Always use `make` or `GOFLAGS=-tags=sqlite_fts5 go test ./...`.

## Architecture

phantom-brain is a **Model Context Protocol server** that gives Claude Code (and any other MCP-compatible agent) long-term durable memory plus per-session active memory. The implementation is Go, the runtime split is two-process:

- **`pbrainctl mcp`** — per-agent stdio MCP server. Talks to the daemon over HTTP. Holds local SQLite for active memory + a snapshot cache for fast recall. Spawned by Claude Code per session.
- **`pbrainctl serve`** — single HTTP daemon. The canonical store. Receives writes, persists to OpenSearch + MinIO, runs async synth (gate + distill via the `claude` CLI), publishes snapshot tarballs the agents pull at birth.

Phase 6 (v2.0+) moved the canonical content store from a local Obsidian vault to **OpenSearch + MinIO**. The local-filesystem "vault" still exists for legacy bulk-migration paths and tests, but agent reads + writes target the daemon, not disk.

### Three storage tiers (memory model)

| Tier | What it is | Where in code |
|---|---|---|
| **Long-term memory** | OpenSearch indices (`pb_summaries`, `pb_entities`, `pb_attachments`) + MinIO for attachment blobs. Canonical, durable, shared across all agents bound to the same vault. | `internal/osearch/` + the daemon HTTP surface in `internal/server/` |
| **Active memory** | Per-process SQLite at `_index/wm-<pid>.sqlite`. Tasks, findings, artifacts, open questions. Lives only for the agent process; dropped at exit. | `internal/working/` |
| **Read cache** | Per-brain `_index/vectors.db` (sqlite-vec + FTS5). A snapshot of the daemon's OS view, pulled as a tarball at birth. Read-only — the agent doesn't write directly. | `internal/index/` + `internal/brain/` birth machinery |

Promotion path: `task_complete` aggregates important findings into a single markdown note and POSTs it as a `brain_learn` to the daemon → lands in long-term as a `task_summary` doc.

### Entry points

| Path | Role |
|---|---|
| `cmd/pbrainctl/main.go` | CLI entry. Subcommand dispatcher: `mcp` (agent), `serve` (daemon), `ingest-bulk`, `vault`, `snapshot`, etc. |
| `internal/server/server.go::Start()` | Daemon lifecycle: load config, build OS client, init MinIO backend, spawn `SynthWorker` + `SnapshotDebouncer`, mount chi router |
| `internal/mcp/server.go::Register()` | MCP tool registration. Mounts every `brain_*` and `task_*` tool against an `mcp-go` server. |
| `internal/brain/lifecycle.go::Start()` | Per-agent birth: claim a brain dir, pull current snapshot, start heartbeat, prep deps for the MCP tools. |

### MCP tools (the public surface)

| Tool | File | Purpose | Writes to |
|---|---|---|---|
| `brain_perceive` | `internal/mcp/ingest.go` | Ingest gathered web content. Kind: `web_scrape`. | Long-term (daemon → OS) |
| `brain_learn` | `internal/mcp/learn.go` | Ingest a curated note. Kind: `note`. Skips LLM gate (defaults to medium reliability). | Long-term |
| `brain_attach` | `internal/mcp/attach.go` | Ingest a binary file. Kind: `attachment_stub`. Bytes → MinIO; metadata → OS. | Long-term + MinIO |
| `brain_recall` | `internal/mcp/recall.go` | Hybrid BM25 + kNN over the local read cache. Optional `topic` filter. | Reads only |
| `brain_trace` | `internal/mcp/trace.go` | Read the local Wiki/_log.md audit trail. | Reads only |
| `brain_checkpoint` | `internal/mcp/brain_checkpoint.go` | Force a checkpoint of the working-memory state. | Local working DB |
| `brain_status` | `internal/mcp/brain_status.go` | Report brain state (gen, snapshot SHA, heartbeat age). | Reads only |
| `brain_death` | `internal/mcp/brain_death.go` | Flip brain status to dead. Phase 6: no payload tarball, just status + log marker. | Local manifest |
| `task_start` | `internal/mcp/task.go` | Create a working-memory task, auto-seeded from a `brain_recall` against the goal. | Active (local WM) |
| `task_update` | `internal/mcp/task.go` | Append a finding / artifact / question. | Active |
| `task_complete` | `internal/mcp/task.go` | Promote important findings to long-term via `brain_learn`. Kind: `task_summary`. | Active → Long-term |
| `task_get` | `internal/mcp/task.go` | Read current task state. | Reads only |

### Data flow on a write

```
agent (Claude Code)
  ↓ brain_perceive("title", body)
internal/mcp/ingest.go
  ├─ canonicalize.SumBody → SHA (content-stable across re-ingest)
  ├─ Index.Has(SHA) → "Duplicate" early-out
  ├─ Ollama.Embed(title + "\n\n" + body) → 768-dim vector
  └─ brain.Client.Perceive(req)              ← agent-side HTTP
       ↓
internal/server/handlers_write.go::handlePerceive
  ├─ validate SHA, title/body, Kind enum
  ├─ osearch.Client.UpsertSummary(doc, synthesised=false)
  └─ SynthWorker.Enqueue(profile, vault, sha)
       ↓ (async, on daemon goroutine)
internal/server/synth_queue.go::processJob
  ├─ osearch.Client.GetSummary
  ├─ CheckCoherence (heuristic)
  ├─ RunGate via `claude` CLI → reliability/topic/category JSON
  ├─ SummarizeContent via `claude` CLI → distilled 3-5 paragraph body
  ├─ ExtractEntities (heuristic on raw body, denylist filtered)
  ├─ UpsertEntity per extracted name (append MentionedBy[])
  ├─ UpsertSummary (synthesised=true)
  └─ OnComplete → SnapshotDebouncer.Trigger
       ↓ (60-second debounce; fires after burst settles)
internal/server/snapshot_export.go::BuildSnapshotFromOS
  ├─ osearch.Export → tarball with _index/vectors.db inside
  ├─ Write _published/snapshot-<gen>.tar.zst (atomic temp+rename)
  ├─ Write .sha256 + .manifest.json sidecars
  └─ Bump .gen-counter
```

Next agent birth pulls the new gen via `GET /api/brain/snapshot/current` → downloads tarball → extracts into `brain_dir/` → opens `_index/vectors.db` for local recall.

### Schema (v2.4 — memory classification)

Every long-term doc carries these fields beyond title/body/topic/reliability:

| Field | Type | Purpose |
|---|---|---|
| `kind` | keyword (closed enum) | What shape of memory: `note` \| `web_scrape` \| `task_summary` \| `attachment_stub` \| `email_import` \| `manual_curate` |
| `memory_type` | keyword (Tulving) | Optional. `semantic` (facts) \| `episodic` (events) \| `procedural` (how-to) \| empty (undecided) |
| `source[]` | keyword (multi) | Provenance: URLs, `task:<id>`, `agent:<id>`, `from_email:<addr>`, file paths. Used for the "where did this come from" facet. |
| `references[]` | keyword (multi) | SHAs of related summaries. Graph hook — populated by `task_complete`, future `brain_link`, or LLM during distill. |
| `captured_at` | date | When the underlying content was authored. Distinct from `created_at` (when OS got it). |
| `capture_minio_key` | keyword | MinIO key of the raw page bytes captured at synth time (when `[capture]` enabled). Empty when capture is off, URL absent, or fetch failed. Retrieve via `GET /api/brain/capture/{sha}` → presigned MinIO URL. |

Adding a new `kind` value: edit the constant block in `internal/osearch/docs.go`, ship the new daemon. No reindex; OS field is `keyword` (any string fits), the enum is application-validated.

`tags[]` swallows v1's legacy `type/vendor/category` frontmatter from the bulk loader — they get prefixed like `vendor:UIA`, `type:invoice`, `category:utilities` and dropped into `tags[]` for keyword aggregation.

### Vault structure (daemon-side, legacy-shaped)

Resolved from `$PHANTOM_BRAIN_DATA_DIR` in the daemon's env (defaults under the data root):

```
<data>/<profile>/<vault>/
  _index/
    .gen-counter            ← monotonic snapshot generation
  _published/
    snapshot-<gen>.tar.zst  ← agent pulls these at birth
    snapshot-<gen>.tar.zst.sha256
    snapshot-<gen>.manifest.json
  locks/
    maintenance.flag        ← present = pause writes
```

Agent-side (per brain instance):

```
$XDG_DATA_HOME/phantom-brain/<profile>/<vault>/brains/<brain_id>/
  manifest.json             ← alive | shutting_down | dead, heartbeat, parent_gen
  markers/<brain_id>        ← heartbeat sentinel
  vectors.db                ← legacy v1 location; ignored in v2
  _index/
    vectors.db              ← snapshot extract — what `internal/index` opens
    wm-<pid>.sqlite         ← per-process working memory
```

`_index/vectors.db` is what hybrid recall reads. The `vault/` subdir under each brain holds the extracted snapshot's Wiki/ when one exists.

### Config layout

```
$PHANTOM_BRAIN_CONFIG_DIR/
  server.toml                                      ← [server] [storage] [opensearch] [defaults]
  profiles/<profile>/vaults/<vault>/
    auth.toml                                      ← bearer_token = "..."
    config.toml                                    ← (optional) per-vault VaultOverrides
```

Bearer tokens live in `auth.toml`, ONE per vault binding. The daemon's registry walks `profiles/*/vaults/*/auth.toml` at startup + on SIGHUP.

### The Gate (`internal/server/gate.go`)

`RunGate()` is the daemon-side LLM call. It shells out to the `claude` CLI (bundled in the Docker image since v2.2.0), authenticated via `CLAUDE_CODE_OAUTH_TOKEN` (Claude Max subscription credentials, NOT `ANTHROPIC_API_KEY`).

Verdict fields (matching `osearch.SummaryDoc`):
- `reliability` — `high | medium | low | contested`
- `category` — `source | formal | informal | philosophical` (required when reliability is low/contested)
- `topic` — closed set: `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

Curated sources (`brain_learn`) skip the LLM and get a fixed `medium` verdict (curation is the quality signal).

`SummarizeContent()` is the distill pass — same CLI, different prompt. Produces the `body` field; `raw_body` keeps the original.

Without the OAuth token the daemon still starts; gate + distill fall through to raw-content fallback (body == raw_body, reliability defaults to medium, topic defaults to general).

### Migration tooling (`pbrainctl ingest-bulk`)

One-shot bulk loader for either a legacy Obsidian vault (the migration we're doing now) OR any future on-disk format the operator wants to import.

```bash
pbrainctl ingest-bulk <path> [--dry-run] [--concurrency N] [--max-file-bytes N] [--timeout-secs N]
```

Routes by directory:
- `Raw/curated/*.md` → `brain_learn` (Kind: note; email-shaped frontmatter → email_import)
- `Raw/gathered/*.md` → `brain_perceive` (Kind: web_scrape)
- `Raw/attachments/*` → `brain_attach` (Kind: attachment_stub)

Embeddings computed locally via Ollama. Idempotent — daemon dedups by SHA. `--dry-run` walks the tree and prints the plan without POSTing.

### Concurrency invariants

- **Queue claiming**: SynthWorker uses an in-memory channel, single worker per daemon. No file-queue. Restart drops queued-but-not-processed jobs (Phase 7 will durable-queue).
- **Entity pages**: `UpsertEntity` reads-modifies-writes the OS doc. Safe under single-worker; would need OS painless scripts for multi-worker.
- **Snapshot rebuild**: `SnapshotDebouncer` per-vault timer registry. Bursts collapse into one rebuild per `snapshot_rebuild_debounce_secs` (default 60s).
- **Doc-ID separator**: SHA-based, format `<profile>:<vault>:<sha>`. Colon, not slash — opensearch-go interpolates IDs raw into URL paths and slashes silently 404.
- **Embedding zero check**: OS rejects all-zero vectors under cosine similarity. Either send nil (caller didn't compute) or a real embedding.

### Configuration knobs (env / server.toml)

| Var | Default | Purpose |
|---|---|---|
| `PHANTOM_BRAIN_CONFIG_DIR` | `~/.config/phantom-brain-server` | Daemon config root |
| `PHANTOM_BRAIN_DATA_DIR` | `~/.local/share/phantom-brain-server` | Daemon data root |
| `XDG_DATA_HOME` | `~/.local/share` | Where agent brain dirs live |
| `CL_BRAIN_API` | — | Agent: daemon URL (`https://pbrain.example.com`) |
| `CL_BRAIN_API_TOKEN` | — | Agent: bearer token matching daemon's `auth.toml` |
| `CL_WORKSPACE_PROFILE` | — | Agent: profile binding |
| `CL_BRAIN_VAULT` | — | Agent: vault binding |
| `CLAUDE_CODE_OAUTH_TOKEN` | — | Daemon: subscription credentials for `claude` CLI |
| `BRAIN_VAULT_PATH` | — | Legacy: enables pre-v2 BRAIN_VAULT_PATH-only mode (no daemon contract) |

### Seed files

`internal/osearch/` ensures the three indices at daemon startup. There are no "seed" content files anymore (v1's `src/seed/wiki/` is gone).

### Utility scripts

- `cmd/pbrainctl/ops.go` — operator subcommands: `vault list`, `snapshot status`, `maintenance enter/exit`, `brain list`, etc.
- `scripts/seed-vault-via-minio.sh` — bootstrap a remote MinIO with a seed tarball (rare).

## What changed in Phase 6 (v2.0+)

Major break from v1 (TypeScript / Obsidian / single-process). If you're reading old v1.x docs:

- Old: TypeScript MCP server backed by Obsidian markdown vault on disk. New: Go MCP server (agent) + Go HTTP daemon (canonical store) + OpenSearch + MinIO.
- Old: synthesizer polled a filesystem queue. New: in-memory channel + single `SynthWorker` goroutine, debounced snapshot rebuild.
- Old: agents shipped death-payload tarballs at shutdown; daemon's reaper merged them. New: agent POSTs writes synchronously during life; no death payload; no reaper.
- Old: snapshot tarball was a reflink of the local Wiki tree. New: `osearch.Export` bulk-scrolls OS into a fresh sqlite-vec + FTS5 db.
- Old: attachments stored on local filesystem under `Raw/attachments/<sha><ext>`. New: MinIO at `<profile>/<vault>/attachments/<sha><ext>`.

The vault format on disk (Raw/curated, Raw/gathered, Raw/attachments) survives only for the bulk-migration path — the daemon doesn't read it during normal operation.
