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

# vet + test + build in one shot before pushing
make all
```

Plain `go build`/`go test` won't work — `internal/sqlite` panics at init() (it registers the sqlite-vec driver) without the `sqlite_fts5` build tag. Always use `make` or `GOFLAGS=-tags=sqlite_fts5 go test ./...`.

> **Cutover status (v3.9.x).** The OpenSearch → Postgres cutover (epic #92: D1/D2a/D2b) is **complete**. The System of Record is now per-profile Postgres; OpenSearch is a single search projection (`pb_records`); the legacy `pb_summaries`/`pb_entities`/`pb_attachments` indices, the snapshot tarball machinery, and the local sqlite-vec read cache are all deleted. Reads are online (recall/fetch POST the daemon). The follow-on audit fix pass (sets A/C/D) is recorded in [`docs/audit-2026-06-27.md`](./docs/audit-2026-06-27.md); the cutover plan is [`docs/design/daemon-cutover-plan.md`](./docs/design/daemon-cutover-plan.md).

### Subcommand layout (v3.0)

`pbrainctl` groups every command under one of two parents — `client` (workstation / agent side) or `server` (daemon host). The old flat names (`pbrainctl serve`, `pbrainctl mcp`, `pbrainctl vault ...`) were removed in v3.0; there are no aliases. Quick reference:

```
pbrainctl client mcp                 # stdio JSON-RPC MCP server (per agent process)
pbrainctl client ingest-bulk         # bulk loader for an Obsidian-shaped tree
pbrainctl client migrate-legacy      # one-time v4.x → v5.0 brain dir migration
pbrainctl client brain list|show|orphans
pbrainctl client gc-brains           # local brain dir GC; bindless walk if no scope set
pbrainctl client queue list|drain-now|clear   # v3.1 write-ahead queue inspection
pbrainctl client reflect             # forget-candidate report — scans the Postgres SoR (issue #72 Phase 1)
pbrainctl client forget <sha>        # delete one record by SHA from the SoR (+ projection delete)
pbrainctl client resynth [--apply]   # re-synthesize the Synthesised=false backlog (issue #82)
pbrainctl client version

