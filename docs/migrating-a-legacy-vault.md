# Migrating a legacy brain vault into a new platform brain

How to move a profile's accumulated memory + attachments from an **older
standalone phantom-brain** (the `<profile>_pb_records` single-index generation,
e.g. a `pbrain-*` docker stack) into a **new platform brain** (the per-vault
`pb_<profile>_<vault>_` generation) — **without losing knowledge or the MinIO
links to attachments**.

This is a **logical re-ingest**, not a byte copy: the daemon re-derives the
canonical SHA, re-embeds locally, and rebuilds the per-vault projection. It is
idempotent (safe to re-run) and leaves the **source untouched** (read-only), so
the old brain remains a fallback until you cut over.

## Why not a simpler path

- **Index layout differs.** Old brain = one `<profile>_pb_records` index tagged
  with a `vault` field; new brain = one index per vault
  (`pb_<profile>_<vault>_pb_records`) fronting a Postgres System-of-Record.
- **Embeddings aren't in `_source`.** A plain OpenSearch reindex loses the
  vectors, so the target must re-embed (cheap — local Ollama `nomic-embed-text`).
- **`backfill-to-pg` won't help** if the source predates the `pb_summaries`/SoR
  split — it reads that newer legacy shape, not `<profile>_pb_records`.
- **`/learn` rejects `kind: attachment`** (closed enum). Attachments must go
  through the daemon's *attachment* path, not the record path → **two passes**.

## Prerequisites

- Read access to the **source** OpenSearch (`_search`) and MinIO (root creds).
- The **target** brain daemon reachable (`$CL_BRAIN_API`, default
  `http://localhost:9998`) and the destination vault's **bearer token**
  (`config/phantom-brain/profiles/<profile>/vaults/<vault>/auth.toml`).
- The target profile/vault already provisioned (`pbrainctl server db provision
  <profile>` + the vault binding exists).
- `python3`, `docker`, and a way to run `mc` (the `minio/mc` image).
- Helper scripts: [`scripts/migrate/build_export.py`](../scripts/migrate/build_export.py),
  [`scripts/migrate/build_attachments.py`](../scripts/migrate/build_attachments.py).

Below, `SRC` = source host (old brain), `DST` = new platform host. `PROFILE`
and `VAULT` are the profile and vault being moved (e.g. `personal` / `memory`).
`SRC_INDEX` is the legacy index (e.g. `personal_pb_records`).

## Pass 0 — dump the source records

```bash
# All records (raise size if you have >10k; else use the scroll API)
docker exec pbrain-opensearch curl -s \
  "localhost:9200/${SRC_INDEX}/_search?size=10000" > allrecords.json
python3 -c "import json;print(len(json.load(open('allrecords.json'))['hits']['hits']),'records')"
```

## Pass 1 — text records (notes, web_scrapes, emails, summaries)

Build the export dir (frontmatter + raw body per record) and import. `/learn`
re-embeds each and derives the canonical SHA (idempotent).

```bash
python3 scripts/migrate/build_export.py allrecords.json ./export      # writes export/records/*.md
docker cp ./export  <brain-container>:/tmp/import
TOKEN=$(python3 -c "import tomllib;print(tomllib.load(open('.../vaults/${VAULT}/auth.toml','rb'))['bearer_token'])")
docker exec -e TOK="$TOKEN" <brain-container> \
  pbrainctl client import /tmp/import --api http://localhost:9998 --token "$TOK"
# -> "import done: records ok=<N> failed=<M>"
```

`kind: attachment` rows **fail here on purpose** (`unknown kind: attachment`) —
they are handled in Pass 2. That's the expected bulk of `failed`.

## Pass 2 — attachments (the blobs + their links)

Attachments are `sha`-keyed blobs. The source keys them as
`<PROFILE>/<VAULT>/attachments/<sha>.<ext>` in its vault bucket; the target uses
the **same key structure** in the `phantom-platform` bucket.

1. **Mirror the blobs** off the source MinIO (root creds read via `printenv` to
   avoid mangling special chars):

   ```bash
   U=$(docker exec <src-minio> printenv MINIO_ROOT_USER | tr -d '\r\n')
   P=$(docker exec <src-minio> printenv MINIO_ROOT_PASSWORD | tr -d '\r\n')
   IP=$(docker inspect <src-minio> --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' | head -1)
   NET=$(docker inspect <src-minio> --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' | head -1)
   mkdir -p ~/blob-staging
   docker run --rm --network "$NET" --entrypoint sh \
     -e U="$U" -e P="$P" -e IP="$IP" -v $HOME/blob-staging:/out minio/mc:latest \
     -c 'mc alias set m http://$IP:9000 $U $P >/dev/null;
         mc mirror --quiet m/<SRC_BUCKET>/'"$PROFILE"'/'"$VAULT"'/attachments /out'
   ```

   Copy `~/blob-staging` to the target host if they differ (`rsync`).

2. **Build + import the attachment package** — pairs each blob with metadata
   (mime/filename/tags/extracted_text) from the dump; the daemon uploads the
   blob to the target MinIO and links it (`minio_key`) to a new attachment doc:

   ```bash
   python3 scripts/migrate/build_attachments.py ~/blob-staging allrecords.json ./att-export
   docker cp ./att-export <brain-container>:/tmp/att-import
   docker exec -e TOK="$TOKEN" <brain-container> \
     pbrainctl client import /tmp/att-import --api http://localhost:9998 --token "$TOK"
   # -> "attachments ok=<N> failed=0"
   ```

## Verify

```bash
# every attachment record should have a minio_key; totals should match the source
docker exec <pg> psql -U phantom -d pb_<PROFILE> -tAc \
  "select count(*) total, count(minio_key) with_blob from records where vault='${VAULT}'"

# blobs actually present in the target bucket (count should equal attachment count)
mc ls --recursive m/phantom-platform/${PROFILE}/${VAULT}/attachments | wc -l

# recall returns migrated knowledge
curl -s -X POST http://localhost:9998/api/brain/recall \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"query":"<something you know is in there>","k":3}' | jq '.hits[].title'
```

## Cut over

Only after verifying: point your `brain_*` client at the new daemon — set
`CL_BRAIN_API` (and `CL_BRAIN_API_TOKEN` to the destination vault token) to the
new host. Keep the old brain running read-only until you're satisfied, then
retire it.

## Notes & gotchas

- **Idempotent.** Re-running either pass is a SHA-deduped no-op for rows already
  present, so a partial run is safe to resume.
- **The source is never written to** — it stays a fallback.
- **`kind`s accepted by `/learn`** are a closed enum (`note`, `web_scrape`,
  `email_import`, `task_summary`, …). Rows with a kind the target rejects are
  reported in `failed` and simply skipped — check the import log if the count is
  higher than the attachment count.
- **`mc` creds:** use the **root** creds via `printenv` inside the MinIO
  container; a service-account `minio_secret_key` from `server.toml` may not
  authorize bucket listing.
- **Attachments carry `extracted_text`** as their searchable body, so the
  knowledge is present even before the async synth pass runs.
