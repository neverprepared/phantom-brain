-- 0004_embedding_model — version every vector by the model that produced it.
--
-- embedding vector(768) hardcodes nomic-embed-text. Without recording WHICH
-- model made each vector, a model change silently mixes incompatible vector
-- spaces and degrades kNN. Tracking model+version makes a swap an INCREMENTAL,
-- detectable re-embed:
--   WHERE embedding IS NULL OR embedding_model IS DISTINCT FROM '<target>'
-- (Dimension is still fixed at 768; a different-dim model needs an ALTER +
-- full re-embed — but at least you can SEE the mismatch.)

ALTER TABLE records  ADD COLUMN embedding_model   text;
ALTER TABLE records  ADD COLUMN embedding_version text;
ALTER TABLE entities ADD COLUMN embedding_model   text;
ALTER TABLE entities ADD COLUMN embedding_version text;

CREATE INDEX records_embed_model_idx  ON records  (profile, vault, embedding_model);
CREATE INDEX entities_embed_model_idx ON entities (profile, vault, embedding_model);
