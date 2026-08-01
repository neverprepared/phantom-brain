-- 0007_synth_failure_tracking — surface synth-pipeline failures on the SoR.
--
-- processJob failures were previously only logged at Warn: the record stayed
-- synthesised=false and the ~30s sweeper re-drained it FOREVER (a poison
-- record = a permanent 30s LLM hot-loop). These columns give the SoR a place
-- to record the failure so the sweeper can dead-letter a repeatedly-failing
-- record (synth_attempts >= maxSynthAttempts) instead of re-processing it, and
-- so `pbrainctl server queue depth` + /metrics can report the dead backlog.
--
-- The partial index backs the keyset backlog scan (ListSynthBacklog): the
-- sweeper walks profile/vault/id over the NOT-synthesised rows only, so the
-- index stays small (it drops synthesised rows entirely).

ALTER TABLE records
    ADD COLUMN synth_attempts   integer NOT NULL DEFAULT 0,
    ADD COLUMN last_synth_error text,
    ADD COLUMN synth_failed_at  timestamptz;

CREATE INDEX IF NOT EXISTS records_synth_backlog_idx
    ON records (profile, vault, id) WHERE NOT synthesised;

COMMENT ON COLUMN records.synth_attempts IS
    'Count of failed synth attempts. Reaching maxSynthAttempts dead-letters the record (excluded from the sweeper backlog scan).';
COMMENT ON COLUMN records.last_synth_error IS
    'Last synth failure message. NULL until a synth attempt fails; cleared by resynth --apply.';
COMMENT ON COLUMN records.synth_failed_at IS
    'Timestamp of the last synth failure. NULL until a synth attempt fails.';
