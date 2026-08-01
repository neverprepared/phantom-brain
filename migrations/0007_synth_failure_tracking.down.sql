DROP INDEX IF EXISTS records_synth_backlog_idx;

ALTER TABLE records
    DROP COLUMN IF EXISTS synth_failed_at,
    DROP COLUMN IF EXISTS last_synth_error,
    DROP COLUMN IF EXISTS synth_attempts;