pbrainctl server serve               # HTTP daemon (per-(profile, vault) synth; Postgres SoR + pb_records projection)
pbrainctl server config validate [profile/vault]  # dry-run the startup registry load before restart (#70)
pbrainctl server vault list|status|reload
pbrainctl server db provision <profile>       # create/migrate the per-profile pb_<profile> Postgres DB
pbrainctl server db migrate <profile>         # run pending SoR migrations for a profile
pbrainctl server queue depth|contributors     # synth backlog inspection (drained from the PG SoR)
pbrainctl server maintenance enter|exit
pbrainctl server backfill-attachment-stubs
pbrainctl server backfill-to-pg <profile>     # one-shot legacy-OS → Postgres SoR backfill (#92)
pbrainctl server bucket create|list           # MinIO bucket admin
pbrainctl server binding create|list|delete   # single-command binding workflow
pbrainctl server version
```

## Architecture

phantom-brain is a **Model Context Protocol server** that gives Claude Code (and any other MCP-compatible agent) long-term durable memory plus per-session active memory. The implementation is Go, the runtime split is two-process:

- **`pbrainctl client mcp`** — per-agent stdio MCP server. Talks to the daemon over HTTP for every read and write. Holds local SQLite only for active (working) memory + a per-binding write-ahead queue for offline writes. Recall and fetch are **online** (POST the daemon); there is no local read cache. Spawned by Claude Code per session.
- **`pbrainctl server serve`** — single HTTP daemon. The canonical store. Receives writes, persists records to **Postgres** (the System of Record) + attachment blobs to **MinIO**, projects each record into the **OpenSearch `pb_records`** search index, and runs async synth (gate + distill via the `claude` CLI). Serves online recall/fetch straight from the SoR + projection.

The canonical content store is **Postgres** (per-profile `pb_<profile>` database, pgvector embeddings); OpenSearch holds the single `pb_records` projection used for hybrid BM25+kNN search; MinIO holds attachment blobs. The local-filesystem "vault" still exists for legacy bulk-migration paths and tests, but agent reads + writes target the daemon, not disk.

### Storage tiers (memory model)

| Tier | What it is | Where in code |
|---|---|---|
| **Long-term memory** | **System of Record = Postgres** — per-profile `pb_<profile>` database (tables `records` / `entities` / `facts`, pgvector embeddings). **Search projection = OpenSearch `pb_records`** (ONE index, hybrid BM25+kNN). **MinIO** for attachment blobs. Canonical, durable, shared across all agents bound to the same vault. v3.2+: the projection index prefix and MinIO bucket MAY be per-binding via `[storage_overrides]` (see "Per-binding storage overrides" below); the SoR is isolated per-profile by database. | `internal/pgstore/` (SoR) + `internal/osproject/` (projection) + `internal/projection/` (outbox/River) + the daemon HTTP surface in `internal/server/` |
| **Active memory** | Per-process SQLite at `_index/wm-<pid>.sqlite`. Tasks, findings, artifacts, open questions. Lives only for the agent process; dropped at exit. | `internal/working/` |
| **Write-ahead queue** (v3.1) | Per-binding SQLite at `<VaultBaseDir>/wqueue.sqlite` + attachment staging dir. Catches writes when the daemon is unreachable; drainer goroutine retries with exponential backoff, classifies transient vs permanent failures, and dead-letters poison rows. Persists across MCP child deaths. | `internal/brain/wqueue/` + `internal/brain/drainer.go` |

There is **no local read cache** anymore — recall and fetch go to the daemon every time (the sqlite-vec `vectors.db` snapshot and `internal/index` were deleted in the #92 cutover). If the daemon is unreachable, recall/fetch error rather than serve stale data.

Promotion path: `task_complete` aggregates important findings into a single markdown note and POSTs it as a `brain_learn` to the daemon → lands in long-term as a `task_summary` doc.

Write path (v3.1): every write tool calls `wqueue.Enqueue` first, then attempts the daemon POST inline. Success → `Delete` the row, return clean result. Failure (network error, 5xx, timeout) → leave the row, append a queued-notice to the tool's text result, retry from the drainer. Caller never sees a write failure. Daemon SHA-dedups so retries are always safe.

### Entry points

| Path | Role |
|---|---|
| `cmd/pbrainctl/main.go` | CLI entry. Routes to `clientCmd` or `serverCmd` parents (v3.0 restructure). |
| `internal/server/server.go::Start()` | Daemon lifecycle: load config, build per-binding Postgres pools + River projection clients + OS projection (`pb_records`) + MinIO backend, spawn `SynthWorker` (with its durability sweeper), mount chi router. |
| `internal/mcp/server.go::Register()` | MCP tool registration. Mounts every `brain_*` and `task_*` tool against an `mcp-go` server. |
| `internal/brain/lifecycle.go::Start()` | Per-agent birth: claim a brain dir (always **greenfield** — no snapshot pull), open `wqueue`, start heartbeat + drainer goroutine. |
| `internal/brain/wqueue/wqueue.go` | Per-binding SQLite write-ahead queue. `OpenOrCreate` (agent) vs `OpenReadOnly` (CLI inspection). `UNIQUE(kind, sha)` + `claimed_at` TTL prevents concurrent-drainer races. `MarkDead` + `ListDead` back the dead-letter path. |
| `internal/brain/drainer.go` | 30s-tick goroutine spawned by Lifecycle. Drains eligible queue rows, dead-letters permanent failures (or rows past `MaxAttempts`), sweeps orphaned staging files. (No snapshot metadata to refresh anymore.) |
| `internal/brain/connectivity.go` | Per-Lifecycle state machine: `offline` (no successful contact this session) → `degraded` (had success, last attempt failed) → `online` (last attempt succeeded). Flips on first successful daemon contact, not on empty queue. |
| `internal/mcp/wqueue_helper.go` | Bridges MCP write tools to `wqueue.Enqueue` + appends queued-notice when daemon POST fails. |

### MCP tools (the public surface)

| Tool | File | Purpose | Writes to |
|---|---|---|---|
| `brain_perceive` | `internal/mcp/ingest.go` | Ingest gathered web content. Kind: `web_scrape`. | Long-term (daemon → OS) |
| `brain_learn` | `internal/mcp/learn.go` | Ingest a curated note. Kind: `note`. Skips LLM gate (defaults to medium reliability). | Long-term |
| `brain_attach` | `internal/mcp/attach.go` | Ingest a binary file. Kind: `attachment_stub`. Bytes → MinIO; metadata → OS. | Long-term + MinIO |
| `brain_recall` | `internal/mcp/recall.go` | **Online-only.** Embeds the query locally, POSTs the vector to the daemon → hybrid BM25 + kNN over `pb_records`. Always fresh; no local snapshot fallback (daemon down ⇒ clear tool error). Result hits include title, kind indicator (`[note]` / `[attachment pdf]` / etc.), 150-char snippet, and a fetch hint for attachments. | Reads only (online) |
| `brain_fetch` | `internal/mcp/fetch.go` | **Online-only.** POSTs the daemon → returns one doc's **full** (untruncated) body straight from the Postgres SoR by SHA. Any SHA recall surfaced is fetchable and fresh. Use deliberately — recall to find, fetch to read. | Reads only (online) |
| `brain_trace` | `internal/mcp/trace.go` | Read the local Wiki/_log.md audit trail. | Reads only |
| `brain_checkpoint` | `internal/mcp/brain_checkpoint.go` | Force a checkpoint of the working-memory state. | Local working DB |
| `brain_status` | `internal/mcp/brain_status.go` | Report brain state (manifest, heartbeat age) + v3.1 connectivity (`online`/`degraded`/`offline`), `queued_writes` depth, `last_daemon_contact_secs`. (No snapshot fields — snapshots are gone.) | Reads only |
| `brain_death` | `internal/mcp/brain_death.go` | Flip brain status to dead. No payload tarball, just status + log marker. | Local manifest |
| `brain_reflect` | `internal/mcp/reflect.go` | Maintenance cycle (issue #72 Phase 1). Read-only report of forget-candidate SHAs. Detector: stale-gate — scans the Postgres SoR via `ListUnsynthesised` (`synthesised == false`). Propose-then-apply: review, then `brain_forget` approved SHAs. | Reads only |
| `brain_forget` | `internal/mcp/forget.go` | Delete one record by SHA (the apply step). Daemon deletes from the Postgres SoR **and** enqueues a projection delete in the same tx, so the record leaves `pb_records` too — an honest "forgotten". | Long-term (delete) |
| `brain_resynth` | `internal/mcp/resynth.go` | Re-synthesis backfill (issue #82). Re-processes `Synthesised=false` records (the backlog) through the gate/distill pipeline. `dry_run` defaults true (report backlog + sample); apply spawns a background backfill that's NON-lossy (bypasses the lossy fast-path `Enqueue`, serialized with the live worker via `processMu`). The fix-it companion to `brain_reflect` — re-synthesize (keep), vs `brain_forget` (delete). The continuous synth sweeper also drains this backlog automatically. | Long-term (re-synth) |
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
  ├─ Ollama.Embed(title + "\n\n" + body) → 768-dim vector  (carried in the request)
  └─ internal/mcp/wqueue_helper.go::EnqueueAndAttempt   ← v3.1 intercept
       ├─ wqueue.Enqueue → row written to local SQLite + (attach only) bytes staged
       ├─ brain.Client.Perceive(req)        ← agent-side HTTP, inline attempt
       │   ├─ success → wqueue.Delete, return clean tool result
       │   └─ failure (ErrDaemonUnreachable / 5xx / timeout) → row stays, append queued-notice
       │         (drainer goroutine retries every 30s with exp backoff, dead-letters permanent failures)
       ↓ (success path continues to daemon)
internal/server/handlers_write.go::handlePerceive
  ├─ validate SHA, title/body, Kind enum
  ├─ writeRecordRaw → Postgres SoR record (synthesised=false, carrying the agent embedding)
  │                   + EnqueueProjectTx (River outbox) in the SAME tx → projects to pb_records
  └─ SynthWorker.Enqueue(profile, vault, sha)   ← best-effort in-memory fast-path trigger
       ↓ (async, on daemon goroutine)
internal/server/synth_queue.go::processJob
  ├─ GetRecordBySHA (Postgres SoR)
  ├─ CheckCoherence (heuristic)
  ├─ RunGate via `claude` CLI → reliability/topic/category JSON
  ├─ SummarizeContent via `claude` CLI → distilled 3-5 paragraph body
  ├─ ExtractEntities (heuristic on raw body, denylist filtered) → write entities/facts
  ├─ MarkRecordSynthesised (distilled body, reliability, topic, embedding)
  └─ re-enqueue projection → re-projects pb_records with the distilled body
```

