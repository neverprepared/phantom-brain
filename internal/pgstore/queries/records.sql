-- records.sql — the immutable content-addressed knowledge log.
-- Dedup is by (profile, vault, sha): a conflict means "already have it".

-- name: UpsertRecord :one
-- Content-addressed insert. ON CONFLICT DO NOTHING returns no row when the
-- (profile, vault, sha) already exists — callers fall back to GetRecordBySHA.
INSERT INTO records (
    profile, vault, sha, kind, memory_type,
    title, raw_body, source_url, source, tags, captured_at,
    minio_key, mime_type, size_bytes, original_filename
) VALUES (
    @profile, @vault, @sha, @kind, @memory_type,
    @title, @raw_body, @source_url, @source, @tags, @captured_at,
    @minio_key, @mime_type, @size_bytes, @original_filename
)
ON CONFLICT (profile, vault, sha) DO NOTHING
RETURNING *;

-- name: GetRecordBySHA :one
SELECT * FROM records
WHERE profile = @profile AND vault = @vault AND sha = @sha;

-- name: GetRecordByID :one
SELECT * FROM records WHERE id = @id;

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
    body              = @body,
    reliability       = @reliability,
    topic             = @topic,
    gate_reason       = @gate_reason,
    synthesised       = true,
    embedding         = @embedding,
    embedding_model   = @embedding_model,
    embedding_version = @embedding_version
WHERE id = @id;

-- name: SetRecordExtractedText :exec
-- Attachment enrichment: store OCR / pdftotext / office-extract output.
UPDATE records SET extracted_text = @extracted_text WHERE id = @id;
