DROP INDEX IF EXISTS entities_embed_model_idx;
DROP INDEX IF EXISTS records_embed_model_idx;
ALTER TABLE entities DROP COLUMN IF EXISTS embedding_version;
ALTER TABLE entities DROP COLUMN IF EXISTS embedding_model;
ALTER TABLE records  DROP COLUMN IF EXISTS embedding_version;
ALTER TABLE records  DROP COLUMN IF EXISTS embedding_model;