Synth durability: the `SynthWorker.Enqueue` fast-path is best-effort (overflow-drop is fine). A **continuous background sweeper** (`runSweeper`, ~30s) drains each binding's `synthesised=false` backlog straight from the Postgres SoR — the SoR record *is* the queue — so a dropped or daemon-restarted trigger only delays synth to the next sweep; jobs are never silently lost. There is no snapshot rebuild and no gen-counter; recall reads `pb_records` live, so a re-projection is visible immediately.

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
  locks/
    maintenance.flag        ← present = pause writes
```

The daemon's canonical state now lives in Postgres (`pb_<profile>`), the `pb_records` OpenSearch index, and MinIO — not on this disk tree. There is no `_index/`, `.gen-counter`, or `_published/` snapshot directory anymore.

Agent-side (per binding, shared across brain instances):

```
$XDG_DATA_HOME/phantom-brain/<profile>/<vault>/
  wqueue.sqlite             ← v3.1 write-ahead queue (per binding)
  wqueue-attach/<sha><ext>  ← v3.1 staged attachment bytes
  brains/<brain_id>/
    manifest.json           ← alive | shutting_down | dead, heartbeat (no parent_gen / parent_snapshot_* — births are greenfield)
    markers/<brain_id>      ← heartbeat sentinel
    _index/
      wm-<pid>.sqlite       ← per-process working (active) memory
