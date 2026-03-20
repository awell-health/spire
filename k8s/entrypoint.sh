#!/usr/bin/env bash
set -e

echo "[mayor] starting up..."

# Required env vars
: "${DOLTHUB_REMOTE:?DOLTHUB_REMOTE must be set}"
: "${BEADS_PREFIX:=spi}"
: "${MAYOR_INTERVAL:=2m}"
: "${MAYOR_STALE_THRESHOLD:=4h}"

# Configure git identity (required by dolt)
git config --global user.name "spire-mayor"
git config --global user.email "mayor@spire.local"

# Configure dolt credentials (JWK file mounted from dolt-creds secret)
CRED_FILE=$(ls /root/.dolt/creds/*.jwk 2>/dev/null | head -1)
if [ -n "$CRED_FILE" ]; then
    KEY_ID=$(basename "$CRED_FILE" .jwk)
    dolt config --global --set user.creds "$KEY_ID" 2>/dev/null || true
    echo "[mayor] dolt credential configured: $KEY_ID"
fi

# Initialize beads from DoltHub clone
if [ ! -d /data/.beads ]; then
    cd /data
    git init -q

    # Clone the real database first
    echo "[mayor] cloning from DoltHub: $DOLTHUB_REMOTE"
    mkdir -p /data/.beads/dolt
    dolt clone "$DOLTHUB_REMOTE" "/data/.beads/dolt/$BEADS_PREFIX" 2>&1 \
        || echo "[mayor] clone warning: could not clone (will init fresh)"

    # Init beads on top of the cloned data
    echo "[mayor] initializing beads..."
    bd init --prefix "$BEADS_PREFIX" --force
    spire init --prefix="$BEADS_PREFIX" --standalone
    echo "[mayor] init complete"
fi

# Ensure routes.jsonl maps the spi rig to local data (required by --rig=spi)
ROUTES_FILE="/data/.beads/routes.jsonl"
if ! grep -q '"prefix":"spi-"' "$ROUTES_FILE" 2>/dev/null; then
    echo '{"prefix":"spi-","path":"."}' >> "$ROUTES_FILE"
    echo "[mayor] added spi- route"
fi

cd /data

# Pin dolt to port 3307 so spire commands (send, register, etc.) can find it
bd dolt set port 3307 2>/dev/null || true

# Clean stale lock and start dolt server once for the lifetime of the container
rm -f /data/.beads/dolt-server.lock
echo "[mayor] starting dolt server..."
bd dolt start
echo "[mayor] dolt server running (port 3307)"

# Shut down dolt cleanly on container exit
trap 'echo "[mayor] shutting down dolt..."; bd dolt stop 2>/dev/null; exit 0' TERM INT

# Register mayor as an agent
spire register mayor "Spire mayor — automated work coordinator" 2>/dev/null || true

echo "[mayor] ready. interval=$MAYOR_INTERVAL, stale=$MAYOR_STALE_THRESHOLD"

# Run the mayor loop
exec spire mayor \
    --interval="$MAYOR_INTERVAL" \
    --stale-threshold="$MAYOR_STALE_THRESHOLD" \
    ${MAYOR_AGENTS:+--agents="$MAYOR_AGENTS"}
