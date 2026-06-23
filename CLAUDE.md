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

Plain `go build`/`go test` won't work — `internal/index` panics at init() without the `sqlite_fts5` build tag. Always use `make` or `GOFLAGS=-tags=sqlite_fts5 go test ./...`.

### Subcommand layout (v3.0)

`pbrainctl` groups every command under one of two parents — `client` (workstation / agent side) or `server` (daemon host). The old flat names (`pbrainctl serve`, `pbrainctl mcp`, `pbrainctl vault ...`) were removed in v3.0; there are no aliases. Quick reference:

```
pbrainctl client mcp                 # stdio JSON-RPC MCP server (per agent process)
pbrainctl client ingest-bulk         # bulk loader for an Obsidian-shaped tree
pbrainctl client migrate-legacy      # one-time v4.x → v5.0 brain dir migration
pbrainctl client brain list|show|orphans
pbrainctl client gc-brains           # local brain dir GC; bindless walk if no scope set
pbrainctl client queue list|drain-now|clear   # v3.1 write-ahead queue inspection
pbrainctl client version

pbrainctl server serve               # HTTP daemon (per-(profile, vault) synth + snapshot publisher)
pbrainctl server vault list|status|reload
pbrainctl server snapshot status|rebuild|prune|claims
pbrainctl server queue depth|contributors
pbrainctl server maintenance enter|exit
pbrainctl server backfill-attachment-stubs
pbrainctl server bucket create|list           # v3.3 MinIO bucket admin
pbrainctl server binding create|list|delete   # v3.3 single-command binding workflow
pbrainctl server version
```

## Architecture

phantom-brain is a **Model Context Protocol server** that gives Claude Code (and any other MCP-compatible agent) long-term durable memory plus per-session active memory. The implementation is Go, the runtime split is two-process:

- **`pbrainctl client mcp`** — per-agent stdio MCP server. Talks to the daemon over HTTP. Holds local SQLite for active memory + a snapshot cache for fast recall. Spawned by Claude Code per session.
- **`pbrainctl server serve`** — single HTTP daemon. The canonical store. Receives writes, persists to OpenSearch + MinIO, runs async synth (gate + distill via the `claude` CLI), publishes snapshot tarballs the agents pull at birth.

Phase 6 (v2.0+) moved the canonical content store from a local Obsidian vault to **OpenSearch + MinIO**. The local-filesystem "vault" still exists for legacy bulk-migration paths and tests, but agent reads + writes target the daemon, not disk.

### Storage tiers (memory model)

| Tier | What it is | Where in code |
|---|---|---|
| **Long-term memory** | OpenSearch indices (`pb_summaries`, `pb_entities`, `pb_attachments`) + MinIO for attachment blobs. Canonical, durable, shared across all agents bound to the same vault. v3.2+: indices and MinIO bucket MAY be per-binding-prefixed via `[storage_overrides]` (see "Per-binding storage overrides" below). | `internal/osearch/` + the daemon HTTP surface in `internal/server/` |
| **Active memory** | Per-process SQLite at `_index/wm-<pid>.sqlite`. Tasks, findings, artifacts, open questions. Lives only for the agent process; dropped at exit. | `internal/working/` |
| **Read cache** | Per-brain `_index/vectors.db` (sqlite-vec + FTS5). A snapshot of the daemon's OS view, pulled as a tarball at birth. Read-only — the agent doesn't write directly. | `internal/index/` + `internal/brain/` birth machinery |
| **Write-ahead queue** (v3.1) | Per-binding SQLite at `<VaultBaseDir>/wqueue.sqlite` + attachment staging dir. Catches writes when the daemon is unreachable; drainer goroutine retries with exponential backoff. Persists across MCP child deaths. | `internal/brain/wqueue/` + `internal/brain/drainer.go` |

Promotion path: `task_complete` aggregates important findings into a single markdown note and POSTs it as a `brain_learn` to the daemon → lands in long-term as a `task_summary` doc.

Write path (v3.1): every write tool calls `wqueue.Enqueue` first, then attempts the daemon POST inline. Success → `Delete` the row, return clean result. Failure (network error, 5xx, timeout) → leave the row, append a queued-notice to the tool's text result, retry from the drainer. Caller never sees a write failure. Daemon SHA-dedups so retries are always safe.

### Entry points