```

There is no `vectors.db` read cache and no `_snapshot-cache/` — recall/fetch are online. `wqueue.sqlite` survives brain-dir destruction — next agent for the same binding picks up any pending writes.

### Config layout

```
$PHANTOM_BRAIN_CONFIG_DIR/
  server.toml                                      ← [server] [storage] [opensearch] [defaults]
  profiles/<profile>/vaults/<vault>/
    auth.toml                                      ← bearer_token = "..."
    config.toml                                    ← (optional) per-vault VaultOverrides
```

Bearer tokens live in `auth.toml`, ONE per vault binding. The daemon's registry walks `profiles/*/vaults/*/auth.toml` at startup + on SIGHUP. `server.toml` also carries the base Postgres DSN (overridable by `PB_POSTGRES_DSN`); each profile's `pb_<profile>` database must be created first with `pbrainctl server db provision <profile>`.

Adding a binding for a new profile, end to end:

```
pbrainctl server binding create <profile>/<vault> --bucket <profile>-archives --create-bucket --index-prefix <profile>_
pbrainctl server db provision <profile>
pbrainctl server vault reload
```

`config.toml` accepts an optional `[storage_overrides]` block (v3.2+) that re-routes a single binding to its own OS index prefix and/or MinIO bucket while the daemon keeps ONE OpenSearch connection pool and ONE MinIO credential. Example:

```toml
# profiles/client_x/vaults/main/config.toml
retention_gens = 10

[storage_overrides]
index_prefix = "client_x_"   # APPENDED to daemon-global cfg.OpenSearch.IndexPrefix
bucket       = "pb-client-x" # REPLACES cfg.Storage.MinIOBucket for this binding
```

Allowed characters in `index_prefix`: lowercase ASCII letters, digits, underscore. Bucket must exist before daemon start — the daemon refuses to create it. In v3.3+ the single-command operator workflow does this for you:

```
pbrainctl server binding create client_x/main \
    --index-prefix client_x_ --bucket pb-client-x --create-bucket
