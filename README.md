# phantom-brain

A [Model Context Protocol](https://modelcontextprotocol.io) server that gives Claude a structured, validated long-term memory backed by a **Postgres** System of Record, an **OpenSearch** search projection, and **MinIO** for attachments.

Content enters through a **Raw → Gate → distill** pipeline. Sources are validated for reliability and classified by subject-matter topic before they become durable memory. The host LLM drives the pipeline; the server enforces structure and provides the reference material.

> **Implementation note.** The codebase is the Go rewrite (v3.9.x). A single `pbrainctl` binary subsumes the agent-side MCP server, the HTTP daemon, and the operator CLI. The original TypeScript implementation lives under [`legacy-ts/`](./legacy-ts/), frozen. The OpenSearch→Postgres cutover (epic #92) is complete — the old snapshot/sqlite-vec read model is gone; see [`CLAUDE.md`](./CLAUDE.md) and [`docs/audit-2026-06-27.md`](./docs/audit-2026-06-27.md).

## How it works

Two processes:

- **Daemon** (`pbrainctl server serve`) — the canonical store. Records land in **Postgres** (the per-profile `pb_<profile>` System of Record, pgvector embeddings); a transactional outbox + River worker project each record into the **OpenSearch `pb_records`** search index; attachment blobs go to **MinIO**. An async synth worker runs the Gate + distill (via the `claude` CLI) and a continuous sweeper keeps the backlog drained.
- **Agent** (`pbrainctl client mcp`) — a per-session stdio MCP server. Every read and write goes to the daemon over HTTP. `brain_recall` / `brain_fetch` are **online** (always fresh, no local cache). Writes pass through a per-binding write-ahead queue so they survive a daemon outage and drain on reconnect.

The lifecycle:

1. **Ingest** — `brain_perceive` (web content), `brain_learn` (curated docs), or `brain_attach` (binary files: PDF, Word, images) POSTs the daemon, which writes a Postgres record (`synthesised=false`) and enqueues a projection job.
2. **Gate** — the synth worker scores the source for reliability (`high | medium | low | contested`) and classifies the subject-matter topic (`agents | memory | governance | tools | …`) via a `claude` CLI call.
3. **Synthesize** — raw content is distilled into a prose body, named entities are extracted, the record is marked synthesised, and `pb_records` is re-projected with the distilled body.
4. **Recall** — `brain_recall` embeds the query locally, POSTs the daemon, and gets hybrid BM25 + kNN hits over `pb_records`, with optional topic/kind/reliability filters. `brain_fetch` returns a full body by SHA from the SoR.

## Binary

v3.0 groups every command under `client` (workstation / agent side) or `server` (daemon host). The old flat names were removed; there are no aliases.

```
# client (agent / workstation)
pbrainctl client mcp                 # stdio JSON-RPC MCP server (per agent process)
pbrainctl client ingest-bulk         # bulk loader for an Obsidian-shaped tree
pbrainctl client migrate-legacy      # one-time port of an old on-disk vault
pbrainctl client brain list|show|orphans
pbrainctl client gc-brains           # garbage-collect dead local brain dirs
pbrainctl client queue list|drain-now|clear   # write-ahead queue inspection (--dead for dead-lettered rows)
pbrainctl client reflect|forget|resynth       # maintenance: report / delete / re-synthesize (Postgres SoR)
pbrainctl client version

# server (daemon host)
pbrainctl server serve               # HTTP daemon (multi-vault; Postgres SoR + pb_records projection + MinIO)
pbrainctl server config validate     # dry-run the registry load before reload
pbrainctl server vault               # list | status | reload (SIGHUPs the daemon)
pbrainctl server db provision|migrate <profile>   # create / migrate the per-profile pb_<profile> database
pbrainctl server backfill-to-pg <profile>         # one-shot legacy-OS → Postgres SoR backfill
pbrainctl server queue               # depth | contributors  [profile/vault]
pbrainctl server maintenance         # enter | exit          [profile/vault]
pbrainctl server backfill-attachment-stubs
pbrainctl server bucket create|list               # MinIO bucket admin
pbrainctl server binding create|list|delete       # single-command binding workflow
pbrainctl server version
```

## MCP tools

| Tool | Purpose |
|---|---|
| `brain_perceive` | Ingest a gathered web source (URL + content). Single item or batch via `items[]` (up to 100) |
| `brain_learn` | Ingest a curated document. Single item or batch via `items[]` (up to 100) |
| `brain_attach` | Ingest a binary file (PDF, Word, image). Stores the blob in MinIO, queues the record for synthesis |
| `brain_recall` | Online hybrid BM25 + kNN search over `pb_records` (always fresh); optional `topic` / kind / reliability filters |
| `brain_fetch` | Return one record's full body by SHA, online from the Postgres SoR |
| `brain_trace` | Read the local synthesis audit trail (`Wiki/_log.md`) |
| `brain_reflect` / `brain_forget` / `brain_resynth` | Maintenance: report forget-candidates, delete one record (SoR + projection), re-synthesize the backlog |
| `task_start` / `task_update` / `task_complete` / `task_get` | Working-memory task lifecycle, auto-seeded from recall context |
| `brain_status` *(agent mode)* | Manifest + heartbeat age + connectivity + queued-writes depth as JSON |
| `brain_checkpoint` *(agent mode)* | Run the working-memory checkpoint flow unless `force=true` |
| `brain_death` *(agent mode)* | Transition this brain to dead (status + log marker; no payload) |

## The Gate

The Gate evaluates each gathered source before it enters the wiki. It never throws — any failure degrades to a `medium` fallback.

- `reliability` — `high | medium | low | contested`
- `category` — failure type when reliability is low/contested: `source | formal | informal | philosophical`
- `topic` — `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

Curated sources (`brain_learn`) skip the LLM — human curation is the quality signal. The `topic` field is stored in summary frontmatter and used by `brain_recall` for scoped retrieval.

## Layouts

The canonical store is Postgres + OpenSearch + MinIO, not a disk tree. Only a little local state remains.

### Agent-side (per binding, shared across brain instances)

```
$XDG_DATA_HOME/phantom-brain/{profile}/{vault}/
├── wqueue.sqlite               # write-ahead queue for offline writes (per binding)
├── wqueue-attach/<sha><ext>    # staged attachment bytes
└── brains/<brain_id>/
    ├── manifest.json           # identity + lifecycle status (births are greenfield — no snapshot parentage)
    ├── markers/<brain_id>      # heartbeat sentinel
    └── _index/
        └── wm-<pid>.sqlite     # per-process working (active) memory
```

There is no `vectors.db` read cache and no snapshot tarball — recall/fetch are online.

### Daemon-side (per `(profile, vault)`, on the daemon host)

```
$PHANTOM_BRAIN_DATA_DIR/{profile}/{vault}/
└── locks/maintenance.flag      # present = pause writes
```

Records live in Postgres (`pb_<profile>`), the search projection in OpenSearch (`pb_records`), and attachments in MinIO.

## Setup

**Prerequisites:** the `claude` CLI on `$PATH` for the gate / summarizer; optionally [Ollama](https://ollama.ai) with `nomic-embed-text` for vector search.

### Install

**Homebrew tap (recommended) — macOS arm64 + Linux amd64:**

```bash
brew install neverprepared/tap/pbrainctl
```

**Linux — release tarball (no Homebrew):**

```bash
curl -L -o pb.tar.gz https://github.com/neverprepared/phantom-brain/releases/latest/download/pbrainctl_linux_amd64.tar.gz
tar -xzf pb.tar.gz                          # extracts pbrainctl at the archive root
sudo install pbrainctl /usr/local/bin/
pbrainctl client version
```

**Any platform — build from source:**

```bash
git clone https://github.com/neverprepared/phantom-brain
cd phantom-brain
make build           # produces ./pbrainctl
```

### MCP server (agent)

Two startup modes, selected automatically by environment.

**Legacy mode** (drop-in replacement for the TS server, reads the same on-disk vault):

```json
{
  "phantom-brain": {
    "command": "/path/to/phantom-brain/pbrainctl",
    "args": ["client", "mcp"],
    "env": { "BRAIN_VAULT_PATH": "/path/to/your/vault" }
  }
}
```

**Agent mode** (online recall/fetch against the daemon; offline writes via the local wqueue):

```json
{
  "phantom-brain": {
    "command": "/path/to/phantom-brain/pbrainctl",
    "args": ["client", "mcp"],
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
pbrainctl server serve
```

Reads `$PHANTOM_BRAIN_CONFIG_DIR` (default `~/.config/phantom-brain-server`) for `server.toml` + per-vault `config.toml` + `auth.toml`, plus a base Postgres DSN (`PB_POSTGRES_DSN` or the `server.toml` field). Each profile's database must be provisioned first: `pbrainctl server db provision <profile>`. State lives under `$PHANTOM_BRAIN_DATA_DIR` (default `/var/lib/phantom-brain`). Acquires an exclusive flock so a second daemon refuses to start.

Container build: `docker build -t pbrainctl -f docker/Dockerfile .`. The Linux `sqlite-vec` shared library is vendored, so Linux amd64 builds work the same as macOS arm64.

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

`server.port`, `server.host`, the base Postgres DSN, `[opensearch]`, and `[storage]` (MinIO). Full schema in [`internal/server/config.go`](./internal/server/config.go).

#### MinIO / S3 (attachment blobs)

MinIO stores attachment bytes written by `brain_attach` at `<profile>/<vault>/attachments/<sha><ext>`; metadata lives on the Postgres record. `[storage] minio_bucket` is the required global fallback bucket; a binding can route to its own bucket via `config.toml [storage_overrides]`. One MinIO credential serves every bucket — give it a wildcard policy.

```toml
[storage]
minio_endpoint   = "minio.example.com:9000"
minio_bucket     = "phantom-brain"   # required fallback default
minio_access_key = "AKIA..."
minio_secret_key = "secret..."
minio_use_ssl    = true
```

Per-profile isolation (`config.toml`): `bucket = "<profile>-archives"` for a dedicated MinIO bucket and `index_prefix = "<profile>_"` for a dedicated `pb_records` index; the Postgres SoR is already isolated per-profile by database (`pb_<profile>`).

## Development

```bash
make build      # produces ./pbrainctl
make test       # full suite (vet + unit + integration)
go test -tags=sqlite_fts5 -race ./internal/brain/... ./internal/server/...
```

CI is in `.github/workflows/go.yml` and runs the full matrix on **macos-14** (Apple Silicon) and **ubuntu-latest** (x86_64) — the `sqlite-vec` shared library is vendored for both. Integration tests that need Postgres / OpenSearch / MinIO are skipped unless those services are available.

## License

MIT
