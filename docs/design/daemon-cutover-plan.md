# Daemon cutover plan — legacy OS → Postgres-SoR + always-online recall

Companion to [`opensearch-native-memory-architecture.md`](./opensearch-native-memory-architecture.md)
and epic #92. The new design is built and merged but **dormant** — nothing in
`internal/server` calls it. This document is the agreed plan for wiring it into
the live daemon and retiring the legacy path.

## Decisions (signed off)

| Decision | Choice | Consequence |
|---|---|---|
| **Read model** | **Always-online recall**; delete the snapshot read path | `brain_recall` queries the daemon every time → always fresh, no snapshot lag. If the daemon/OS is unreachable, recall **errors** (no stale local fallback). |
| **Write resilience** | **Keep** the write-ahead queue (`wqueue`, v3.1) | Writes still survive a daemon outage and drain on reconnect. Independent of reads. |
| **Cutover safety** | **Parallel-run** (dual-write + backfill) → per-profile read-flip | Legacy stays a complete, clean rollback target until reads are validated online. |

Always-online directly resolves the staleness that motivated the redesign
("your own writes aren't recall-able until the next snapshot publishes").

## What's already built (merged, dormant)

| PR | Layer |
|---|---|
| #94 | SoR schema (`records`/`entities`/`facts`) + `migrations/` |
| #97 | Per-profile DB provisioning (`pbrainctl server db provision`) |
| #98 | sqlc data-access (`internal/pgstore/pgdb`) |
| #99 | Transactional outbox + River projection worker (`internal/projection`) |
| #100 | `pb_records` projection index + `OSProjector` (`internal/osproject`) |
| #101 | Hybrid recall query + search pipeline (`osproject.Recaller`) |

## Current (legacy) data paths — what we're changing

Grounded in code (file:line):

- **Write** — `internal/server/handlers_write.go:272` `handlePerceive` → `osc.UpsertSummary(…, Synthesised=false)` (`:320`) → `d.synth.Enqueue(…)` (`:325`). Embedding is **agent-computed** and passed through in the request (`req.Embedding`, `:317`).
- **Synth** — `internal/server/synth_queue.go:357` `processJob`: `GetSummary` → gate (`RunGate`) → distill (`SummarizeContent`, `claude` CLI) → entity extract/upsert → `UpsertSummary(…, Synthesised=true)` (`:493`) → `OnComplete` → `SnapshotDebouncer.Trigger`. **No embedding computed daemon-side.**
- **Embedding** — agent-side only: `internal/mcp/ingest.go:115`, `attach.go:168`, and the recall query at `internal/mcp/recall.go:69`. The daemon is stateless on embeddings.
- **Read / recall** — **purely agent-side over a local snapshot.** `internal/mcp/recall.go` → `Index.SearchHybrid` over `_index/vectors.db` (`internal/index`). The snapshot is pulled at birth (`internal/brain/birth.go:310` `seedFromDaemon`). **There is no daemon recall HTTP endpoint.**
- **Snapshot build** — `internal/server/snapshot_export.go:42` `BuildSnapshotFromOS` → `internal/osearch/export.go:166` scrolls `pb_summaries` (`Synthesised=true` only) → writes `vectors.db` → `_published/snapshot-<gen>.tar.zst`.
- **Per-binding resolution** — `internal/server/binding_views.go`: `osBindingView` (wraps `osearch.Client.WithPrefix`) + `minioBindingView` (per-bucket), cached in `bindingDepCache` keyed by `VaultKey`, resolved via `Daemon.resolveOS` / `resolveAttach` (`handlers_write.go:31`), rebuilt at startup + SIGHUP. **This is the pattern `pgBindingView` mirrors.**

## The phases

### Phase A — Per-binding Postgres resolution *(additive, dormant, no behavior change)*

Extend the binding-views pattern with a `pgBindingView` holding, per `(profile, vault)`:
- `*pgxpool.Pool` via `pgstore.Open(pgstore.DSNForProfile(baseDSN, profile))`
- a River client (`projection.NewClient`) with `osproject.Projector` as the worker's `Projector`
- `osproject.Recaller`

At startup / SIGHUP (`buildBindingDeps`), for each binding: open pool, `projection.MigrateRiver`, `osproject.EnsureIndex` (`pb_records`), `osproject.EnsureSearchPipeline`, start the River client. Add `Daemon.resolvePG(b) (*pgBindingView, error)` — fail-loud on cache miss, mirroring `resolveOS`.

