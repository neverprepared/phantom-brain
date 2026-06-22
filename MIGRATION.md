# Migration: v1.x → v2.0 (Phase 6)

v2.0 is a **breaking change**. The data plane moved from a death-payload + reaper + local-collective architecture to **OpenSearch as the canonical content store**, with agent writes flowing synchronously through the daemon and **per-agent SQLite caches** populated from snapshot tarballs at birth.

Single-operator deploys take ~10 minutes to migrate. Multi-host fleets need to coordinate the cut.

---

## What changed

### Architecture

| Surface | v1.x | v2.0 |
|---|---|---|
| Canonical store | Daemon's local filesystem (`Wiki/`, `_index/`) | **OpenSearch** (`pb_summaries`, `pb_entities`, `pb_attachments`) |
| Agent → daemon transit | Death tarball uploaded at shutdown, daemon's reaper merges into local FS | Synchronous `POST /api/brain/{perceive,learn,attach}` during life |
| Snapshot source | Reflink of local `Wiki/` + `_index/vectors.db` | `osearch.Export`: bulk scroll OS → fresh sqlite-vec+FTS5 tarball |
| Synth queue | File-based (`queue/pending/`, `queue/claimed/`, …) drained by polling goroutine | In-memory channel drained by a daemon-level `SynthWorker` |
| Attachment blobs | Local FS under `Raw/attachments/` | MinIO at `<profile>/<vault>/attachments/<sha><ext>` |

### Removed surfaces

- Daemon: `/api/brain/merge/init`, `/api/brain/merge/upload/{id}`, `/api/brain/merge/complete/{id}` (and the `LocalBackend` HMAC upload-token system feeding them).
- Daemon: file-queue synthesizer + reaper goroutines per vault.
- Agent: `internal/brain/shipqueue.go` (no more outbound death tar queue).
- Agent: `internal/brain/death.go::packDeathPayload` (Death just flips status now).
- CLI: `pbrainctl force-merge` and `pbrainctl force-checkpoint` (no in-band queue to drain).

### New surfaces

- Daemon: `POST /api/brain/{perceive,learn,attach,trace}` and `GET /api/brain/attach/{sha}`.
- Daemon: `[opensearch]` config block in `server.toml` (addresses + auth + sandbox prefix).
- Daemon: `SnapshotDebouncer` triggers a rebuild after every synth completion (debounced by `snapshot_rebuild_debounce_secs`, default 60s).
- CLI: `pbrainctl ingest-bulk <vault-dir>` — bootstrap loader for the bulk migration.
- Agent: `brain.Client` grew `Perceive` / `Learn` / `Attach` / `Trace` / `AttachGet` methods.

### Compatibility break

- **v1.x agents will not talk to a v2.0 daemon.** The `/merge/*` endpoints are gone; the agent's `UploadShipQueue` returns 404. Upgrade agents in lockstep.
- **v2.0 agents will not talk to a v1.x daemon.** The `/api/brain/perceive` etc. endpoints don't exist there.
- **Existing v1.x brain dirs on agent hosts can be deleted.** Birth re-pulls from the new daemon's snapshot.
- **`brain_status` JSON shape dropped `ship_queue_count` / `ship_queue_bytes` fields.** Any monitoring that parses these breaks; treat as zero.

---

## Migration steps

### 1. Stand up OpenSearch

A single-node dev instance is enough for a personal deploy. Docker Compose includes one:

```sh
cd docker
# .env already has MINIO_ROOT_PASSWORD; nothing new required.
cp -r config-example config       # if you haven't already
# The new config-example/server.toml has [opensearch] pointing at
# the compose service; copy or merge into your live config.
docker compose up -d --build
docker compose logs -f opensearch # wait for "started" then ctrl-c
```

For an existing real OpenSearch cluster, point `server.toml`'s `[opensearch]` at it and provide creds:

```toml
[opensearch]
addresses            = ["https://os.internal:9200"]
username             = "phantom-brain"
password             = "..."
insecure_skip_verify = false
```

Indices auto-create on daemon startup (`pb_summaries`, `pb_entities`, `pb_attachments`).

### 2. Upgrade the daemon

```sh
docker compose pull pbrainctl    # or: docker compose build pbrainctl
docker compose up -d pbrainctl
curl http://localhost:9998/api/brain/health   # expect 200 + vault list
```

The daemon will refuse to start if `[opensearch]` is configured but unreachable.

### 3. Load existing content (one-shot)

Run `ingest-bulk` against your existing Obsidian vault. It walks `Raw/curated/`, `Raw/gathered/`, and `Raw/attachments/`, embeds locally via Ollama, and POSTs to the daemon.

```sh
export CL_BRAIN_API=http://localhost:9998
export CL_BRAIN_API_TOKEN=$(cat ~/.config/phantom-brain-server/profiles/personal/vaults/memory/auth.toml | grep bearer | cut -d'"' -f2)

pbrainctl ingest-bulk ~/obsidian/vaults/personal-memory --dry-run    # plan
pbrainctl ingest-bulk ~/obsidian/vaults/personal-memory              # do it
```

A ~1.3 GB vault takes hours because the daemon's `SynthWorker` runs the `claude` CLI for gate + distill on every doc. Synth is async — the bulk loader returns when uploads complete; enrichment continues in the background.

Track completion via daemon logs:

```sh
docker compose logs -f pbrainctl | grep "synth"
```

### 4. Upgrade agents

After the bulk load drains:

```sh
brew upgrade neverprepared/tap/pbrainctl
# or for tarball deploys: re-download from the v2.0.0 GitHub release
```

Delete the agent's old brain dir(s) and restart:

```sh
rm -rf ~/.local/share/phantom-brain/brains/*
# Restart whatever launches `pbrainctl mcp`; it'll birth fresh from the daemon snapshot.
brain_status   # in your MCP client; should report seed_source=tarball
brain_recall "something you know is in the vault"   # should hit
```

### 5. Verify + retire

- `pbrainctl snapshot status personal/memory` → non-zero `gen` with size comparable to your prior `_index/vectors.db`.
- `brain_recall` returns results against ingested content.
- Drop any v1.x backups / shipqueue staging dirs (`~/.local/share/phantom-brain/ship-pending/`).

---

## Rolling back

If something breaks during step 2 (daemon upgrade), the rollback is:

```sh
docker compose down pbrainctl
docker compose pull pbrainctl:v1.2.1   # or whatever your last v1 tag is
docker compose up -d pbrainctl
```

OpenSearch + MinIO can stay running; v1.x daemons ignore both. The v2.0 docs in OS stay dormant; a re-cut at v2.0.x will pick up where you left off because doc IDs are content-addressed by SHA256.

After step 3 (bulk load) you're operationally committed — v1.x agents won't see the OS-side content. If you must roll back, your last `_index/vectors.db` on the daemon host is the v1 truth.

---

## Operational notes

- **Synth queue is in-memory.** A daemon restart drops queued jobs (LLM cost wasted; raw docs stay in OS). Re-trigger by calling `brain_perceive` again or running `pbrainctl ingest-bulk` against the affected paths — SHA dedup makes it cheap.
- **Snapshot rebuild is debounced.** First synth landing kicks the 60-second timer; bursts collapse into one rebuild. Tune `snapshot_rebuild_debounce_secs` in `[defaults]` if you want a tighter window.
- **Index prefix sandboxes.** Set `index_prefix = "stage_"` in a staging server.toml to run against the same OS cluster without colliding with prod.
- **No mid-session refresh.** Snapshot-at-birth UX retained — an agent's local cache is frozen until it restarts and re-births. Polling / SSE push is on the table for v2.1.
