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
