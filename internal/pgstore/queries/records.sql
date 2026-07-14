-- records.sql — the immutable content-addressed knowledge log.
-- Dedup is by (profile, vault, sha): a conflict means "already have it".

-- name: UpsertRecord :one
-- Content-addressed insert carrying the agent-computed embedding so kNN /
-- semantic recall works off the raw write (the synth pass later overwrites
-- it with the canonical embedding via MarkRecordSynthesised). ON CONFLICT
-- DO UPDATE backfills a previously-NULL embedding on re-ingest WITHOUT
-- clobbering an existing one (or any other field) — COALESCE keeps the
-- stored value when present. Unlike DO NOTHING, DO UPDATE RETURNS the row
-- on conflict too, so callers always get the record back.
INSERT INTO records (
    profile, vault, sha, kind, memory_type,
    title, raw_body, source_url, source, tags, captured_at,
    minio_key, mime_type, size_bytes, original_filename, embedding
) VALUES (
    @profile, @vault, @sha, @kind, @memory_type,
    @title, @raw_body, @source_url, @source, @tags, @captured_at,
    @minio_key, @mime_type, @size_bytes, @original_filename, @embedding
)
ON CONFLICT (profile, vault, sha) DO UPDATE SET
    embedding = COALESCE(records.embedding, EXCLUDED.embedding)
RETURNING *;

-- name: GetRecordBySHA :one
SELECT * FROM records
WHERE profile = @profile AND vault = @vault AND sha = @sha;

-- name: GetRecordByID :one
SELECT * FROM records WHERE id = @id;

-- name: DeleteRecordBySHA :one
-- The brain_forget primitive (issue #72): delete one record by its
-- content-addressed identity, RETURNING its id so the caller can enqueue
-- a projection delete in the same tx. Returns pgx.ErrNoRows when the SHA
-- isn't present — the handler reports forgotten=false rather than lying.
DELETE FROM records
WHERE profile = @profile AND vault = @vault AND sha = @sha
RETURNING id;

-- name: ListUnsynthesised :many
-- The resynth scan: records still awaiting the gate + distill + embed pass.
-- Rows may carry a NULL embedding, exercising the nullable vector override.
SELECT * FROM records
WHERE profile = @profile AND vault = @vault AND NOT synthesised
ORDER BY id
LIMIT @lim;

-- name: CountUnsynthesised :one
SELECT count(*) FROM records
WHERE profile = @profile AND vault = @vault AND NOT synthesised;

-- name: MarkRecordSynthesised :exec
-- Fill in the derived fields after the synthesis pipeline runs.
-- updated_at is bumped by the records_set_updated_at trigger.
UPDATE records SET
    body               = @body,
    reliability        = @reliability,
    topic              = @topic,
    gate_reason        = @gate_reason,
    synthesised        = true,
    embedding          = @embedding,
    embedding_model    = @embedding_model,
    embedding_version  = @embedding_version,
    capture_minio_key  = @capture_minio_key,
    capture_size_bytes = @capture_size_bytes
WHERE id = @id;

-- name: SetRecordExtractedText :exec
-- Attachment enrichment: store OCR / pdftotext / office-extract output.
UPDATE records SET extracted_text = @extracted_text WHERE id = @id;

-- name: ListRecords :many
-- Mart projection scan (pbrainctl mart): keyset-paginated enumeration of a
-- tenant's records with optional facet filters. This is the generic "list
-- records" read the resynth-only ListUnsynthesised never provided; the core
-- stays ignorant of marts — a mart is just a consumer of this + the HTTP
-- endpoint over it.
--
-- Array filters use the coalesce(array_length(...),0)=0 guard rather than a
-- bare IS NULL so a nil []string param cleanly means "no filter": pgx encodes
-- a nil slice as an empty array (not SQL NULL), so an IS NULL guard would
-- never fire and an empty filter would wrongly match nothing. tags/source use
-- the array-overlap operator && ("carries ANY of these"), GIN-accelerated by
-- records_tags_gin / records_source_gin; kind/topic/reliability use = ANY.
-- The id > @after_id keyset walks the PK deterministically; @lim bounds it.
SELECT * FROM records
WHERE profile = @profile
  AND vault   = @vault
  AND synthesised = @synthesised
  AND (coalesce(array_length(@kinds::text[], 1), 0) = 0         OR kind = ANY(@kinds::text[]))
  AND (coalesce(array_length(@topics::text[], 1), 0) = 0        OR topic = ANY(@topics::text[]))
  AND (coalesce(array_length(@reliabilities::text[], 1), 0) = 0 OR reliability = ANY(@reliabilities::text[]))
  AND (coalesce(array_length(@tags_any::text[], 1), 0) = 0      OR tags && @tags_any::text[])
  AND (coalesce(array_length(@source_any::text[], 1), 0) = 0    OR source && @source_any::text[])
  AND id > @after_id
ORDER BY id
LIMIT @lim;

-- name: ListRecordsSince :many
-- Change feed for incremental mart refresh (pbrainctl mart sync). Same facet
-- filters as ListRecords, but ordered by the records_set_updated_at-maintained
-- updated_at so a caller can ask "what changed since my cursor". The cursor is
-- COMPOUND — (updated_at, id) — because many rows can share an updated_at
-- (a batch synth pass); keyset `updated_at > @since OR (updated_at = @since AND
-- id > @after_id)` neither skips nor duplicates across page boundaries the way
-- a bare `updated_at > @since` would. Deletes are NOT visible here (a forgotten
-- row simply stops appearing) — pruning is a periodic full rebuild's job.
SELECT * FROM records
WHERE profile = @profile
  AND vault   = @vault
  AND synthesised = @synthesised
  AND (coalesce(array_length(@kinds::text[], 1), 0) = 0         OR kind = ANY(@kinds::text[]))
  AND (coalesce(array_length(@topics::text[], 1), 0) = 0        OR topic = ANY(@topics::text[]))
  AND (coalesce(array_length(@reliabilities::text[], 1), 0) = 0 OR reliability = ANY(@reliabilities::text[]))
  AND (coalesce(array_length(@tags_any::text[], 1), 0) = 0      OR tags && @tags_any::text[])
  AND (coalesce(array_length(@source_any::text[], 1), 0) = 0    OR source && @source_any::text[])
  AND (updated_at > @since OR (updated_at = @since AND id > @after_id))
ORDER BY updated_at, id
LIMIT @lim;
