# Validate the MinIO-backed brain end-to-end

Switches your active Claude Code MCP from legacy mode (`BRAIN_VAULT_PATH` reading a local Obsidian vault) to v5.0 agent-contract mode (`CL_BRAIN_*` talking to a daemon that uses MinIO for death-payload transit).

Prereqs already done (from prior sessions):
- MinIO running at `minio.neverprepared.com`, bucket `obsidian-vaults` populated with `personal-memory/` synced from local
- Phase 5 MinIO backend code on `main`
- `pbrainctl` build target works (`make build` in this repo)

## Overview

```
LAPTOP                                  REMOTE HOST                MinIO
──────                                  ───────────                ─────
.claude.json CL_BRAIN_API=remote ──▶  pbrainctl serve  ◀──────  testbucket
       │                                   │   ▲                obsidian-vaults
       ▼                                   │   │
pbrainctl mcp                              │   │   reaps from
(per Claude Code session)                  │   └── MinIO _uploads/
       │                                   │
       │ POST /merge/init                  ▼
       │ presigned URL ◀────────────  S3 PUT direct
       │ POST /merge/complete  ────▶  daemon GETs from MinIO
       │                                   │
       │                                   ▼
       │                              reaper + synthesizer
       └─── GET /snapshot/current ◀── /published/snapshot-N.tar.zst
```

## Step 1 — On the REMOTE host: install + bootstrap

```bash
# Get the binary onto the remote host. Easiest path: build locally,
# scp over. Or build from source on the remote (Go 1.26+ required).
scp ./pbrainctl  remote:/usr/local/bin/pbrainctl
ssh remote 'pbrainctl server version'

# Run the bootstrap. Pulls personal-memory from the obsidian-vaults
# bucket into the daemon's collective vault, writes config files,
# generates a bearer token, builds the initial snapshot.
ssh remote './scripts/bootstrap-remote-daemon.sh \
    --profile personal \
    --vault   memory \
    --bucket  obsidian-vaults \
    --minio-endpoint  minio.neverprepared.com \
    --minio-access-key NFC04PJQDF3P7ND82U70 \
    --minio-secret-key '\''<your-secret>'\'''
```

The script prints the bearer token + the exact `.claude.json` snippet you'll need in step 3. **Save the bearer token — it's only shown once.**

## Step 2 — On the REMOTE host: start the daemon

**Foreground (for first validation; you keep the terminal open):**
```bash
ssh remote pbrainctl serve
```

**Or systemd (background, survives reboots):**
```bash
scp scripts/systemd/pbrainctl.service remote:/etc/systemd/system/
ssh remote 'sudo systemctl daemon-reload && sudo systemctl enable --now pbrainctl'
ssh remote 'sudo journalctl -u pbrainctl -f'
```

Sanity check from the laptop:
```bash
curl -fsS http://<remote-host>:9998/api/brain/health | jq
```
Should return `{"status":"ok","vaults":[{"profile":"personal","vault":"memory"}]}`.

## Step 3 — On the LAPTOP: switch `.claude.json`

Find your current `.claude.json` (usually `~/.claude.json`). Locate the `phantom-brain` mcpServers entry; it currently looks like:

```json
"phantom-brain": {
  "command": "/path/to/pbrainctl",
  "args": ["mcp"],
  "env": {
    "BRAIN_VAULT_PATH": "/Users/cdowning/workspaces/profiles/personal/obsidian/vaults/personal-memory"
  }
}
```

Replace with:

```json
"phantom-brain": {
  "command": "/path/to/pbrainctl",
  "args": ["mcp"],
  "env": {
    "CL_BRAIN_API":         "http://<remote-host>:9998",
    "CL_BRAIN_API_TOKEN":   "<bearer-from-step-1>",
    "CL_WORKSPACE_PROFILE": "personal",
    "CL_BRAIN_VAULT":       "memory"
  }
}
```

**Don't use nested shell fallback syntax** (`${VAR:-${OTHER}}`) — Claude Code partial-expansion leaves a trailing `}` in the resolved value. Plain `${VAR}` references only.

## Step 4 — Quit + restart Claude Code

The currently-running MCP server is in legacy mode and outlives this conversation; the new agent-contract config only takes effect on the next launch. Quit Claude Code (Cmd-Q) and reopen.

## Step 5 — Verify

From a Claude Code chat, call:

```
brain_status
```

Expected JSON:
```json
{
  "brain_id": "<uuid>",
  "brain_dir": "/Users/.../.local/share/phantom-brain/personal/memory/brains/<uuid>",
  "manifest": {
    "schema_version": 1,
    "profile": "personal",
    "vault":   "memory",
    "parent_gen":            1,
    "parent_snapshot_sha256": "<sha>",
    "seed_source":           "tarball",
    ...
  },
  "heartbeat_age_secs": <small>,
  "ship_queue_count":  0,
  "ship_queue_bytes":  0
}
```

Key fields:
- `seed_source: "tarball"` — proves the brain birthed by downloading a snapshot from the daemon
- `parent_gen` non-zero — same proof
- `heartbeat_age_secs` < 60 — heartbeat goroutine is alive

If you see `seed_source: "greenfield"` instead, the daemon was unreachable at birth and the brain fell back to an empty vault. Check the daemon's logs.

## Step 6 — Round-trip test

Have Claude ingest something via `brain_perceive` or `brain_learn`. Then call `brain_death`:

```
brain_death
```

Returns the local payload path. The death payload ships to MinIO via the daemon, the daemon reaps + synthesizes, and the new content lands in the collective vault. Confirm:

```bash
ssh remote pbrainctl vault status
# personal/memory	gen=2	queue_pending=0	ledger_rows=1  maintenance=false
```

`gen=2` (was 1 after bootstrap) means a new snapshot was built. `ledger_rows=1` is the merge record from your dying brain.

## Switching back

If anything goes sideways, revert `.claude.json` to the legacy block:

```json
"env": { "BRAIN_VAULT_PATH": "/Users/cdowning/workspaces/profiles/personal/obsidian/vaults/personal-memory" }
```

Restart Claude Code. The legacy vault on disk is untouched throughout this exercise — fully reversible.

## Common diagnoses

| Symptom | Fix |
|---|---|
| `brain_status` returns "lifecycle not initialised (legacy BRAIN_VAULT_PATH mode)" | Restart didn't take effect — confirm `.claude.json` was saved + Claude Code was fully quit (Cmd-Q, not just window close) |
| `/api/brain/health` 401 INVALID_TOKEN | Bearer token mismatch between `.claude.json` and `auth.toml` |
| Birth fails with "daemon snapshot fetch" warning | Daemon unreachable from agent host. Check firewall, port, scheme |
| `bucket TEST does not exist` | Bucket names must be lowercase + ≥3 chars |
| Death payload upload 403 | MinIO access key lacks `s3:PutObject` on the bucket |
| `gen` doesn't bump after a death | Reaper hasn't fired yet (5s default poll); or synthesizer didn't get scheduled — `pbrainctl force-merge` + `pbrainctl force-checkpoint` exercise them manually |
