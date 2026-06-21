# Seeding an existing vault into a v5.0 daemon

Use this when you have a working vault on disk (TS-era or any v5.0 vault from another host) and you want to start a brand-new `pbrainctl serve` daemon with that content already populated.

## What the seed does — and doesn't

**Does:** direct-copy `Wiki/` + `Raw/` + `_index/` from the source vault into the daemon's `collective/vault/` on the daemon host. Builds an initial snapshot so brains can birth from it.

**Doesn't:** re-run the synthesis pipeline. The Wiki you have is the Wiki the daemon starts with — every summary + entity page is preserved as-is. If you want the gate + LLM to re-evaluate every source, use `pbrainctl migrate-legacy` instead and ship the resulting brain through the daemon (slower + cost LLM calls).

**MinIO's role:** transit only. The seed tarball lives briefly under `<bucket>/<profile>/<vault>/seed/` while you transfer between hosts. The daemon's canonical vault still ends up on its own local disk; MinIO holds death-payload uploads during normal operation.

## Prerequisites

- Source host has the existing vault on disk + `mc` configured with the MinIO alias
- Daemon host has `pbrainctl` installed (built from this repo) + `mc` + a populated `$PHANTOM_BRAIN_CONFIG_DIR` with `server.toml` + `profiles/<profile>/vaults/<vault>/auth.toml`
- MinIO bucket exists and the configured access key can `PutObject` + `GetObject` + `ListBucket` under `<bucket>/<profile>/<vault>/seed/`

## Two-phase invocation

```bash
# === phase 1: pack — run on the host with the existing vault ===
./scripts/seed-vault-via-minio.sh pack \
    --vault   /Users/you/obsidian/vaults/personal-memory \
    --bucket  testbucket \
    --prefix  personal/memory

# Outputs progress per subdir (Wiki + Raw + _index). Resumable via
# mc mirror's incremental sync — safe to re-run on a partial upload.

# === phase 2: apply — run on the daemon host ===
./scripts/seed-vault-via-minio.sh apply \
    --bucket   testbucket \
    --prefix   personal/memory \
    --data-dir /var/lib/phantom-brain

# Pulls from the bucket → daemon's collective vault, then runs
# `pbrainctl snapshot rebuild` to produce gen=1.
```

Verify:

```bash
pbrainctl snapshot status personal/memory --data-dir /var/lib/phantom-brain
pbrainctl vault status   --data-dir /var/lib/phantom-brain
```

## What the script refuses to do

- **Pack** without `Wiki/` + `Raw/` in the source — guards against pointing at a non-vault directory by mistake.
- **Apply** when the target collective vault already has a non-empty `Wiki/` — refuses to clobber a vault that's in use. Manually remove `<data-dir>/<profile>/<vault>/collective/vault/` first if you really want to re-seed.

## Cleanup after a successful seed

```bash
mc rm --recursive --force np/<bucket>/<profile>/<vault>/seed/
```

The seed bucket prefix isn't part of the daemon's normal operation — leaving it costs storage with no upside. If your bucket has a lifecycle rule (recommended: `--expire-days 7 --filter '*/seed/*'`), this happens automatically.

## When NOT to use this script

- **First daemon you've ever run** — easier to start empty + ingest from scratch via `brain_perceive` / `brain_learn` from the agent side.
- **You want a fresh re-synthesis** — use `pbrainctl migrate-legacy` instead. The TS-era vault becomes a brain dir, the brain dies, the death payload ships to the daemon, the daemon re-runs the gate + LLM on every source. Expensive but produces a clean Wiki.
- **You have read-write access to both hosts' filesystems already** — skip MinIO entirely:
  ```bash
  rsync -av /local/vault/{Wiki,Raw,_index}/ daemon-host:/var/lib/phantom-brain/<profile>/<vault>/collective/vault/
  ssh daemon-host pbrainctl snapshot rebuild <profile>/<vault>
  ```
