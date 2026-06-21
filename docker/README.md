# Docker — MinIO + phantom-brain daemon, one `docker compose up`

For operators who want the full stack containerized: MinIO + `pbrainctl serve` running together, state persisted to bind-mounts, bucket auto-created on first boot.

## Quick start

```bash
cd docker
cp .env.example .env                   # set MINIO_ROOT_PASSWORD + (optional) MINIO_PUBLIC_URL
cp -r config-example config            # then edit config/server.toml + auth.toml
docker compose up -d --build
docker compose logs -f pbrainctl
```

After it's running:

```bash
curl http://localhost:9998/api/brain/health
# {"status":"ok","vaults":[{"profile":"personal","vault":"memory"}]}
```

The MinIO console is at <http://localhost:9001> (root creds from `.env`).

## What the stack looks like

```
host  ──:9998──▶  pbrainctl-daemon  (container, host network)
                       │
                       │ MinIO API at MINIO_PUBLIC_URL (default localhost:9000)
                       ▼
host  ──:9000──▶  minio  (container, bridge network, port-forwarded)
host  ──:9001──▶  minio console
                       ▲
                       │ mc-init one-shot creates the bucket
                       │ + adds a 1-day lifecycle on _uploads/*
                       └─ exits cleanly, doesn't restart
```

## Bind-mount layout

The compose file writes everything under `./docker/data/`:

```
docker/data/
├── minio/        ← MinIO's blob storage
└── pbrainctl/    ← daemon's $PHANTOM_BRAIN_DATA_DIR (collective vault, snapshots, ledger)
```

Add `docker/data/` + `docker/.env` + `docker/config/` to `.gitignore` if you commit your operator setup elsewhere.

## Networking — why `network_mode: host` on the daemon

Presigned URLs that MinIO mints embed a hostname. The daemon's MinIO client uses the same endpoint string for both:
- Its own internal calls (`GetObject` in `/merge/complete`)
- The presigned URLs it returns to agents

If those two need different hostnames (the classic "internal vs external endpoint" S3 problem), you have to do networking tricks. Host networking sidesteps it: the daemon resolves `localhost:9000` the same way agents on the host do.

**For multi-host setups** (agents on different machines than the daemon), drop `network_mode: host` from the `pbrainctl` service and set `MINIO_PUBLIC_URL=http://<your-server-ip>:9000` (or an https reverse-proxied URL) in `.env`. Presigned URLs will then use that hostname; the daemon talks to MinIO via the compose-network DNS name `minio:9000`. Note: in that mode, `minio_endpoint` in `server.toml` must also change to `minio:9000`.

## Switching the agent (`pbrainctl mcp`) to talk to this daemon

In your laptop's `.claude.json`, replace the `phantom-brain` entry with:

```json
"phantom-brain": {
  "command": "pbrainctl",
  "args": ["mcp"],
  "env": {
    "CL_BRAIN_API":         "http://localhost:9998",
    "CL_BRAIN_API_TOKEN":   "<bearer-from-auth.toml>",
    "CL_WORKSPACE_PROFILE": "personal",
    "CL_BRAIN_VAULT":       "memory"
  }
}
```

Quit + reopen Claude Code. Call `brain_status` — expect `seed_source: "tarball"` if you've seeded the collective vault, or `"greenfield"` if not.

## Seeding the collective vault

The compose stack starts with an empty `personal/memory` vault. To populate it from an existing vault:

```bash
# rclone from a MinIO bucket (e.g. the obsidian-vaults backup):
rclone copy --progress minio:obsidian-vaults/personal-memory \
    ./data/pbrainctl/personal/memory/collective/vault

# or rsync from local
rsync -av /path/to/your/vault/{Wiki,Raw,_index}/ \
    ./data/pbrainctl/personal/memory/collective/vault/

# then build the first snapshot inside the running container
docker compose exec pbrainctl pbrainctl snapshot rebuild personal/memory
```

## Operator commands inside the container

```bash
docker compose exec pbrainctl pbrainctl version
docker compose exec pbrainctl pbrainctl vault list
docker compose exec pbrainctl pbrainctl snapshot status personal/memory
docker compose exec pbrainctl pbrainctl queue depth
```

## Updating to a new release

```bash
docker compose pull            # pulls newer minio if upstream tagged
docker compose up -d --build   # rebuilds pbrainctl from this repo
```

(Eventually we'll publish prebuilt phantom-brain images so the `--build` becomes a `pull`.)

## Hardening checklist (for non-dev use)

- Replace `MINIO_ROOT_USER=minioadmin` + the default password with strong values
- Scope a service account (via `mc admin user svcacct add`) instead of using root creds in `server.toml`
- Move `MINIO_PUBLIC_URL` behind an https reverse proxy with a real cert
- Switch `restart: unless-stopped` to a real supervisor (systemd, k8s) if you outgrow compose
- Move `data/` off the project dir onto a backed-up volume
