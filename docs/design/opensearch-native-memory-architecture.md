# OpenSearch-Native Memory Architecture (design)

> Status: proposal / design exploration. Captures a target architecture and the
> reasoning behind it. Not yet implemented; see **Phasing** for an incremental path.

## 1. Context & motivation

phantom-brain today is a custom, **local-snapshot** memory system: agents pull a
birth-time snapshot tarball into a local `sqlite-vec` + FTS5 read cache, while
OpenSearch + MinIO act as the "canonical store" and an in-memory channel drives
LLM synthesis. That design optimizes for **offline, single-agent** operation.

The real goal is the opposite: **shared knowledge across many agents on many
machines.** Against that goal, the current design fights itself, and a single
build/validation session surfaced the symptoms:

- **Snapshot staleness** — recall reads a birth-time snapshot, so an agent can't
  see what it (or another agent) just wrote until it restarts and re-pulls. A
  coverage validation falsely reported ~1,000 docs "missing" that were simply
  un-snapshotted.
- **Dropped synthesis jobs** — the in-memory synth queue is non-blocking and
  overflow-drops; a bulk import outran it and silently stranded ~1,078 docs at
  `Synthesised=false` with no retry path.
- **Expensive rebuilds** — synthesized output (distilled bodies, gate verdicts,
  entities, embeddings) lives **only** in the disposable index, so a nuke-and-
  rebuild re-pays for *all* LLM synthesis (hours), not just re-indexing (minutes).
- **Single-worker synthesis** — entity "backlinks" are a denormalized
  `mentioned_by[]` array maintained by read-modify-write, which forces the synth
  worker to be single-threaded.

The deeper realizations:

1. Much of the custom machinery **reimplements infrastructure** — a job queue, a
   vector store, hybrid search, and an eventual-consistency snapshot-distribution
   system that a database + search engine already provide.
2. The **data model (Raw / Summaries / Entities) is Obsidian/filesystem-shaped**,
   carried over from the earlier Obsidian "Karpathy brain" model. It materializes
   relationships (backlinks, derived pages) because a *file store* can't query
   them cheaply. A *query engine* can.

**Principle for the rewrite:** use infrastructure for commodity concerns; keep
custom code only for the memory *semantics* that are genuinely ours.

## 2. Principles

1. **Separate the System of Record from the search index.** The SoR is durable
   and authoritative; the index is derived, rebuildable, and disposable.
2. **Materialize only what's expensive to compute** (LLM synthesis). Derive cheap
   relationships (entity backlinks, facets) at query time.
3. **Two identity models for two kinds of knowledge** (see §4): content-identity
   for immutable records; referent-identity for mutable state.
4. **Append fast, synthesize async, reconcile on a schedule, project to the index
   for finding** (CQRS / event-sourcing shape).
5. **Centralize for sharing.** Many agents/machines → one shared truth + index.
   Accept the online dependency; spend scaling effort on the write/synth side,
   not the read side.
6. **Vectors are for semantic *reach*, not speed.** Keyword (BM25) is the fast,
   exact half; vectors are the meaning half. Recall needs both → hybrid.

## 3. Target architecture — three stores, one gateway

| Store | Holds | Role |
|---|---|---|
| **MinIO** | the **bytes** — PDFs, images, office docs | blob storage |
| **Postgres** | **structured truth** — metadata, extracted/synthesized text, relationships, version history, embeddings, **+ pointers (keys) to MinIO blobs** | **System of Record** (authoritative, transactional) |
| **OpenSearch** | a **searchable copy** — text fields + `knn_vector` | derived hybrid-search index (rebuildable; holds nothing irreplaceable) |

The **daemon** stays as a stateless **gateway**: per-binding auth, multi-tenant
(profile/vault) routing/isolation. It scales horizontally.

```
ingest ──► MinIO (bytes)
       └─► Postgres (truth: records + state + history + blob pointers)
                  │  project / sync  (cheap: no LLM, no re-embed)
                  └─► OpenSearch (hybrid search index)

recall ──► OpenSearch (BM25 + kNN, fused) ──► ids/snippets
       └─► Postgres (authoritative record) ──► MinIO (blob via key)
```

The SoR is really **Postgres + MinIO together**: Postgres is the relational
truth, MinIO the blob truth, Postgres holds the keys that tie them. OpenSearch is
downstream of both.

**Division of labor:** MinIO = bytes. Postgres = truth. OpenSearch = speed.

### What changes from today