- **Config**: base DSN from `server.toml`/`PB_POSTGRES_DSN`; per-profile derived. Requires `pbrainctl server db provision <profile>` to have run.
- **Graceful absence**: if Postgres is unconfigured, skip PG resolution — legacy is untouched.
- **Verify**: daemon starts with PG bindings resolved and reachable; no handler consumes them yet. Integration test: `resolvePG` returns a working pool + River client + Recaller.
- **Rollback**: trivial — nothing reads these yet.

### Phase B — Dual-write parallel-run + backfill *(writes only, flag-gated)*

- **Flag**: per-binding `[dual_write]` (so you enable one profile at a time).
- **Write handlers** (`handlePerceive/Learn/Attach`): after the legacy `UpsertSummary` + `Enqueue`, also call `projection.WriteRecordAndEnqueue` (record + River→`pb_records`), populating `records.embedding` from `req.Embedding`. New-store failure during parallel-run is **logged + metered, not fatal** — legacy is authoritative; backfill/`resynth` reconciles.
- **Synth** (`processJob`): after the legacy `UpsertSummary(Synthesised=true)`, also update the new-store record (`pgdb.MarkRecordSynthesised` with distilled body/reliability/topic/embedding) and write entities (`entities`/`entity_aliases`/`record_entities` + `facts`), then re-enqueue the projection.
- **Backfill**: one-shot `pbrainctl server backfill-to-pg <profile>` — scroll `pb_summaries` → insert `records` **reusing existing embeddings** (same `nomic-embed-text` 768-dim; set `embedding_model`) → enqueue projection. Idempotent (`ON CONFLICT (profile,vault,sha) DO NOTHING`). **No re-embed.**
- **Validate**: a comparison harness queries the new store via `Recaller` and diffs top-K against snapshot recall for a sample of queries — *without* flipping any agent read.
- **Verify**: dual-write produces matching `pb_records`; backfill completes; recall parity acceptable.
- **Rollback**: flip the flag off → writes go legacy-only; the new store goes stale but harmless. Reads never touched legacy.

### Phase C — Read cutover *(the consequential flip, per-profile flag)*

- **Daemon endpoint**: `POST /api/brain/recall` — body `{query, embedding, kinds, topic, memory_type, reliability, size}` → `resolvePG` → `Recaller.Recall` → hits. (Pipeline ensured in Phase A.)
- **Agent**: `internal/mcp/recall.go` — when online-recall is enabled, POST to the daemon instead of `Index.SearchHybrid`. The agent still embeds the query locally (it already does, `recall.go:69`) and sends the vector. Degraded mode (no vector) handled by the Recaller.
- **Per-profile flag**: flip one binding, validate recall quality live (fresh results include just-written content — no lag), roll back by flipping back.
- **Verify**: agent recall returns fresh hits; quality at or above the snapshot baseline.
- **Rollback**: flip flag → agent reads the snapshot again (still intact until Phase D).

### Phase D — Decommission *(after C validated across profiles)*

- Stop legacy writes (drop `pb_summaries`/`pb_entities`/`pb_attachments` writes from handlers + synth).
- **Delete the snapshot read path**: `BuildSnapshotFromOS`, `seedFromDaemon` birth-pull, `internal/index`, the gen-counter, `_published/*.tar.zst`, snapcache. Large deletion, large simplification.
- Drop the legacy OS indices (operator action).
- **Keep** `wqueue` + the drainer — they wrap the write POST regardless of store.

## Cross-cutting

- **Write queue stays** across all phases; it is store-agnostic.
- **Embedding stays agent-side**; online recall sends the query vector to the daemon (no daemon Ollama).
- **Observability**: metric dual-write divergence, recall latency, backfill progress, new-store write failures.
- **Tenant isolation** is preserved: per-profile Postgres DB (`pb_<profile>`) + per-binding OS prefix + MinIO bucket — the existing boundary, now extended to the SoR.

## Sequencing

Phase A is safe additive work and can land immediately. Phases B–D each change live
behavior and should land behind their flags with the verify/rollback gate above. Each
phase is one PR (or a small series), auto-merged on green, consistent with the issue-driven flow.
