#!/usr/bin/env bash
# bootstrap-remote-daemon.sh — first-time setup of a pbrainctl daemon
# on a remote host that will serve agents over the network with MinIO
# as the death-payload transit.
#
# What it does:
#   1. Verifies pbrainctl is installed
#   2. Writes server.toml + profiles/<profile>/vaults/<vault>/auth.toml
#      into $PHANTOM_BRAIN_CONFIG_DIR
#   3. Generates a bearer token (or uses --token if provided)
#   4. Pulls the seed vault from MinIO into the daemon's
#      $PHANTOM_BRAIN_DATA_DIR/<profile>/<vault>/collective/vault/
#   5. Builds the first snapshot so agents have something to birth from
#   6. Prints the .claude.json snippet the operator pastes into their
#      agent host's config
#
# What it doesn't do:
#   - Install pbrainctl (you build it: `make build` from this repo
#     and copy the binary; or set up `go install` / a release tarball)
#   - Start the daemon as a service (you run `pbrainctl serve`
#     yourself; systemd unit example in scripts/systemd/)
#   - Configure firewall / reverse proxy / TLS in front of the daemon
#   - Renew bearer tokens
#
# Re-runnable? Pull + snapshot are incremental. The script REFUSES to
# overwrite existing server.toml / auth.toml — delete them first if
# you really want to re-bootstrap.

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  bootstrap-remote-daemon.sh \
      --profile <profile> \
      --vault   <vault> \
      --bucket  <obsidian-vaults-bucket> \
      --minio-endpoint <minio.example.com> \
      --minio-access-key <key> \
      --minio-secret-key <secret> \
      [--minio-use-ssl true|false]   (default true)
      [--data-dir   <path>]          (default $HOME/.local/share/phantom-brain-server)
      [--config-dir <path>]          (default $HOME/.config/phantom-brain-server)
      [--token <bearer>]             (default: generate via openssl rand -hex 32)
      [--port <n>]                   (default 9998)
      [--host <bind>]                (default 0.0.0.0)

Example:
  bootstrap-remote-daemon.sh \
      --profile personal \
      --vault   memory \
      --bucket  obsidian-vaults \
      --minio-endpoint  minio.neverprepared.com \
      --minio-access-key NFC04PJQDF3P7ND82U70 \
      --minio-secret-key 'werO1pPJVPy7WLd9lpeRSMVEdRmjc+gcv6AGWlYZ'
EOF
    exit 1
}

# Defaults
SSL=true
PORT=9998
HOST=0.0.0.0
DATA_DIR="$HOME/.local/share/phantom-brain-server"
CONFIG_DIR="$HOME/.config/phantom-brain-server"
TOKEN=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile)           PROFILE="$2";       shift 2 ;;
        --vault)             VAULT="$2";         shift 2 ;;
        --bucket)            BUCKET="$2";        shift 2 ;;
        --minio-endpoint)    MENDPOINT="$2";     shift 2 ;;
        --minio-access-key)  MACCESS="$2";       shift 2 ;;
        --minio-secret-key)  MSECRET="$2";       shift 2 ;;
        --minio-use-ssl)     SSL="$2";           shift 2 ;;
        --data-dir)          DATA_DIR="$2";      shift 2 ;;
        --config-dir)        CONFIG_DIR="$2";    shift 2 ;;
        --token)             TOKEN="$2";         shift 2 ;;
        --port)              PORT="$2";          shift 2 ;;
        --host)              HOST="$2";          shift 2 ;;
        -h|--help)           usage ;;
        *) usage ;;
    esac
done

for v in PROFILE VAULT BUCKET MENDPOINT MACCESS MSECRET; do
    [[ -n "${!v:-}" ]] || { echo "ERROR: --$(echo "$v" | tr '[:upper:]' '[:lower:]') is required" >&2; usage; }
done

require() {
    command -v "$1" >/dev/null 2>&1 || { echo "ERROR: missing dependency: $1" >&2; exit 2; }
}
require pbrainctl
require rclone
require openssl