Today OpenSearch plays **two** roles — metadata system-of-record *and* search
index. The redesign **splits** them: authoritative metadata + synthesized output
move to **Postgres**; OpenSearch becomes a **pure derived index**. MinIO is
unchanged (already correct). Because the synthesized output now lives durably in
Postgres, **rebuilding OpenSearch is a re-index in minutes — no LLM, no re-embed.**

## 4. The two-identity knowledge model (core idea)

A SHA is **identity by content** ("same bytes = same thing") — perfect for dedup,
useless for updates (a changed fact has a new SHA and looks brand-new). Updates
need **identity by referent** ("what is this *about*?"). Both are needed, for
**different kinds of knowledge**:

| | **Records / events** (immutable) | **State / current-facts** (mutable) |
|---|---|---|
| Identity | by **content** (SHA, dedup) | by **referent** (entity/attribute key) |
| Examples | receipts, invoices, scrapes *as-of-a-date*, meeting notes, contract versions | current address, project status, config value, "who owns X now" |
| On new info | **append** (never update — it's history) | **upsert / supersede** |
| Forget? | basically never (it's the record of what happened) | version it; "current" moves on |

You never "update" a 2024 receipt — content identity + dedup is correct for it.
"Bob's current address" *is* meant to change — referent identity.

**Driven by `memory_type` + source kind:** `episodic` ≈ immutable record;
`semantic`/`procedural` ≈ may be mutable state; an attachment/receipt is a
*record*, an extracted "current status" is *state*.

**They layer (event sourcing):** the immutable content-addressed records are the
**log** (durable truth of what happened); the referent-keyed mutable facts are a
**projection** computed from it. A receipt is stored once, immutably; the derived
fact ("total spent with vendor X", "latest status") updates as new records
accrue. Each *change* to a mutable fact is itself an immutable versioned record
(`valid_from`/`valid_to`), so the mutable "current" view always rides on an
immutable history.

## 5. Data model (OpenSearch-optimized)

- **Records** (append-only, content-addressed): the durable knowledge units —
  raw + synthesized fields, embedding, provenance (`source`/`source_url`),
  `memory_type`/`kind`, blob pointer (`minio_key`) for attachments. Most of the
  corpus lives here and simply accretes + dedups. Each embedding carries
  `embedding_model` + `embedding_version` (§13) so a model change is an
  incremental re-embed, not a big-bang reindex.
- **State / facts**: `(entity, attribute, value)` with a version history table.
  `UNIQUE(entity_id, attribute)` → a new value **UPSERTs** and supersedes the old
  (kept as history). Relational integrity makes "the current value of X" a row you
  update, not a blob you re-append.
- **Entities**: a canonical entity table + an **alias** table + a **note↔entity
  join** table. This **replaces the denormalized `mentioned_by[]`** — "what
  mentions X" becomes a *join/query*, not a materialized backlink. Consequences:
  no read-modify-write race, **no single-worker synthesis constraint**, and the
  `pb_entities`-as-backlink-store index can go away. (Keep a thin entity table
  only if you want entity-level descriptions; not the backlinks.)
- **Attachments**: **one** record per attachment (metadata + `minio_key` +
  `extracted_text` + embedding), not today's stub-in-summaries + doc-in-
  attachments pair.
- **OpenSearch projection**: index the searchable fields (title, body,
  extracted_text, tags) + `knn_vector`. Pure derivation from Postgres, kept in
  sync via the **transactional outbox** (§13) — never a dual-write.

**OpenSearch is a search engine, not a graph DB.** Shallow one-hop entity facets
("what mentions X", co-occurrence) → OpenSearch aggregations or a Postgres join.
A genuine multi-hop knowledge graph would need a graph store — **deferred** until
that's a real product need (don't add it speculatively).

## 6. Search & embeddings

**Hybrid search is native to OpenSearch.** BM25 (`match`) + kNN (`neural`) fused
by a search pipeline (normalization-processor and/or RRF). Adopting it lets us
**delete the custom `sqlite-vec` + FTS5 + hand-rolled RRF** fusion in
`internal/index` — that local stack is, almost line-for-line, a reimplementation
of what OpenSearch ships.

| Today (local) | OpenSearch native |
|---|---|
| sqlite FTS5 (BM25) | Lucene BM25 (`match`) |
| sqlite-vec (kNN) | k-NN plugin (`neural`) |
| custom RRF in Go | `hybrid` query + normalization/RRF processor |

**Embeddings — open decision.** Two coherent options:

- **(A) Hybrid (keep Ollama client-side; move only the *search* to OS).** The
  client still embeds (Ollama) for ingest and query; OpenSearch does the kNN over
  a client-supplied query vector using the `knn_vector` index it already has. No
  connector, no model in-cluster. Keeps an offline-capable local path; adds a
  live-OS "fresh" path to fix staleness. Lightest change.
- **(B) Neural search (move *embedding* to OS).** OpenSearch embeds via ml-commons
  — either a **connector** to Ollama (`/v1/embeddings`, OpenAI-compatible; reuse
  `nomic-embed-text` so vectors stay consistent) or an **in-cluster** ONNX model.
  The client sends text, not vectors. But the `neural` **query is server-side, so
  recall goes online** (unwinds the offline snapshot model).

**Identity vs. search recap:** whoever holds the embedding model is in the path of
*every query* (you must embed the query to compare it). Model on client (Ollama) →
queries can be local/offline. Model in/behind OS → every query touches OS. You can
have "no Ollama" *or* "no model behind OS," not both.

**Recommendation:** given the shift to centralized, shared, online operation,
**(B) via a connector to a load-balanced Ollama** is coherent and removes
client-side embedding — accepting that recall is online. Keep **(A)/local sqlite**
as an *optional* offline fallback to design later if offline turns out to matter.

**Ollama operations:** put instances **behind HAProxy** (over nginx — free active
health checks). The neural path centralizes embedding load onto a shared Ollama.
Only helps across **separate hardware**
(same-GPU instances just contend). Co-locate OS + Ollama (LAN), keep models warm
(`OLLAMA_KEEP_ALIVE`), use keepalive, point the connector at the nginx VIP. Note:
load-balancing Ollama does **not** speed up synthesis — that bottleneck is the
`claude` gate/distill, not embedding.

## 7. Pipelines (CQRS shape)

| Stage | When | Notes |
|---|---|---|
| **Append record** (Postgres + MinIO) | **synchronous, fast** | the SoR write; must not be lost. Content-dedup (SHA) here. |
| **Synthesis** (gate / distill / embed / entity-extract) | **async, per-record** (durable queue) | expensive LLM; write results back to Postgres → project to OS. **Parallelizable** once the entity read-modify-write is gone. |
| **Reconciliation** (entity resolution, supersede, near-dup → mutable-state projection) | **async, batch / scheduled, idempotent** | `brain_reflect` generalized. Continuous worker, scheduled sweep, or on-demand. Isolated from the interactive path. |

**Durable queue replaces the in-memory channel.** It absorbs bursts, never drops,
applies backpressure — the real fix for the dropped-jobs failure. Recommended:
**River** (a Postgres-backed Go job queue) — it reuses the Postgres we're already
standing up, so no Redis/NATS, and queue state lives in the same transaction as
the write (pairs naturally with the outbox in §13).

The write path just **appends the immutable record and enqueues** — it never
blocks on synthesis or reconciliation. Everything downstream is eventually-
consistent derivation over a log that is already durable.

## 8. Update / reconciliation mechanics (matching old ↔ new)

Manufacturing the old↔new link, **most-reliable → most-fuzzy**:

1. **Provenance / same-source** (deterministic, do first) — same `source_url` /
   file / record id → update that source's prior state. Covers re-scrapes,
   re-syncs, refreshed documents.
2. **Structured keys** — extract `(entity, attribute, value)`; `UNIQUE(entity,
   attribute)` → **upsert**; old value → history.
3. **Entity resolution** — canonical id + alias table + embedding similarity +
   LLM adjudication. Probabilistic.
4. **Semantic near-dup** — embedding similarity + entity overlap + LLM decides
   *supersedes / complements / unrelated*.
5. **Confirm** — propose-then-apply for high-stakes merges; auto-apply only above
   a confidence threshold.
6. **Version, never destroy** — supersede (`valid_from`/`valid_to`,
   `superseded_by`); keep history; "current" = latest.

**Layer split:** the **records** layer just accretes + dedups (no reconciliation
needed). The **state** layer is what gets reconciled. `brain_forget` shrinks back
to its proper role — genuine junk, not "retiring superseded facts" (those are
versioned, not deleted).

**Cadence:** because reconciliation is idempotent over durable records, it runs
in whatever mode fits — continuous worker (steady volume), scheduled sweep (bulk,
cost-controlled), or on-demand. The mutable-state view is then **eventually
consistent** (lags the log by the cadence); the immutable records are always
immediately consistent. For strong read-your-writes on a mutable fact, do a
read-time merge for that one query rather than making the pipeline synchronous.

## 9. Multi-agent / scaling

- **Reads scale natively** — central OpenSearch with replicas; latency is a
  **topology** question (keep the cluster near the agents; co-locate OS + Ollama).
  This is the part that "many agents finding shared knowledge" makes easy.
- **Writes** — the **synthesis pipeline is the bottleneck** under concurrent load,
  not search. Invest here: the **durable queue** (burst absorption, correctness)
  and **parallel synthesis** (unlocked by the entity-model redesign).
- **Workload evolves read-heavy.** Per-topic writes taper as the corpus covers a
  topic; residual writes shift from *net-new* (handled by dedup + recall) to
  *updates* (handled by §8 reconciliation). So invest in **reconciliation
  semantics + durable queue**, not raw write throughput.
- **Connections** — PgBouncer in front of Postgres; daemon gateway in front of
  everything for auth + multi-tenant isolation.
- **HA** — the central stores become a shared dependency; you give up per-agent
  offline survival (intrinsic to sharing). Replicate accordingly.

## 10. What to keep / drop / build

**Keep** (genuine value, not plumbing):
- The synthesis pipeline (gate / distill) — the memory semantics that are ours.
- MinIO blob storage.
- `memory_type` classification (it drives the two-identity model).
- The daemon as a per-binding multi-tenant gateway.

**Drop** (reinvented or filesystem-era):
- Custom `sqlite-vec` + FTS5 + hand-rolled RRF fusion → OpenSearch native hybrid.
- Snapshot tarballs + birth/death + local read-cache → query the index (if online).
- Denormalized entity `mentioned_by[]` → join/query.
- In-memory synth channel → durable queue.
- Attachment stub-pair → one record per attachment.

**Build:**
- Postgres SoR: records (append-only) + state/facts (upsert + history) + entities/
  aliases + join tables.
- A durable queue for synthesis.
- The reconciliation job (`brain_reflect` generalized): scheduled dedup + state
  updates.
- The OpenSearch projection/sync from Postgres.
- (Decision) embeddings: OS neural connector to a load-balanced Ollama, **or**
  hybrid (client Ollama + OS kNN search).
- (Optional, later) an offline local fallback; a graph layer if traversal becomes real.

## 11. Open decisions

1. **Embedding location** — neural-connector (online recall, no client Ollama) vs
   hybrid (offline-capable, keeps client Ollama). Drives §6.
2. **Offline support** — required, or not? Determines whether any local/sqlite
   path survives at all.
3. **Reconciliation cadence + auto-merge confidence threshold** — how fresh the
   mutable-state view is, and how much human confirmation it needs.
4. **Postgres now vs. interim** — the synthesis-durability fix (so rebuilds don't
   re-pay LLM) can land *first* by persisting synthesized output back into the
   Obsidian `Wiki/` layer (which the original Karpathy model had!), before a full
   Postgres migration.
5. **Graph DB** — deferred until multi-hop traversal is a real need.

## 12. Phasing (incremental, lowest-risk first)

- **Phase 0 (free):** *Name* the architecture — OpenSearch is a rebuildable index,
  not the SoR. Treat it as disposable. Costs nothing; clarifies every later choice.
- **Phase 1 — durable synthesis:** persist synthesized output (distilled, verdict,
  entities, embedding) to a durable layer so OpenSearch rebuilds don't re-run the
  LLM. Cheapest version: write it back into the Obsidian `Wiki/` layer.
- **Phase 2 — correctness:** replace the in-memory synth channel with a **durable
  queue** (fixes drops/backpressure) and **drop `mentioned_by`** for a join (kills
  the single-worker constraint → unlocks parallel synth).
- **Phase 3 — Postgres SoR:** move authoritative records + state + history to
  Postgres; OpenSearch becomes a pure projection.
- **Phase 4 — reconciliation:** the scheduled/batch dedup + state-update job
  (`brain_reflect` generalized) over the durable records.
- **Phase 5 — search modernization:** OpenSearch native hybrid; the embedding
  decision (neural connector vs hybrid); Ollama behind nginx.
- **Later / optional:** offline fallback; graph layer.

## 13. Design hardening (review additions)

Items an adversarial review surfaced. Ordered by importance; all are standard
patterns/tools (consistent with the "compose primitives" philosophy).

### 13.1 Index consistency — the transactional outbox (load-bearing)

"Project to OpenSearch" must NOT be a dual-write (write PG, then write OS): if PG
commits and the OS write fails or the process dies between them, the index
silently disagrees with the SoR — the same class of silent inconsistency that
defined the migration session.

**Pattern:** in ONE Postgres transaction, write the record **and** an `outbox`
row. A relay worker reads the outbox, projects to OpenSearch (idempotent upsert by
SHA), and marks the row done. Guarantees every committed record eventually reaches
the index, exactly-once-effective, no race. **River** drives the relay (same DB).
This is the backbone of "OS is a derived projection" and deserves first-class
treatment, not a verb.

### 13.2 Observability from day one (fixes the session's root pain)

The dropped-jobs / staleness pain was, at root, **invisibility** — you couldn't
see the 1,078 backlog without building a tool to look. Don't repeat it:
- **Prometheus** metrics: synth queue depth, **outbox lag**, reconciliation lag,
  recall latency (p50/p95), embedding latency, per-profile doc counts.
- **OpenTelemetry** traces across gateway → PG → OS → Ollama.
- The backlog should be a **dashboard number + alert**, not a tool you remember to
  run. The next "silently stranded" must fire an alert, not surface weeks later.

### 13.3 Embedding model versioning (one column, saves a big-bang)

`embedding vector(768)` hardcodes `nomic-embed-text`. Add `embedding_model` +
`embedding_version` per record/entity (migration 0004). A model swap becomes an
incremental, detectable re-embed instead of an all-or-nothing reindex. Cheap now,
brutal to retrofit.

### 13.4 Tenant isolation per profile (sensitive data — financial, GSA/gov)

Data is isolated **per profile** today: each profile has a dedicated MinIO bucket
and OpenSearch token (v3.2 also gives per-binding index prefixes). **The SoR should
match that boundary** — don't weaken isolation by pooling all profiles into shared
Postgres tables.

**Recommended: a Postgres database per profile** (`pb_personal`, `pb_gsa`,
`pb_lakeview`, …), resolved by the **same per-binding storage resolution** that
already maps a binding → (OS index prefix, MinIO bucket). Extend it to
(OS prefix, MinIO bucket, **Postgres DSN/database**); the gateway routes each
profile to its own DB/pool. This gives **physical isolation** — a missing
`WHERE profile=…` literally cannot cross profiles because it's a different database
— consistent with your bucket/token model, plus clean per-profile backup/DR.

- `vault` stays a **column** within a profile's DB (vaults inside one profile are a
  softer boundary than across profiles).
