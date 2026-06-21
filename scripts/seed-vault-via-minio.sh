#!/usr/bin/env bash
# seed-vault-via-minio.sh — one-time copy of an existing on-disk vault
# into a v5.0 daemon's collective vault, transiting MinIO as the
# upload medium. Two phases:
#
#   pack    (run on the host that has the existing vault)
#   apply   (run on the daemon host)
#
# Uses mc for the actual byte movement so progress + retries come
# for free. The script's job is sanity-checking + invoking pbrainctl
# at the right moments.
#
# WARNING: this seeds the daemon's collective vault by direct file
# copy — bypasses the synthesis pipeline. The existing Wiki is
# preserved as-is. Any inconsistencies in the source carry forward.
# If you want a fresh re-synthesis, use the migrate-legacy path
# instead (see README).

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  seed-vault-via-minio.sh pack  --vault <local-vault-path> \
                                --bucket <bucket>          \
                                --prefix <profile/vault>   \
                                [--alias <mc-alias>]

  seed-vault-via-minio.sh apply --bucket <bucket>          \
                                --prefix <profile/vault>   \
                                --data-dir <pbrainctl data dir> \
                                [--alias <mc-alias>]

Examples:
  # On the laptop with the existing TS vault
  seed-vault-via-minio.sh pack \
      --vault   ~/obsidian/vaults/personal-memory \
      --bucket  testbucket \
      --prefix  personal/memory

  # On the daemon host (after mc alias set np https://...)
  seed-vault-via-minio.sh apply \
      --bucket   testbucket \
      --prefix   personal/memory \
      --data-dir /var/lib/phantom-brain
EOF
    exit 1
}

ALIAS="np"
MODE="${1:-}"
shift || usage

while [[ $# -gt 0 ]]; do
    case "$1" in
        --vault)    VAULT="$2";    shift 2 ;;
        --bucket)   BUCKET="$2";   shift 2 ;;
        --prefix)   PREFIX="$2";   shift 2 ;;
        --data-dir) DATA_DIR="$2"; shift 2 ;;
        --alias)    ALIAS="$2";    shift 2 ;;
        --yes|-y)   YES=1;         shift   ;;
        *) usage ;;
    esac
done

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "ERROR: missing dependency: $1" >&2
        exit 2
    fi
}

case "$MODE" in
    pack)
        [[ -n "${VAULT:-}" && -n "${BUCKET:-}" && -n "${PREFIX:-}" ]] || usage
        require mc

        # Sanity-check the source vault. The daemon expects at least
        # Wiki/ and Raw/ at the top level; _index/ is regenerable but
        # we ship it anyway to save the daemon a rebuild.
        for sub in Wiki Raw; do
            if [[ ! -d "$VAULT/$sub" ]]; then
                echo "ERROR: source vault missing required dir: $VAULT/$sub" >&2
                exit 3
            fi
        done

        echo "Pack plan:"
        echo "  source vault : $VAULT"
        echo "  target       : $ALIAS/$BUCKET/$PREFIX/seed/{Wiki,Raw,_index}"
        for sub in Wiki Raw _index; do
            if [[ -d "$VAULT/$sub" ]]; then
                printf "    %-8s %s\n" "$sub" "$(du -sh "$VAULT/$sub" | awk '{print $1}')"
            fi
        done
        if [[ "${YES:-}" != "1" ]]; then
            read -rp "Proceed? [y/N] " ok
            [[ "$ok" == "y" || "$ok" == "Y" ]] || { echo "aborted"; exit 0; }
        fi

        # Upload each top-level subdir as a mirrored prefix. `mc mirror`
        # is incremental + resumable; safe to re-run on partial uploads.
        for sub in Wiki Raw _index; do
            if [[ -d "$VAULT/$sub" ]]; then
                echo "+ mc mirror $VAULT/$sub $ALIAS/$BUCKET/$PREFIX/seed/$sub"
                mc mirror --overwrite "$VAULT/$sub" "$ALIAS/$BUCKET/$PREFIX/seed/$sub"
            fi
        done
        echo "Pack complete. Now run on the daemon host:"
        echo "  $0 apply --bucket $BUCKET --prefix $PREFIX --data-dir <path>"
        ;;

    apply)
        [[ -n "${BUCKET:-}" && -n "${PREFIX:-}" && -n "${DATA_DIR:-}" ]] || usage
        require mc
        require pbrainctl

        # Refuse if the collective already has content — we don't want
        # to clobber a vault that's already in use.
        COLLECTIVE_VAULT="$DATA_DIR/$PREFIX/collective/vault"
        if [[ -d "$COLLECTIVE_VAULT/Wiki" ]] && [[ "$(ls -A "$COLLECTIVE_VAULT/Wiki" 2>/dev/null)" ]]; then
            echo "ERROR: $COLLECTIVE_VAULT/Wiki is non-empty." >&2
            echo "Refusing to overwrite an active vault. Remove it first if you really want to re-seed." >&2
            exit 4
        fi

        echo "Apply plan:"
        echo "  source       : $ALIAS/$BUCKET/$PREFIX/seed/{Wiki,Raw,_index}"
        echo "  target       : $COLLECTIVE_VAULT/{Wiki,Raw,_index}"
        if [[ "${YES:-}" != "1" ]]; then
            read -rp "Proceed? [y/N] " ok
            [[ "$ok" == "y" || "$ok" == "Y" ]] || { echo "aborted"; exit 0; }
        fi

        mkdir -p "$COLLECTIVE_VAULT"
        for sub in Wiki Raw _index; do
            if mc stat "$ALIAS/$BUCKET/$PREFIX/seed/$sub" >/dev/null 2>&1; then
                echo "+ mc mirror $ALIAS/$BUCKET/$PREFIX/seed/$sub $COLLECTIVE_VAULT/$sub"
                mc mirror --overwrite "$ALIAS/$BUCKET/$PREFIX/seed/$sub" "$COLLECTIVE_VAULT/$sub"
            else
                echo "  (skip $sub — not in bucket)"
            fi
        done

        # Build the first snapshot so birthing brains have something to
        # download. snapshot rebuild requires the registry to know
        # about the vault (configured in $PHANTOM_BRAIN_CONFIG_DIR).
        IFS='/' read -r PROFILE VAULT_NAME <<<"$PREFIX"
        echo "+ pbrainctl snapshot rebuild $PROFILE/$VAULT_NAME --data-dir $DATA_DIR"
        pbrainctl snapshot rebuild "$PROFILE/$VAULT_NAME" --data-dir "$DATA_DIR"

        echo
        echo "Done. Verify with:"
        echo "  pbrainctl snapshot status $PROFILE/$VAULT_NAME --data-dir $DATA_DIR"
        echo "  pbrainctl vault status --data-dir $DATA_DIR"
        echo
        echo "(Optional) remove the bucket-side seed after verifying:"
        echo "  mc rm --recursive --force $ALIAS/$BUCKET/$PREFIX/seed/"
        ;;

    *)
        usage
        ;;
esac