VAULT_CFG_DIR="$CONFIG_DIR/profiles/$PROFILE/vaults/$VAULT"
COLLECTIVE_VAULT="$DATA_DIR/$PROFILE/$VAULT/collective/vault"
SERVER_TOML="$CONFIG_DIR/server.toml"
AUTH_TOML="$VAULT_CFG_DIR/auth.toml"

# --- Step 1: refuse if config already exists ---
for f in "$SERVER_TOML" "$AUTH_TOML"; do
    if [[ -f "$f" ]]; then
        echo "ERROR: $f already exists. Remove it (and probably the rest of $CONFIG_DIR) if you really want to re-bootstrap." >&2
        exit 3
    fi
done

# --- Step 2: write configs ---
mkdir -p "$VAULT_CFG_DIR"
cat >"$SERVER_TOML" <<EOF
[server]
port = $PORT
host = "$HOST"
log_level = "info"

[storage]
backend          = "minio"
minio_endpoint   = "$MENDPOINT"
minio_bucket     = "$BUCKET"
minio_access_key = "$MACCESS"
minio_secret_key = "$MSECRET"
minio_use_ssl    = $SSL

[defaults]
retention_gens = 30
reaper_poll_interval_secs = 5
EOF
chmod 600 "$SERVER_TOML"

if [[ -z "$TOKEN" ]]; then
    TOKEN="pb_${PROFILE}_${VAULT}_$(openssl rand -hex 24)"
fi
cat >"$AUTH_TOML" <<EOF
bearer_token = "$TOKEN"
description  = "auto-bootstrapped by bootstrap-remote-daemon.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
EOF
chmod 600 "$AUTH_TOML"

echo "wrote $SERVER_TOML"
echo "wrote $AUTH_TOML"

# --- Step 3: configure rclone remote if absent ---
if ! rclone listremotes | grep -q '^pbminio:'; then
    rclone config create pbminio s3 \
        provider Minio \
        endpoint "$( [[ "$SSL" == "true" ]] && echo https || echo http )://$MENDPOINT" \
        access_key_id "$MACCESS" \
        secret_access_key "$MSECRET" \
        region us-east-1 \
        force_path_style true \
        no_check_bucket false >/dev/null
    echo "created rclone remote 'pbminio:'"
fi

# --- Step 4: pull seed from MinIO ---
mkdir -p "$COLLECTIVE_VAULT"
echo "pulling $BUCKET/$VAULT into $COLLECTIVE_VAULT (mirror; incremental on re-run)"
rclone copy --progress --transfers=8 --checkers=16 \
    "pbminio:$BUCKET/$VAULT" "$COLLECTIVE_VAULT"

# --- Step 5: build first snapshot ---
echo "building initial snapshot"
pbrainctl snapshot rebuild "$PROFILE/$VAULT" \
    --data-dir "$DATA_DIR" --config-dir "$CONFIG_DIR"

# --- Step 6: print operator next steps ---
cat <<EOF

============================================================
  bootstrap complete
============================================================

daemon config dir : $CONFIG_DIR
daemon data dir   : $DATA_DIR
collective vault  : $COLLECTIVE_VAULT
bearer token      : $TOKEN

Next steps:

1. Start the daemon (foreground):
     pbrainctl serve

   Or run via systemd (see scripts/systemd/pbrainctl.service).

2. On your AGENT host (laptop running Claude Code), update .claude.json:

   "phantom-brain": {
     "command": "pbrainctl",
     "args": ["mcp"],
     "env": {
       "CL_BRAIN_API":         "http://<this-host>:$PORT",
       "CL_BRAIN_API_TOKEN":   "$TOKEN",
       "CL_WORKSPACE_PROFILE": "$PROFILE",
       "CL_BRAIN_VAULT":       "$VAULT"
     }
   }

3. Restart Claude Code so it spawns a new MCP server with the
   agent-contract env block.

4. Verify (from Claude Code or any MCP client) by calling
   brain_status; it should report seed_source=tarball and a
   non-zero parent_gen.

To switch BACK to the local vault, revert .claude.json to:
     "env": { "BRAIN_VAULT_PATH": "/path/to/your/vault" }
The legacy vault on disk is untouched.
EOF