- Cost: N migration runs (tooling handles), per-profile backups, a pool per DB
  (PgBouncer). Trivial at a handful of profiles.
- Lighter alternative — one DB + **row-level security** + `profile` column — is
  viable but a softer boundary (a bad policy / superuser bypasses it) and less
  consistent with the physical isolation you already run. Prefer DB-per-profile.

**A2A changes the threat model.** Exposing memory as an agent other agents can call
means the Agent Card + task surface needs real authN/authZ, scoped per profile —
the same rigor as MCP's per-binding token, or it's an open door to the corpus.

### 13.5 Backup & disaster recovery

Splitting SoR from index makes **Postgres + MinIO the irreplaceable pair** (OS is
rebuildable). Therefore: Postgres WAL archiving / PITR (not just `pg_dump`), MinIO
backup/replication, and — the upside — treat **"rebuild OS from Postgres" as a
*tested* DR drill**. The migration session already proved rebuild-from-source
works; formalize it into a documented, exercised "nuke OS, re-project from PG in
minutes" runbook. It's both the fastest recovery and the migration mechanism.

### 13.6 Testing — testcontainers-go

Formalize the real-stack validation (spin up Postgres + OpenSearch + MinIO in
integration tests via **testcontainers-go**) so the subtle parts — outbox
projection, reconciliation, tenant isolation — are tested against the real engines,
not mocks. Exactly the schema validation already done, made permanent.

