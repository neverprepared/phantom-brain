-- 0005_capture_columns (down) — drop the capture pointer columns.
ALTER TABLE records DROP COLUMN capture_size_bytes;
ALTER TABLE records DROP COLUMN capture_minio_key;
