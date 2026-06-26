-- Runs once on first init of an empty Postgres data dir (the official
-- image executes *.sql in /docker-entrypoint-initdb.d alphabetically).
--
-- Enables pgvector so the System of Record can do embedding-similarity
-- (entity resolution, near-duplicate reconciliation) without a round
-- trip to OpenSearch. See docs/design/opensearch-native-memory-architecture.md.
--
-- Schema (records / state+history / entities / joins) is intentionally
-- NOT defined here — that belongs in versioned application migrations,
-- not a one-shot init script. This file only ensures the extension.

CREATE EXTENSION IF NOT EXISTS vector;

-- pg_trgm is handy for fuzzy text matching in entity resolution
-- (alias/name similarity) alongside vector similarity.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
