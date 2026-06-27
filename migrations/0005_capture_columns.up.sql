-- 0005_capture_columns — raw-source capture pointer on the records SoR.
--
-- The synth pipeline can fetch the page bytes behind a record's source_url
-- and stash them in MinIO (best-effort, gated on [capture]). Phase D1 had
-- nowhere to persist the resulting key, so it was logged and dropped —
-- handleCaptureGet could never find post-cutover captures. These columns
-- give the SoR a home for the capture pointer + its size.

ALTER TABLE records ADD COLUMN capture_minio_key text;
ALTER TABLE records ADD COLUMN capture_size_bytes bigint;

COMMENT ON COLUMN records.capture_minio_key IS
    'MinIO key of the raw page bytes captured at synth time. NULL when capture is off, the source_url is absent, or the fetch failed.';
COMMENT ON COLUMN records.capture_size_bytes IS
    'Size in bytes of the captured raw-source blob. NULL when no capture was stored.';