### 13.7 Smaller items

- **Chunking** for retrieval — records are embedded whole; long PDFs/docs retrieve
  better chunked (sibling to the reranking point in §6).
- **Degraded-mode behavior** — specify per-component failure: OS down (writes still
  append to PG, project later), Ollama down, PG down. The online design needs
  *explicit* failure semantics where the old design had offline resilience.
- **Deletion / right-to-be-forgotten** — immutable content-addressed records make
  "delete everything about X" genuinely hard. Fine for a personal brain; a real
  question for the GSA tenant. Define a hard-delete path that spans records + facts
  + blobs + index.

## Appendix — one-paragraph summary

Separate **truth** from **search**: Postgres + MinIO are the durable System of
Record (Postgres holds structured truth + synthesized output + version history and
points at MinIO blobs); OpenSearch is a rebuildable hybrid-search index projected
from it. Model knowledge with **two identities** — immutable content-addressed
**records** that just accrue and dedup, and a referent-keyed **mutable-state
projection** derived from them via scheduled reconciliation. **Append fast,
synthesize async, reconcile on a schedule, project to OpenSearch for finding.**
Adopt infra for the commodity parts (queue, vector store, hybrid search, blob
storage, load balancing); keep custom code only for the memory semantics —
synthesis and reconciliation — that are genuinely yours.