| Path | Role |
|---|---|
| `cmd/pbrainctl/main.go` | CLI entry. Routes to `clientCmd` or `serverCmd` parents (v3.0 restructure). |
| `internal/server/server.go::Start()` | Daemon lifecycle: load config, build OS client, init MinIO backend, spawn `SynthWorker` + `SnapshotDebouncer`, mount chi router. |
| `internal/mcp/server.go::Register()` | MCP tool registration. Mounts every `brain_*` and `task_*` tool against an `mcp-go` server. |
| `internal/brain/lifecycle.go::Start()` | Per-agent birth: claim a brain dir, pull current snapshot, open `wqueue`, start heartbeat + drainer goroutine. |
| `internal/brain/wqueue/wqueue.go` | Per-binding SQLite write-ahead queue. `OpenOrCreate` (agent) vs `OpenReadOnly` (CLI inspection). `UNIQUE(kind, sha)` + `claimed_at` TTL prevents concurrent-drainer races. |
| `internal/brain/drainer.go` | 30s-tick goroutine spawned by Lifecycle. Drains eligible queue rows, refreshes snapshot metadata via `/api/brain/snapshot/current`, sweeps orphaned staging files. |
| `internal/brain/connectivity.go` | Per-Lifecycle state machine: `offline` (no successful contact this session) → `degraded` (had success, last attempt failed) → `online` (last attempt succeeded). Flips on first successful daemon contact, not on empty queue. |
| `internal/mcp/wqueue_helper.go` | Bridges MCP write tools to `wqueue.Enqueue` + appends queued-notice when daemon POST fails. |

### MCP tools (the public surface)

| Tool | File | Purpose | Writes to |
|---|---|---|---|
| `brain_perceive` | `internal/mcp/ingest.go` | Ingest gathered web content. Kind: `web_scrape`. | Long-term (daemon → OS) |
| `brain_learn` | `internal/mcp/learn.go` | Ingest a curated note. Kind: `note`. Skips LLM gate (defaults to medium reliability). | Long-term |
| `brain_attach` | `internal/mcp/attach.go` | Ingest a binary file. Kind: `attachment_stub`. Bytes → MinIO; metadata → OS. | Long-term + MinIO |
| `brain_recall` | `internal/mcp/recall.go` | Hybrid BM25 + kNN over the local read cache. Result hits include title, kind indicator (`[note]` / `[attachment pdf]` / etc.), 150-char snippet, and a fetch hint for attachments. Footer mentions snapshot age when > 1h. | Reads only |
| `brain_trace` | `internal/mcp/trace.go` | Read the local Wiki/_log.md audit trail. | Reads only |
| `brain_checkpoint` | `internal/mcp/brain_checkpoint.go` | Force a checkpoint of the working-memory state. | Local working DB |
| `brain_status` | `internal/mcp/brain_status.go` | Report brain state (gen, snapshot SHA, heartbeat age) + v3.1 connectivity (`online`/`degraded`/`offline`), `queued_writes` depth, `last_daemon_contact_secs`, `snapshot_age_secs`. | Reads only |
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
  └─ internal/mcp/wqueue_helper.go::EnqueueAndAttempt   ← v3.1 intercept
       ├─ wqueue.Enqueue → row written to local SQLite + (attach only) bytes staged
       ├─ brain.Client.Perceive(req)        ← agent-side HTTP, inline attempt
       │   ├─ success → wqueue.Delete, return clean tool result
       │   └─ failure (ErrDaemonUnreachable / 5xx / timeout) → row stays, append queued-notice
       │         (drainer goroutine retries every 30s with exp backoff)
       ↓ (success path continues to daemon)
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

Agent-side (per binding, shared across brain instances):

```
$XDG_DATA_HOME/phantom-brain/<profile>/<vault>/
  wqueue.sqlite             ← v3.1 write-ahead queue (per binding)
  wqueue-attach/<sha><ext>  ← v3.1 staged attachment bytes
  _snapshot-cache/          ← snapcache fallback for offline-at-birth
  brains/<brain_id>/
    manifest.json           ← alive | shutting_down | dead, heartbeat, parent_gen, parent_snapshot_built_at
    markers/<brain_id>      ← heartbeat sentinel
    _index/
      vectors.db            ← snapshot extract — what `internal/index` opens
      wm-<pid>.sqlite       ← per-process working memory
```

