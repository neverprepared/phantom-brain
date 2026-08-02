-- Revert 0007: drop the verbatim kinds (skill/todo/session) from the CHECK.
-- Any existing rows with these kinds must be removed/retyped first or the
-- ADD CONSTRAINT will fail.
ALTER TABLE records DROP CONSTRAINT records_kind_chk;
ALTER TABLE records ADD CONSTRAINT records_kind_chk CHECK (kind IN
    ('note','web_scrape','task_summary','attachment','email_import','manual_curate'));
