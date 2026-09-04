#!/usr/bin/env bash
#
# Railway entrypoint for the vpn-ui container profile.
#
# Responsibilities (everything the "one-click" deploy needs, so no manual
# setup is required after the button):
#
#   1. Resolve the state directories. /data is the Railway volume mount point;
#      without a volume the deploy still works but state is ephemeral (a loud
#      warning is printed).
#   2. Sync the panel's web port to Railway's $PORT. Railway proxies the
#      service's assigned port, so the DB's webPort must always match it.
#      This runs on EVERY boot, so changing the port in Railway's service
#      settings just works after a redeploy.
#   3. First-run provisioning, applied exactly once (guarded by a marker file
#      on the persistent volume so later panel-side changes survive restarts):
#        - VPNUI_ADMIN_USERNAME + VPNUI_ADMIN_PASSWORD  -> panel admin login
#        - VPNUI_WEB_BASE_PATH                          -> panel base path
#   4. exec the web server (`vpn-ui run`).
#
# Everything is driven by env vars; see RAILWAY.md for the full table.

set -euo pipefail

cd /app

log()  { printf '[vpn-ui-railway] %s\n' "$*"; }
warn() { printf '[vpn-ui-railway] WARNING: %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 1) State directories (database, logs, extracted Xray core) under the volume
# ---------------------------------------------------------------------------
DATA_DIR="${VPNUI_DATA_DIR:-/data}"
if ! mkdir -p "${DATA_DIR}" 2>/dev/null || [ ! -w "${DATA_DIR}" ]; then
    DATA_DIR="/app/data"
    mkdir -p "${DATA_DIR}"
    warn "/data is not writable — no Railway volume is attached to this service."
    warn "Falling back to container storage at ${DATA_DIR}: the database and"
    warn "config will be EPHEMERAL and LOST on every redeploy/restart."
    warn "Attach a volume at /data (Railway: Service > Volumes) to persist."
fi

export VPNUI_DB_FOLDER="${VPNUI_DB_FOLDER:-${DATA_DIR}}"
export VPNUI_LOG_FOLDER="${VPNUI_LOG_FOLDER:-${DATA_DIR}/logs}"
export VPNUI_BIN_FOLDER="${VPNUI_BIN_FOLDER:-${DATA_DIR}/bin}"
mkdir -p "${VPNUI_DB_FOLDER}" "${VPNUI_LOG_FOLDER}" "${VPNUI_BIN_FOLDER}"

# ---------------------------------------------------------------------------
# 2) Panel port follows Railway's PORT (0.0.0.0:$PORT is the proxy target)
# ---------------------------------------------------------------------------
PORT="${PORT:-2083}"
if ! [[ "${PORT}" =~ ^[0-9]+$ ]] || [ "${PORT}" -lt 1 ] || [ "${PORT}" -gt 65535 ]; then
    warn "PORT='${PORT}' is not a valid TCP port — falling back to 2083"
    PORT=2083
fi
# Idempotent: writes webPort into the settings DB before the server starts.
log "syncing panel port to Railway PORT=${PORT}"
./vpn-ui setting -port "${PORT}"

# ---------------------------------------------------------------------------
# 3) First-run provisioning (once; marker lives on the persistent volume)
# ---------------------------------------------------------------------------
MARKER="${DATA_DIR}/.railway-provisioned"
if [ ! -f "${MARKER}" ]; then
    if [ -n "${VPNUI_ADMIN_USERNAME:-}" ] && [ -n "${VPNUI_ADMIN_PASSWORD:-}" ]; then
        log "provisioning admin user '${VPNUI_ADMIN_USERNAME}' from env"
        ./vpn-ui setting -username "${VPNUI_ADMIN_USERNAME}" -password "${VPNUI_ADMIN_PASSWORD}"
    elif [ -n "${VPNUI_ADMIN_USERNAME:-}" ] || [ -n "${VPNUI_ADMIN_PASSWORD:-}" ]; then
        warn "only one of VPNUI_ADMIN_USERNAME / VPNUI_ADMIN_PASSWORD is set —"
        warn "both are required to provision the admin; keeping the default"
        warn "admin login. CHANGE IT after first login (Panel Settings > Authentication)."
    else
        warn "no VPNUI_ADMIN_USERNAME/VPNUI_ADMIN_PASSWORD set — the panel starts"
        warn "with the default admin/admin login. CHANGE IT after first login."
    fi

    if [ -n "${VPNUI_WEB_BASE_PATH:-}" ]; then
        log "setting panel base path to '${VPNUI_WEB_BASE_PATH}'"
        ./vpn-ui setting -webBasePath "${VPNUI_WEB_BASE_PATH}"
    fi

    mkdir -p "$(dirname "${MARKER}")"
    touch "${MARKER}"
fi

# ---------------------------------------------------------------------------
# 4) Run
# ---------------------------------------------------------------------------
log "starting vpn-ui panel on 0.0.0.0:${PORT} (state in ${DATA_DIR})"
exec ./vpn-ui run