`_index/vectors.db` is what hybrid recall reads. `wqueue.sqlite` survives brain-dir destruction — next agent for the same binding picks up any pending writes. The drainer also polls `/api/brain/snapshot/current` every cycle to refresh the in-memory snapshot-built-at without re-pulling the tarball (mid-session full refresh remains a Phase 7 item).

### Config layout

```
$PHANTOM_BRAIN_CONFIG_DIR/
  server.toml                                      ← [server] [storage] [opensearch] [defaults]
  profiles/<profile>/vaults/<vault>/
    auth.toml                                      ← bearer_token = "..."
    config.toml                                    ← (optional) per-vault VaultOverrides
```

Bearer tokens live in `auth.toml`, ONE per vault binding. The daemon's registry walks `profiles/*/vaults/*/auth.toml` at startup + on SIGHUP.

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
pbrainctl client ingest-bulk <path> [--dry-run] [--concurrency N] [--max-file-bytes N] [--timeout-secs N]
```

Routes by directory:
- `Raw/curated/*.md` → `brain_learn` (Kind: note; email-shaped frontmatter → email_import)
- `Raw/gathered/*.md` → `brain_perceive` (Kind: web_scrape)
- `Raw/attachments/*` → `brain_attach` (Kind: attachment_stub)

Embeddings computed locally via Ollama. Idempotent — daemon dedups by SHA. `--dry-run` walks the tree and prints the plan without POSTing.

### Concurrency invariants

- **Daemon synth queue**: in-memory channel, single worker per daemon. No file-queue on the daemon side. Restart drops queued-but-not-processed jobs (Phase 7 will durable-queue).
- **Agent write-ahead queue (v3.1)**: SQLite-backed, durable across MCP child deaths. `UNIQUE(kind, sha)` makes re-enqueue of the same SHA a no-op (benign). `claimed_at` + 5min TTL prevents two drainers from racing on the same row; on expiry an abandoned claim auto-releases. WAL mode + 5s busy_timeout for multi-process safety.
- **Entity pages**: `UpsertEntity` reads-modifies-writes the OS doc. Safe under single-worker; would need OS painless scripts for multi-worker.
- **Snapshot rebuild**: `SnapshotDebouncer` per-vault timer registry. Bursts collapse into one rebuild per `snapshot_rebuild_debounce_secs` (default 60s).
- **Doc-ID separator**: SHA-based, format `<profile>:<vault>:<sha>`. Colon, not slash — opensearch-go interpolates IDs raw into URL paths and slashes silently 404.
- **Embedding zero check**: OS rejects all-zero vectors under cosine similarity. Either send nil (caller didn't compute) or a real embedding.
- **Drainer cadence**: 30s poll. On each cycle: claim eligible rows, attempt POSTs, refresh snapshot metadata from `/api/brain/snapshot/current`, sweep orphan staging files. Backoff per row: exponential, base 30s, cap 5min, ±20% jitter.

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

Before the HTTP listener opens, `Daemon.Start` walks every binding, collects the distinct (prefix, bucket) pairs, and runs:
- `osearch.Client.EnsurePrefixes` — idempotent create-if-missing for `pb_summaries`, `pb_entities`, `pb_attachments` under each prefix.
- `MinIOBackend.EnsureBucketExists` — probes (no create) for each distinct bucket. Missing bucket = startup error (`mc mb` is operator action).

Failure here aborts startup. Better to refuse to serve than to 500 on every write to a misconfigured binding.

### Operator-footgun guard

`VerifyStorageOverrides` runs after eager-ensure for every binding whose resolved prefix differs from the daemon default. For each such binding:

1. Count `pb_summaries` matching `(profile, vault)` under the binding's prefixed index.
2. If 0, count the same query under the SHARED prefix.
3. If shared count > 0, refuse to start with:

   `server: binding client_x/main has [storage_overrides] (prefix="client_x_") but 1234 docs exist on shared indices — run migration or revert config`

The trigger condition is "operator added `[storage_overrides]` to a binding that already had data on the shared prefix" — the binding would silently stop seeing its own docs. A binding that's migrated (docs on both shared AND prefixed, or only on prefixed) passes the check.

### Tenant-boundary safety

Cache miss on `resolveOS` / `resolveAttach` / `SynthWorker.resolveForJob` returns an error rather than silently falling back to the shared daemon-global infrastructure. Synthesising a doc into the wrong tenant's indices is a worse failure than dropping the job; HTTP handlers return 500 ("binding configuration error") and SynthWorker drops the job, expecting a re-enqueue (`brain_reflect`) once the binding view is registered. Tests + legacy single-binding daemons opt back into the shared fallback explicitly via `Daemon.allowSharedFallback`.

## Offline resilience (v3.1)

Writes never fail because the daemon is unreachable. The three failure modes (workstation offline, daemon down, OS/MinIO down) all collapse to one path: enqueue + retry.

### Flow

1. MCP write tool (perceive/learn/attach/trace, or task_complete's promote step) calls `internal/mcp/wqueue_helper.go::EnqueueAndAttempt`.
2. `wqueue.Enqueue` writes the row to local SQLite. For `attach`, bytes are copied to the staging dir BEFORE the row is inserted — a crash mid-Enqueue leaves orphan files (swept by drainer), never an orphan row pointing at missing bytes.
3. Inline POST to daemon attempted. Success → `wqueue.Delete` the row, return clean tool result. Failure → row stays, tool result gets a queued-notice appended: `Queued (daemon unreachable since 2m). 3 writes pending sync.`
4. Background drainer (30s tick) picks eligible rows (backoff-elapsed, not currently claimed), claims with `claimed_at = now`, attempts POST, deletes on success, releases claim + bumps `attempts` on failure.
5. Drainer also refreshes `Lifecycle.snapshotBuiltAt` from `/api/brain/snapshot/current` each cycle — so `brain_recall`'s footer + `brain_status`'s `snapshot_age_secs` reflect the daemon's view, not just the birth-time value.

### What queued writes look like to recall

**Invisible** until they sync. Queued ≠ searchable. The local `vectors.db` is built from the snapshot tarball at birth and never mutated by the agent. A note queued at 10am while offline is in `wqueue.sqlite` but not in `vectors.db` — recall won't surface it until the drainer syncs + the daemon synthesises + a new snapshot publishes + the agent restarts and pulls the new gen.

This is a deliberate decision (#61): the snapshot is canonical; local divergence is not. Users who care about searching their own offline writes can `pbrainctl client queue list` to inspect what's pending.

### Sentinel error: `brain.ErrDaemonUnreachable`

`internal/brain/client.go::do` wraps every transport failure (timeout, connection refused, EOF) with this sentinel. Callers that need to distinguish "daemon down" from "internal error" use `errors.Is(err, brain.ErrDaemonUnreachable)`. The `pbrainctl client queue drain-now` subcommand uses this to exit 0 (daemon-down, retry later, not the operator's problem) vs exit 1 (real internal error).

### Operator subcommands

- `pbrainctl client queue list` — read-only inspection. Returns `no queue (no offline activity yet)` on a fresh box (uses `wqueue.OpenReadOnly` — does NOT side-effect-create the file).
- `pbrainctl client queue drain-now` — force a drain attempt. Exits 0 with "N pending" if daemon still down; exits 0 + clean if drained; exits 1 on internal error.
- `pbrainctl client queue clear --confirm` — escape hatch. Deletes rows + staging files.

### What v3.1 does NOT solve

- **Daemon-side synth durability**: SynthWorker still uses an in-memory channel. Daemon crash during synth loses the in-memory queue.
- **Mid-session full snapshot refresh**: agent's local `vectors.db` is still birth-time. The drainer refreshes the displayed metadata, not the data. Phase 7 remains the home for true mid-session refresh.

## What changed in Phase 6 (v2.0+)

Major break from v1 (TypeScript / Obsidian / single-process). If you're reading old v1.x docs:

- Old: TypeScript MCP server backed by Obsidian markdown vault on disk. New: Go MCP server (agent) + Go HTTP daemon (canonical store) + OpenSearch + MinIO.
- Old: synthesizer polled a filesystem queue. New: in-memory channel + single `SynthWorker` goroutine, debounced snapshot rebuild.
- Old: agents shipped death-payload tarballs at shutdown; daemon's reaper merged them. New: agent POSTs writes synchronously during life; no death payload; no reaper.
- Old: snapshot tarball was a reflink of the local Wiki tree. New: `osearch.Export` bulk-scrolls OS into a fresh sqlite-vec + FTS5 db.
- Old: attachments stored on local filesystem under `Raw/attachments/<sha><ext>`. New: MinIO at `<profile>/<vault>/attachments/<sha><ext>`.

The vault format on disk (Raw/curated, Raw/gathered, Raw/attachments) survives only for the bulk-migration path — the daemon doesn't read it during normal operation.
