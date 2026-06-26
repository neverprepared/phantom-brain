# Postgres migrations — System of Record schema

Schema for the OpenSearch-native redesign (epic #92, design doc
`docs/design/opensearch-native-memory-architecture.md`). Postgres is the
**System of Record**; OpenSearch is a derived index projected from these
tables; MinIO holds attachment bytes (referenced by `records.minio_key`).

## Convention

Plain numbered SQL with `golang-migrate`-style up/down pairs
(`NNNN_name.up.sql` / `NNNN_name.down.sql`). Tool-agnostic — run with
[`golang-migrate`](https://github.com/golang-migrate/migrate), `goose`, or
raw `psql` (in order). Each file is idempotent-safe where practical
(`IF NOT EXISTS`), but they're meant to run once, in sequence.

```sh
# Recommended: provision a profile DB (create db + extensions + migrate) in
# one step. --dsn points at the MAINTENANCE db; per-profile db is derived.
pbrainctl server db provision <profile> --dsn postgres://pbrain:***@localhost:5433/phantom_brain
pbrainctl server db migrate   <profile>     # migrate-only (db already exists)

# Or directly with golang-migrate against a specific profile DB:
migrate -path migrations -database "pgx5://…/pb_<profile>" up
```

The embedded copy of these files (`migrations/embed.go`) is what
`pbrainctl server db ...` applies, so the binary always migrates the schema
it shipped with.

Requires the `vector` and `pg_trgm` extensions (the compose pgvector
image enables them on init).

## The model (two identities)

| Layer | Tables | Identity | Mutability |
|---|---|---|---|
| **Records** | `records` | content (SHA, dedup) | immutable — append + synth-fill |
| **Entities** | `entities`, `entity_aliases`, `record_entities` | canonical id + aliases | identity survives rename |
| **State** | `facts`, `fact_history` | referent (entity, attribute) | upsert + versioned history |

- **Records** = the durable log (episodic events, raw + synthesized
  content, attachment metadata + embedding). "What mentions entity X" is a
  join over `record_entities`, **not** a denormalized backlink.
- **Facts** = the referent-keyed mutable projection. `UNIQUE(entity,
  attribute)` → upsert; the prior value moves to `fact_history`
  (`valid_from`/`valid_to`). This is the layer reconciliation maintains.

## Migrations

| # | Adds |
|---|---|
| 0001 | `records` (+ `set_updated_at()` trigger helper) |
| 0002 | `entities`, `entity_aliases`, `record_entities` |
| 0003 | `facts`, `fact_history` |
| 0004 | `embedding_model` + `embedding_version` on records/entities (versioned vectors) |

## Tenant isolation

Per the design (§13.4): the SoR is isolated **per profile** to match the existing
dedicated-MinIO-bucket + OpenSearch-token boundary. The recommended deployment runs
**a Postgres database per profile** (`pb_personal`, `pb_gsa`, …), so these
migrations are applied **once per profile DB**. `(profile, vault)` columns remain
on every table (vault is a sub-scope within a profile's DB, and the columns keep
the schema portable if you ever consolidate). Run migrations against each profile's
`DATABASE_URL`.

## Open decisions (intentionally deferred)

- Row-level security as an *additional* guard within a profile DB (defense in
  depth) — columns + per-DB isolation for now.
- `references[]` between records — modeled as a join later if needed.
- Embedding dim is fixed at **768** (`nomic-embed-text`); a model change
  means a re-embed + an `ALTER`.
- `id` is `bigint IDENTITY` (single-cluster). Switch to UUID only if/when
  cross-system id generation matters.
