-- facts.sql — mutable state: the referent-keyed projection.
-- (entity, attribute) is unique: the CURRENT value is one row you UPSERT;
-- the prior value moves to fact_history (versioned, never destructive).

-- name: GetFact :one
SELECT * FROM facts
WHERE entity_id = @entity_id AND attribute = @attribute;

-- name: UpsertFact :one
-- Set the current value for (entity, attribute). On conflict, overwrite the
-- value, provenance, and confidence (caller archives the old value into
-- fact_history first via InsertFactHistory).
INSERT INTO facts (
    profile, vault, entity_id, attribute, value, source_record_id, confidence
) VALUES (
    @profile, @vault, @entity_id, @attribute, @value, @source_record_id, @confidence
)
ON CONFLICT (entity_id, attribute) DO UPDATE SET
    value            = EXCLUDED.value,
    source_record_id = EXCLUDED.source_record_id,
    confidence       = EXCLUDED.confidence
RETURNING *;

-- name: InsertFactHistory :exec
-- Append a superseded fact value into the immutable history.
INSERT INTO fact_history (
    profile, vault, entity_id, attribute, value,
    source_record_id, valid_from, superseded_by_record_id
) VALUES (
    @profile, @vault, @entity_id, @attribute, @value,
    @source_record_id, @valid_from, @superseded_by_record_id
);

-- name: ListFactsForEntity :many
SELECT * FROM facts
WHERE entity_id = @entity_id
ORDER BY attribute;
