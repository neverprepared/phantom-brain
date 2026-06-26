-- entities.sql — canonical entities, aliases, and the record↔entity join.
-- The forward edge (record → entities) is record_entities; the reverse
-- ("what mentions X") is just a query over it — no array read-modify-write.

-- name: UpsertEntity :one
-- Canonical-by-slug. A repeat upsert refreshes name and only overwrites the
-- description when a non-NULL one is supplied (COALESCE keeps the old blurb).
INSERT INTO entities (
    profile, vault, slug, name, description,
    embedding, embedding_model, embedding_version
) VALUES (
    @profile, @vault, @slug, @name, @description,
    @embedding, @embedding_model, @embedding_version
)
ON CONFLICT (profile, vault, slug) DO UPDATE SET
    name        = EXCLUDED.name,
    description = COALESCE(EXCLUDED.description, entities.description)
RETURNING *;

-- name: GetEntityBySlug :one
SELECT * FROM entities
WHERE profile = @profile AND vault = @vault AND slug = @slug;

-- name: AddEntityAlias :exec
-- Alternate names that resolve to the same entity (renames, "Bob"↔"Robert").
INSERT INTO entity_aliases (entity_id, alias)
VALUES (@entity_id, @alias)
ON CONFLICT (entity_id, alias) DO NOTHING;

-- name: ResolveEntityByAlias :one
-- Find the canonical entity for an alias within a tenant.
SELECT e.* FROM entities e
JOIN entity_aliases a ON a.entity_id = e.id
WHERE e.profile = @profile AND e.vault = @vault AND a.alias = @alias;

-- name: LinkRecordEntity :exec
-- Forward edge: this record mentions this entity.
INSERT INTO record_entities (record_id, entity_id)
VALUES (@record_id, @entity_id)
ON CONFLICT (record_id, entity_id) DO NOTHING;

-- name: RecordsMentioningEntity :many
-- Reverse backlink (the mentioned_by[] replacement): every record that
-- mentions the entity identified by slug, within a tenant.
SELECT r.* FROM records r
JOIN record_entities re ON re.record_id = r.id
JOIN entities e         ON e.id = re.entity_id
WHERE e.profile = @profile AND e.vault = @vault AND e.slug = @slug
ORDER BY r.id;
