-- 0006_records_changefeed_idx — support the incremental mart change feed.
--
-- ListRecordsSince (pbrainctl mart sync) filters by (profile, vault) and walks
-- a compound (updated_at, id) keyset ordered by updated_at, id. This index
-- makes that a range scan instead of a tenant scan + sort. The trailing id
-- keeps the keyset tie-break (many rows can share an updated_at from one synth
-- batch) index-ordered too.
CREATE INDEX records_changefeed_idx ON records (profile, vault, updated_at, id);