```

The command writes `auth.toml` (mode 0o600) + `config.toml` with the override block, prints the generated bearer token once, and (with `--create-bucket`) calls MakeBucket inline. See "Per-binding storage overrides (v3.2)" below for the full contract.

**Run `binding create` on the storage box host, not inside the daemon container.** The daemon container bind-mounts `/config` read-only (it reads config, never writes), so `binding create` will fail with EROFS there (it returns an actionable hint, not the raw syscall error — issue #69). Install `pbrainctl` on the host (brew or scp) and point the subcommand at the bind-mount source: `pbrainctl server binding create … --config-dir <host-path>`. This avoids hand-editing the TOML (the typo/duplicate-token/silent-drift failure mode). On a workstation you can write to any local `--config-dir` and copy the resulting `profiles/<profile>/vaults/<vault>/` subtree into the daemon's config root.

### The Gate (`internal/server/gate.go`) + synth LLM backend (`internal/server/llm.go`)

`RunGate()` is the daemon-side LLM call. As of the pluggable-backend change it routes through an `LLMBackend` (`internal/server/llm.go`) rather than shelling out directly, so synth (gate verdict + distill + entity extraction) runs on one of two backends, **selected by the `[synth]` block in `server.toml`**:

- **`backend = "ollama"` (DEFAULT)** — a local Ollama model via `POST /api/generate` (`internal/ollama.Client.Generate`). Zero Claude tokens. Model defaults to `qwen2.5:7b` (`ollama.DefaultGenModel`) and **must be pulled locally** (`ollama pull qwen2.5:7b`). The gate call pins Ollama's `format:"json"` so small models emit parseable verdicts; distill runs free-form. Health is probed lazily and the first success is cached, so a daemon that starts before Ollama self-heals on the next synth job (no restart needed).
- **`backend = "claude"`** — the bundled `claude` CLI (`CallClaudeCLI`, in the Docker image since v2.2.0), authenticated via `CLAUDE_CODE_OAUTH_TOKEN` (Claude Max subscription credentials, NOT `ANTHROPIC_API_KEY`). Higher-quality verdicts/summaries; costs tokens.

`NewLLMBackend(cfg.Synth)` builds the backend; an unknown name falls through to Ollama. Tests use `SynthWorkerOpts.DisableCLI` to drop the backend to nil (no LLM); when `LLM` is nil but `DisableCLI` is false the worker defaults to the Claude CLI backend for back-compat.

Verdict fields (matching `osearch.SummaryDoc`):
- `reliability` — `high | medium | low | contested`
- `category` — `source | formal | informal | philosophical` (required when reliability is low/contested)
- `topic` — closed set: `agents | memory | governance | tools | training | infrastructure | knowledge | multiagent | general`
- `reason` — one-sentence explanation

Curated sources (`brain_learn`) skip the LLM and get a fixed `medium` verdict (curation is the quality signal).

`SummarizeContent()` is the distill pass — same backend, different prompt (free-form prose). Produces the `body` field; `raw_body` keeps the original.

If the selected backend is unavailable (Ollama unreachable / model not pulled; or, for `claude`, no OAuth token) the daemon still starts and gate + distill fall through to raw-content fallback (body == raw_body, reliability defaults to medium, topic defaults to general); entity extraction falls back to the regex extractor.

### Migration tooling (`pbrainctl ingest-bulk`)

One-shot bulk loader for either a legacy Obsidian vault (the migration we're doing now) OR any future on-disk format the operator wants to import.

```bash
pbrainctl client ingest-bulk <path> [--dry-run] [--concurrency N] [--max-file-bytes N] [--timeout-secs N]
```

Routes by directory:
- `Raw/curated/*.md` → `brain_learn` (Kind: note; email-shaped frontmatter → email_import)
- `Raw/gathered/*.md` → `brain_perceive` (Kind: web_scrape)
- `Raw/attachments/*` → `brain_attach` (Kind: attachment_stub)

Embeddings computed locally via Ollama. Idempotent — daemon dedups by SHA. `--dry-run` walks the tree and prints the plan without POSTing.

### Concurrency invariants

- **Daemon synth queue**: an in-memory channel + single worker per daemon for the low-latency fast path, **backed by a durable sweeper**. The fast-path `Enqueue` is best-effort (overflow-drop OK); `runSweeper` (~30s) drains each binding's `synthesised=false` backlog from the Postgres SoR, so a restart or a dropped trigger only delays synth — it never loses a job. The SoR record is the durable queue.
- **Agent write-ahead queue (v3.1)**: SQLite-backed, durable across MCP child deaths. `UNIQUE(kind, sha)` makes re-enqueue of the same SHA a no-op (benign). `claimed_at` + 5min TTL prevents two drainers from racing on the same row; on expiry an abandoned claim auto-releases. Permanent failures (4xx, malformed payload, unknown kind) or rows past `MaxAttempts` are dead-lettered, not retried forever. WAL mode + 5s busy_timeout for multi-process safety.
- **Projection**: record write + projection enqueue commit in ONE Postgres tx via the River outbox (`projection.WriteRecordAndEnqueue` / `EnqueueProjectTx`) — no dual-write race. The River worker projects to `pb_records` at-least-once.
- **Doc-ID separator**: SHA-based, format `<profile>:<vault>:<sha>`. Colon, not slash — opensearch-go interpolates IDs raw into URL paths and slashes silently 404.
- **Embedding zero check**: OS rejects all-zero vectors under cosine similarity. Either send nil (caller didn't compute) or a real embedding. The agent-computed embedding is persisted on the SoR record, so kNN recall works on freshly-written records.
- **Drainer cadence**: 30s poll. On each cycle: claim eligible rows, attempt POSTs, dead-letter permanent/exhausted rows, sweep orphan staging files. Backoff per row: exponential, base 30s, cap 5min, ±20% jitter.

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
| `CLAUDE_CODE_OAUTH_TOKEN` | — | Daemon: subscription credentials for `claude` CLI (only used when `[synth] backend = "claude"`) |
| `PB_SYNTH_BACKEND` | `ollama` | Daemon: synth LLM backend — `ollama` (default) or `claude`. Overrides `[synth] backend` in server.toml |
| `PB_SYNTH_OLLAMA_MODEL` | `qwen2.5:7b` | Daemon: Ollama generation model for synth (gate/distill/entities). Must be `ollama pull`'d on the Ollama host. Overrides `[synth] ollama_model` |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Ollama endpoint — shared by the agent embedding client AND the daemon synth backend. Overrides `[synth] ollama_base_url` |
| `PB_SYNTH_TIMEOUT_SECS` | `120` | Daemon: per-call ceiling (seconds) for each synth LLM call (gate verdict + distill), applied as a ceiling to both. Generous by default because local Ollama is slower than the Claude CLI and the first job after a restart pays model cold-load. Overrides `[synth] timeout_secs` |
| `PB_POSTGRES_DSN` | — | Daemon: base/maintenance Postgres DSN; `pgstore.DSNForProfile` derives the per-profile `pb_<profile>` DSN from it (overrides the `server.toml` field) |
| `BRAIN_VAULT_PATH` | — | Legacy: enables pre-v2 BRAIN_VAULT_PATH-only mode (no daemon contract) |

### Seed files

`internal/osproject` ensures the single `pb_records` projection index (+ its search pipeline) per binding at daemon startup; the per-profile Postgres SoR is created by `pbrainctl server db provision`. There are no "seed" content files anymore (v1's `src/seed/wiki/` is gone).

### Utility scripts

- `cmd/pbrainctl/ops.go` — operator subcommands: `vault list`, `snapshot status`, `maintenance enter/exit`, `brain list`, etc.
- `scripts/seed-vault-via-minio.sh` — bootstrap a remote MinIO with a seed tarball (rare).

## Per-binding storage overrides (v3.2)

A binding may carve out its own OS index set and/or MinIO bucket without standing up a separate daemon. One process, one OpenSearch connection pool, one MinIO credential — but per-(profile, vault) physical storage. Targets the multi-tenant case where one operator hosts several clients and wants a hard data boundary without N daemons.

### Resolution

`Registry.Load` builds `VaultBinding.Storage` once per binding:

```
IndexPrefix = cfg.OpenSearch.IndexPrefix + overrides.StorageOverrides.IndexPrefix
Bucket      = overrides.StorageOverrides.Bucket || cfg.Storage.MinIOBucket
```

Global prefix stays first so a dev/test sandbox prefix still wraps every binding the same way. Bucket is a straight replacement — MinIO credentials and endpoint are NOT overridable (Level 2 contract: one TCP pool, one credential, many buckets).

Write paths go through per-binding views (`internal/server/binding_views.go`):
- `*osBindingView` — a `*osearch.Client` clone bound via `WithPrefix(binding.Storage.IndexPrefix)`.
- `*minioBindingView` — the shared `*MinIOBackend` with `binding.Storage.Bucket` pinned.

Both are constructed once at startup (and rebuilt on SIGHUP reload) and cached on the Daemon by `VaultKey`. Handlers + the SynthWorker fetch the view via `Daemon.resolveOS` / `Daemon.resolveAttach` / `SynthWorker.resolveForJob` — there is NO per-request allocation beyond the cache lookup.

### Eager-ensure at startup

Before the HTTP listener opens, `Daemon.Start` walks every binding and (per binding) runs:
- `osproject.EnsureIndex` + `EnsureSearchPipeline` — idempotent create-if-missing for the single `pb_records` projection index (under the binding's resolved prefix) plus its hybrid-search pipeline. (The legacy `EnsurePrefixes` over `pb_summaries`/`pb_entities`/`pb_attachments` is gone — those indices are no longer ensured or written.)
- `MinIOBackend.EnsureBucketExists` — probes (no create) for each distinct bucket. Missing bucket = startup error (`mc mb` is operator action).
- per-profile Postgres: open the `pb_<profile>` pool, run the SoR + River migrations.

Failure here aborts startup. Better to refuse to serve than to 500 on every write to a misconfigured binding.

### Operator-footgun guard (retired in the #92 cutover)

The v3.2 `VerifyStorageOverrides` startup check guarded against an operator adding `[storage_overrides]` to a binding that already had data on the *shared* `pb_summaries` indices (the binding would silently stop seeing its own docs). Post-cutover those legacy indices are no longer ensured or written, and the per-binding `pb_records` projection is authoritative-from-birth — there is no shared-vs-prefixed straddle to guard against, so the check is **no longer wired into startup** (the function lingers in `storage_overrides_check.go` but is dead). Migrating an existing tenant's data is now `pbrainctl server backfill-to-pg <profile>`, not a shared-index reconciliation.

### Tenant-boundary safety

Cache miss on `resolveOS` / `resolveAttach` / `resolvePG` / `SynthWorker.resolveForJob` returns an error rather than silently falling back to the shared daemon-global infrastructure. Projecting a doc into the wrong tenant's indices is a worse failure than dropping the job; HTTP handlers return 500 ("binding configuration error") and SynthWorker skips the job — the durable sweeper re-picks it from the SoR once the binding view is registered. Tests + legacy single-binding daemons opt back into the shared fallback explicitly via `Daemon.allowSharedFallback`.

## Operator workflow: writing config in a read-only container

The recommended production hardening bind-mounts the daemon's `/config` **read-only** (the daemon reads config, never writes it). That means `pbrainctl server binding create` cannot run *inside* the container — it needs to write `auth.toml` + `config.toml` under the config dir and fails with EROFS (handled with an actionable hint, not a raw syscall error — issue #69). Use one of the paths below; never hand-edit TOML as a first resort (it caused the typo / duplicate-token / silent-drift incidents this workflow exists to prevent).

### Two viable paths

1. **`pbrainctl` on the storage-box host** (preferred). Brew-install or scp the binary onto the host that owns the bind-mount source, and run against that path:
   ```
   pbrainctl server binding create client_x/main \
       --config-dir /srv/phantom-brain/config \
       --index-prefix client_x_ --bucket pb-client-x --create-bucket
   ```
   The host path is the *source* of the container's read-only mount, so the daemon picks up the new binding on the next reload.

2. **Workstation scratch dir, then copy.** Generate the subtree anywhere writeable, then move it onto the host's mount source:
   ```
   pbrainctl server binding create client_x/main --config-dir /tmp/scratch ...
   scp -r /tmp/scratch/profiles/client_x dch-host:/srv/phantom-brain/config/profiles/
   ```

After either path, **validate before restarting** (issue #70):
```
pbrainctl server config validate --config-dir /srv/phantom-brain/config
```
then reload: SIGHUP via `pbrainctl server vault reload` (re-reads the registry, no downtime) is preferred over a full `docker compose restart pbrainctl` (a restart drops the in-memory synth fast-path queue — the durability sweeper re-drains the SoR backlog afterward, but the reload avoids the disruption entirely).

### Manual fallback recipe (last resort)

If neither path is convenient, write the files by hand — but honor every reminder here, because each maps to a real incident:

```bash
PROFILE=client_x VAULT=main
BDIR=/srv/phantom-brain/config/profiles/$PROFILE/vaults/$VAULT
mkdir -p "$BDIR"
TOKEN=$(openssl rand -hex 32)            # FRESH per binding — do NOT reuse a shell var across bindings
printf 'bearer_token = "%s"\n' "$TOKEN" > "$BDIR/auth.toml"
chmod 0600 "$BDIR/auth.toml"             # auth.toml holds a secret
# optional per-binding storage overrides:
printf '[storage_overrides]\nindex_prefix = "client_x_"\nbucket = "pb-client-x"\n' > "$BDIR/config.toml"
pbrainctl server config validate --config-dir /srv/phantom-brain/config   # catch mistakes BEFORE reload
```

- **Fresh token per binding.** Reusing one token across bindings trips the daemon's duplicate-bearer-token guard at startup → crash-loop.
- **`chmod 0600 auth.toml`.** It's a bearer secret; `binding create` sets this for you, a manual write does not.
- **SIGHUP, not restart.** `pbrainctl server vault reload` reloads the registry without dropping the synth queue.

### Startup-failure recovery flowchart

When the daemon crash-loops after a config change (it refuses to serve **any** binding, including unaffected ones), `config validate` should have caught it first. If it slipped through:

| Symptom in `docker compose logs pbrainctl --tail 20` | Cause | Fix |
|---|---|---|
| `parse … config.toml` / TOML decode error | syntax typo | fix the TOML; `config validate` before reload |
| `duplicate bearer_token across vaults; conflict at …` | reused token | grep every `auth.toml`, regenerate one |
| Postgres connect / migrate failure for a profile | `pb_<profile>` not provisioned or DSN wrong | `pbrainctl server db provision <profile>`; check the base DSN |
| writes silently missing during the outage | wqueue queued offline | `pbrainctl client queue list` (`--dead` for poison rows); they drain on reconnect |

## Offline resilience (v3.1)

Writes never fail because the daemon is unreachable. The three failure modes (workstation offline, daemon down, OS/MinIO down) all collapse to one path: enqueue + retry.

### Flow

1. MCP write tool (perceive/learn/attach/trace, or task_complete's promote step) calls `internal/mcp/wqueue_helper.go::EnqueueAndAttempt`.
2. `wqueue.Enqueue` writes the row to local SQLite. For `attach`, bytes are copied to the staging dir BEFORE the row is inserted — a crash mid-Enqueue leaves orphan files (swept by drainer), never an orphan row pointing at missing bytes.
3. Inline POST to daemon attempted. Success → `wqueue.Delete` the row, return clean tool result. Failure → row stays, tool result gets a queued-notice appended: `Queued (daemon unreachable since 2m). 3 writes pending sync.`
4. Background drainer (30s tick) picks eligible rows (backoff-elapsed, not currently claimed), claims with `claimed_at = now`, attempts POST, deletes on success, releases claim + bumps `attempts` on failure. Permanent failures (4xx, malformed payload, unknown kind) or rows past `MaxAttempts` are dead-lettered (`MarkDead`) instead of retried forever — inspect them with `pbrainctl client queue list --dead`.

### What queued writes look like to recall

**Invisible** until they sync. Queued ≠ searchable. Recall is online against `pb_records`, but a note still sitting in `wqueue.sqlite` (because the daemon was unreachable) hasn't reached the daemon yet — so it isn't a record, isn't projected, and recall won't surface it until the drainer syncs it and the daemon ingests + projects it. Once the POST lands, the raw projection is immediate and the distilled body follows after synth; there is no snapshot to rebuild and no agent restart needed.

Users who care about what's still pending can `pbrainctl client queue list` to inspect the offline backlog.

### Sentinel error: `brain.ErrDaemonUnreachable`

`internal/brain/client.go::do` wraps every transport failure (timeout, connection refused, EOF) with this sentinel. Callers that need to distinguish "daemon down" from "internal error" use `errors.Is(err, brain.ErrDaemonUnreachable)`. The `pbrainctl client queue drain-now` subcommand uses this to exit 0 (daemon-down, retry later, not the operator's problem) vs exit 1 (real internal error).

### Operator subcommands

- `pbrainctl client queue list` — read-only inspection. Returns `no queue (no offline activity yet)` on a fresh box (uses `wqueue.OpenReadOnly` — does NOT side-effect-create the file).
- `pbrainctl client queue drain-now` — force a drain attempt. Exits 0 with "N pending" if daemon still down; exits 0 + clean if drained; exits 1 on internal error.
- `pbrainctl client queue clear --confirm` — escape hatch. Deletes rows + staging files.

### What v3.1 does NOT solve

- **Daemon reachability for reads**: recall/fetch are online. If the daemon/OS/Postgres is unreachable, recall and fetch **error** — there is no stale local fallback (a deliberate cutover decision; the snapshot read path was deleted). Writes still queue and drain; only reads hard-depend on the daemon.

(Synth durability — previously a Phase 7 gap — is now handled by the continuous SoR-backed sweeper; see Concurrency invariants.)

## History (how we got here)

Two major breaks shaped the current design:

**Phase 6 (v2.0+)** — broke from v1 (TypeScript / Obsidian / single-process): a Go MCP agent + Go HTTP daemon replaced the TS server; canonical content moved off the on-disk Obsidian vault. **Note:** Phase 6 introduced a snapshot/sqlite-vec read model (the daemon published `snapshot-<gen>.tar.zst` tarballs containing a `vectors.db` that agents pulled at birth, and agents recalled locally). **That snapshot model was itself removed in the #92 cutover (below) and is no longer how the system works** — references to snapshots, `vectors.db`, gen-counters, or `_published/` describe the old v2–v3.8 design.

**The #92 cutover (epic D1/D2a/D2b, v3.9.x)** — retired the OpenSearch-as-SoR + snapshot read model:
- System of Record moved to **per-profile Postgres** (`pb_<profile>`, pgvector); OpenSearch collapsed to a **single `pb_records` projection**; the legacy `pb_summaries`/`pb_entities`/`pb_attachments` indices are no longer written or read.
- The **snapshot machinery and the local sqlite-vec read cache were deleted** — `internal/index`, `vectors.db`, `BuildSnapshotFromOS`, the `SnapshotDebouncer`, `/api/brain/snapshot/*`, `pbrainctl server snapshot …`, and birth snapshot-pull all gone.
- **Reads went online**: `brain_recall` / `brain_fetch` POST the daemon every time (always fresh; no stale fallback). Writes still go through the v3.1 wqueue + the transactional River projection outbox; a continuous SoR-backed sweeper makes synth durable.
- A follow-on audit fix pass (sets A/C/D, see `docs/audit-2026-06-27.md`) repointed `brain_forget`/`brain_reflect` at the Postgres SoR and restored kNN recall by persisting the agent-computed embedding on the SoR record.

The vault format on disk (Raw/curated, Raw/gathered, Raw/attachments) survives only for the bulk-migration path — the daemon doesn't read it during normal operation. Attachments live in MinIO at `<profile>/<vault>/attachments/<sha><ext>`.
