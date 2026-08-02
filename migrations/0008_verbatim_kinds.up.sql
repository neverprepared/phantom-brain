-- 0007_verbatim_kinds — allow authored, verbatim record kinds.
--
-- skill / todo / session are authored content, not raw sources to distill:
-- the synth worker persists raw_body AS the body with no LLM gate/distill
-- pass (see internal/server/synth_queue.go processJob verbatim branch, and
-- osearch.Kind.IsVerbatim). They back the centralized skills/todo/sessions
-- vaults folded into phantom-brain.
ALTER TABLE records DROP CONSTRAINT records_kind_chk;
ALTER TABLE records ADD CONSTRAINT records_kind_chk CHECK (kind IN
    ('note','web_scrape','task_summary','attachment','email_import','manual_curate',
     'skill','todo','session'));
