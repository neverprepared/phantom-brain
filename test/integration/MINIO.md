# MinIO smoke test runbook

The Go unit suite covers the MinIO backend's surface (constructor validation, URL signing, state-keeping) but can't prove the wire-level integration without a real bucket. This runbook covers the one-time setup + the gated end-to-end test that does.

## Prerequisites

- `mc` (MinIO client) on `$PATH`
- Reachable MinIO endpoint with admin or bucket-owner creds you can issue a scoped access key from
- This repo built (`make build`)

## Step 1 — Create the bucket

```bash
# point mc at the operator's MinIO
mc alias set np https://minio.neverprepared.com "$ADMIN_ACCESS" "$ADMIN_SECRET"

# create the bucket — name it whatever you'll put in server.toml
mc mb np/phantom-brain

# (recommended) lifecycle rule: expire orphaned uploads after 1 day
mc ilm rule add --expire-days 1 --filter '*/_uploads/*' np/phantom-brain
```

## Step 2 — Issue a scoped access key

The daemon needs `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`, and (for the smoke test only) `s3:ListBucket` on the target bucket. Anything broader is unnecessary.

Minimal policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:GetObject",
        "s3:DeleteObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::phantom-brain",
        "arn:aws:s3:::phantom-brain/*"
      ]
    }
  ]
}
```

```bash
mc admin policy create np phantom-brain-rw policy.json
mc admin user add np pb-daemon "$(openssl rand -hex 16)"
mc admin policy attach np phantom-brain-rw --user pb-daemon
```

Capture the access key + secret you just minted.

## Step 3 — Run the gated smoke test

```bash
export MINIO_INTEGRATION=1
export MINIO_ENDPOINT=minio.neverprepared.com
export MINIO_BUCKET=phantom-brain
export MINIO_ACCESS_KEY=...     # from step 2
export MINIO_SECRET_KEY=...     # from step 2
export MINIO_USE_SSL=true       # default; set to false for http endpoints

go test -tags=sqlite_fts5 -v -run TestMinIO_AgentDaemonSmoke ./test/integration/
```

The test exercises:

1. `pbrainctl serve` boots with `[storage] backend = "minio"` pointed at the bucket
2. Agent births greenfield (daemon has no snapshot yet), seeds a Raw markdown file in its brain dir
3. Agent shuts down → death payload landed in local `_pending/`
4. `UploadShipQueue` calls `/merge/init` → daemon presigns S3 PUT URL → agent PUTs the tarball **directly to MinIO** → `/merge/complete` → daemon GETs from MinIO → atomic-renames into `brains/_pending/<brain_id>.tar` → best-effort deletes the upload from MinIO
5. `ReapOnce` merges, `SynthesizeOne` writes Wiki
6. Asserts: ledger row present, Wiki populated, bucket has zero leftover `_uploads/*` objects

## What to do when it fails

The test logs the failure step clearly. Common diagnosis:

| Symptom | Likely cause |
|---|---|
| `BucketExists … 403 Access Denied` | Access key missing `s3:ListBucket`. Add it (the daemon doesn't need it but the preflight does). |
| `daemon Start: minio backend requires …` | A required `[storage]` field in `server.toml` is empty. |
| `Start … x509: certificate signed by unknown authority` | Custom CA in front of MinIO. Either install the root CA on the host or use `minio_use_ssl = false` against a plaintext endpoint. |
| `ship complete … shipped=[] failed=[…]` | Look at the failure: typically a presigned PUT 403 (key lacks `s3:PutObject`) or DNS failure on the presigned host. |
| `bucket has N leftover _uploads/ objects` | RemoveObject silently failing — key lacks `s3:DeleteObject`. Non-fatal but worth fixing so the lifecycle rule isn't your only cleanup. |
| `expected 1 ledger row, got 0` | Reaper rejected the tar. Check the daemon's stderr for `safetar:` errors. |

## Cleanup

```bash
mc rm --recursive --force np/phantom-brain/smoketest/
# or just drop the whole bucket if it's dedicated to testing:
mc rb --force np/phantom-brain
```

The test writes to MinIO under `smoketest/memory/_uploads/` so it won't collide with real `personal/memory` traffic on the same bucket.

## Why the test is gated

`MINIO_INTEGRATION=1` keeps it out of CI. CI has no bucket and no creds; building a fake MinIO is more work than the test is worth. The harness is `_test.go` so it ships with the source — operators run it once during setup + after any change to `internal/server/storage.go`.
