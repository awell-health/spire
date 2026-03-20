#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

# Required env vars
: "${DOLTHUB_REMOTE:?DOLTHUB_REMOTE must be set}"
: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"
: "${STEWARD_STALE_THRESHOLD:=4h}"

# Configure git identity (required by dolt)
git config --global user.name "spire-steward"
git config --global user.email "steward@spire.local"

# Configure dolt credentials (JWK file mounted from dolt-creds secret)
CRED_FILE=$(ls /root/.dolt/creds/*.jwk 2>/dev/null | head -1)
if [ -n "$CRED_FILE" ]; then
    KEY_ID=$(basename "$CRED_FILE" .jwk)
    dolt config --global --set user.creds "$KEY_ID" 2>/dev/null || true
    echo "[steward] dolt credential configured: $KEY_ID"
fi

# Initialize beads from DoltHub clone
if [ ! -d /data/.beads ]; then
    cd /data
    git init -q

    # Clone the real database first
    echo "[steward] cloning from DoltHub: $DOLTHUB_REMOTE"
    mkdir -p /data/.beads/dolt
    dolt clone "$DOLTHUB_REMOTE" "/data/.beads/dolt/$BEADS_PREFIX" 2>&1 \
        || echo "[steward] clone warning: could not clone (will init fresh)"

    # Init beads on top of the cloned data
    echo "[steward] initializing beads..."
    bd init --prefix "$BEADS_PREFIX" --force
    spire init --prefix="$BEADS_PREFIX" --standalone
    echo "[steward] init complete"
fi

# Ensure routes.jsonl maps the spi rig to local data (required by --rig=spi)
ROUTES_FILE="/data/.beads/routes.jsonl"
if ! grep -q '"prefix":"spi-"' "$ROUTES_FILE" 2>/dev/null; then
    echo '{"prefix":"spi-","path":"."}' >> "$ROUTES_FILE"
    echo "[steward] added spi- route"
fi

cd /data

# Pin dolt to port 3307 so spire commands (send, register, etc.) can find it
bd dolt set port 3307 2>/dev/null || true

# Clean stale lock and start dolt server once for the lifetime of the container
rm -f /data/.beads/dolt-server.lock
echo "[steward] starting dolt server..."
bd dolt start
echo "[steward] dolt server running (port 3307)"

# Shut down dolt cleanly on container exit
trap 'echo "[steward] shutting down dolt..."; bd dolt stop 2>/dev/null; exit 0' TERM INT

# Register steward as an agent
spire register steward "Spire steward — automated work coordinator" 2>/dev/null || true

# Start the bead bridge (creates SpireWorkload CRs from ready beads)
/bead-bridge.sh &
BRIDGE_PID=$!
echo "[steward] bead bridge started (PID $BRIDGE_PID)"

echo "[steward] ready. interval=$STEWARD_INTERVAL, stale=$STEWARD_STALE_THRESHOLD"

# Run the steward loop (replaces this process)
exec spire steward \
    --interval="$STEWARD_INTERVAL" \
    --stale-threshold="$STEWARD_STALE_THRESHOLD" \
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"}
