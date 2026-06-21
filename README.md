# mcp-phantom-brain

A [Model Context Protocol](https://modelcontextprotocol.io) server that gives Claude a structured, validated long-term memory backed by an Obsidian vault on disk.

Content enters through a **Raw → Gate → Wiki** pipeline. Sources are validated for reliability and classified by subject-matter topic before being written to the knowledge base. The host LLM drives the pipeline; the server enforces structure and provides the reference material.

> **Implementation note.** The current codebase is the Go rewrite (v5.0 spec, Phases 0–2.5). A single `pbrainctl` binary subsumes the MCP server, the HTTP synthesis daemon, and the operator CLI. The original TypeScript implementation now lives under [`legacy-ts/`](./legacy-ts/) and is frozen pending deletion.

## How it works

1. **Ingest** — `brain_perceive` (web content), `brain_learn` (curated docs), or `brain_attach` (binary files: PDF, Word, images) writes raw content to `Raw/` and enqueues it for processing.
2. **Gate** — the synthesizer claims the next queue item and runs the Gate: a `claude` CLI call that scores the source for reliability (`high | medium | low | contested`) and classifies the subject-matter topic (`agents | memory | governance | tools | …`).
3. **Synthesize** — raw content is distilled into a prose summary via LLM, written to `Wiki/summaries/`, named entities are extracted and fanned out to `Wiki/entities/`, and the mapping is recorded in `provenance.json`.
4. **Recall** — `brain_recall` searches summaries and entity pages via hybrid FTS5 + vector RRF, with optional topic pre-filtering.

In agent-contract mode (v5.0), the lifecycle adds **mortal brains**: each MCP process births from a daemon-published snapshot, accumulates work locally, and ships a trimmed death payload back to the daemon on shutdown. The daemon merges + synthesises into the collective vault and publishes the next snapshot. See the v5.0 spec for the full lifecycle.

## Binary

```
pbrainctl mcp              # stdio JSON-RPC MCP server (per agent process)
pbrainctl serve            # HTTP synthesis daemon (multi-vault, per-vault reaper + synth)
pbrainctl migrate-legacy   # one-time port of an old vault into the v5.0 layout

pbrainctl vault            # list | status | reload (SIGHUPs the daemon)
pbrainctl snapshot         # status | rebuild | prune | claims  [profile/vault]
pbrainctl queue            # depth | contributors  [profile/vault]
pbrainctl maintenance      # enter | exit          [profile/vault]
pbrainctl list             # list local brain dirs
pbrainctl show <brain_id>  # dump a brain's manifest
pbrainctl orphans          # dry-run recovery sweep
pbrainctl force-merge      # one ReapOnce  [profile/vault]
pbrainctl force-checkpoint # one SynthesizeOne  [profile/vault]
pbrainctl version
```

## MCP tools

| Tool | Purpose |
|---|---|
| `brain_perceive` | Ingest a gathered web source (URL + content). Single item or batch via `items[]` (up to 100) |
| `brain_learn` | Ingest a curated document. Single item or batch via `items[]` (up to 100) |
| `brain_attach` | Ingest a binary file (PDF, Word, image). Stores raw binary in `Raw/attachments/`, extracts text, queues for synthesis |
| `brain_recall` | Hybrid FTS5 + vector search; optional `topic` filter |
| `brain_trace` | Query the synthesis audit trail (`_log.md`) by text, reliability, or date |
| `task_start` / `task_update` / `task_complete` / `task_get` | Working-memory task lifecycle, auto-seeded from vault context |
| `brain_status` *(agent mode)* | Manifest + heartbeat age + ship-queue depth as JSON |
| `brain_checkpoint` *(agent mode)* | Run the checkpoint flow; honors the v4.4 mtime-cutoff predicate unless `force=true` |
| `brain_death` *(agent mode)* | Transition this brain to dead and pack the death payload into the local ship queue |

## The Gate

The Gate evaluates each gathered source before it enters the wiki. It never throws — any failure degrades to a `medium` fallback.

- `reliability` — `high | medium | low | contested`
- `category` — failure type when reliability is low/contested: `source | formal | informal | philosophical`
- `topic` — `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

Curated sources (`brain_learn`) skip the LLM — human curation is the quality signal. The `topic` field is stored in summary frontmatter and used by `brain_recall` for scoped retrieval.

## Layouts

### Agent vault (per process, v5.0)

```
$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/brains/<brain_id>/
├── manifest.json          # identity, parentage, lifecycle status
├── vault/
│   ├── Wiki/{summaries,entities}/
│   ├── Raw/{curated,gathered,attachments}/
│   └── _log.md            # append-only synthesis audit trail
├── _index/                # vectors.db (FTS5 + sqlite-vec) + provenance.json
└── markers/alive          # flock-held heartbeat marker
```

### Daemon collective (per `(profile, vault)`, on the daemon host)

```
$PHANTOM_BRAIN_DATA_DIR/{profile}/{vault}/collective/
├── vault/                 # canonical Wiki + Raw + queue
├── _index/                # vectors.db + provenance.json + .gen-counter
├── _published/            # snapshot-<gen>.tar.zst tarballs + sidecars
├── brains/                # _uploads, _staging, _pending, _merged
└── ledger/merges.sqlite   # per-vault merge audit log
```

## Setup

**Prerequisites:** Go 1.26+ for build; the `claude` CLI on `$PATH` for the gate / summarizer; optionally [Ollama](https://ollama.ai) with `nomic-embed-text` for vector search.

```bash
git clone https://github.com/neverprepared/mcp-phantom-brain
cd mcp-phantom-brain
make build           # produces ./pbrainctl
```

### MCP server (agent)

Two startup modes, selected automatically by environment.

**Legacy mode** (drop-in replacement for the TS server, reads the same on-disk vault):

```json
{
  "phantom-brain": {
    "command": "/path/to/mcp-phantom-brain/pbrainctl",
    "args": ["mcp"],
    "env": { "BRAIN_VAULT_PATH": "/path/to/your/vault" }
  }
}
```

**Agent-contract mode** (v5.0 mortal brains + daemon ship):

```json
{
  "phantom-brain": {
    "command": "/path/to/mcp-phantom-brain/pbrainctl",
    "args": ["mcp"],
    "env": {
      "CL_BRAIN_API": "https://your-daemon",
      "CL_BRAIN_API_TOKEN": "pb_personal_memory_…",
      "CL_WORKSPACE_PROFILE": "personal",
      "CL_BRAIN_VAULT": "memory"
    }
  }
}
```

> **Gotcha:** do not use nested shell fallback syntax (`${VAR:-${OTHER}}`) in the MCP env block — Claude Code partially expands it, leaving a trailing `}`. Use plain `${VAR}` references only.

### Daemon

```bash
pbrainctl serve
```

Reads `$PHANTOM_BRAIN_CONFIG_DIR` (default `~/.config/phantom-brain-server`) for `server.toml` + per-vault `config.toml` + `auth.toml`. State lives under `$PHANTOM_BRAIN_DATA_DIR` (default `/var/lib/phantom-brain`). Acquires an exclusive flock so a second daemon refuses to start.

Container build: `docker build -t pbrainctl -f docker/Dockerfile .` (currently macOS-arm64 only until the Linux `sqlite-vec` binary is vendored).

## Configuration

### Agent (per-process env)

| Var | Purpose |
|---|---|
| `BRAIN_VAULT_PATH` *(legacy mode)* | Absolute path to the on-disk vault |
| `CL_BRAIN_API` *(agent mode)* | Daemon base URL |
| `CL_BRAIN_API_TOKEN` *(agent mode)* | Bearer token; daemon resolves to (profile, vault) |
| `CL_WORKSPACE_PROFILE` / `CL_BRAIN_VAULT` *(agent mode)* | Belt-and-suspenders match against the token |
| `CL_BRAIN_ID` *(agent mode)* | Optional — rebind to an existing brain dir |
| `OLLAMA_BASE_URL`, `EMBEDDING_MODEL`, `EMBEDDING_DIMS` | Vector search |

### Daemon (server.toml)

`server.port`, `server.host`, `defaults.{retention_gens,reaper_poll_interval_secs,…}`, `storage.backend` (`local` default; `minio` enabled — see below). Full v4.4 §4 schema in [`internal/server/config.go`](./internal/server/config.go).

#### MinIO / S3 backend

When `[storage] backend = "minio"`, brain uploads go directly to S3 via presigned PUT URLs (daemon never sees the bytes). On `/merge/complete` the daemon downloads the object into local `brains/_pending/<brain_id>.tar` so the reaper picks it up unchanged.

```toml
[storage]
backend = "minio"
minio_endpoint   = "minio.example.com:9000"
minio_bucket     = "phantom-brain"
minio_access_key = "AKIA..."
minio_secret_key = "secret..."
minio_use_ssl    = true
```

Bucket layout: `<profile>/<vault>/_uploads/<upload_id>.tar`. Daemon deletes the upload key after a successful merge; configure a bucket lifecycle rule to expire orphaned `_uploads/*` after the upload TTL (default 1 hour) as a backstop.

## Development

```bash
make build      # produces ./pbrainctl
make test       # full suite (vet + unit + integration)
go test -tags=sqlite_fts5 -race ./internal/brain/... ./internal/server/...
```

CI is in `.github/workflows/go.yml` (currently macos-14 only until the Linux dylib lands). Deferred items: Linux `sqlite-vec` vendoring, MinIO backend, automated `brain_checkpoint` cadence, prompt-file extraction, rate limiting.

## License

MIT
