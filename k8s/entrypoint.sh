#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

# Required env vars
: "${DOLTHUB_REMOTE:?DOLTHUB_REMOTE must be set}"
: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"

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

cd /data

# First boot: clone from DoltHub and init beads.
# Subsequent boots: PV has data, just clean up stale locks and start.
if [ ! -d /data/.beads/dolt/"$BEADS_PREFIX" ]; then
    echo "[steward] first boot — initializing from DoltHub..."
    git init -q 2>/dev/null || true

    mkdir -p /data/.beads/dolt
    echo "[steward] cloning $DOLTHUB_REMOTE..."
    dolt clone "$DOLTHUB_REMOTE" "/data/.beads/dolt/$BEADS_PREFIX" 2>&1 \
        || echo "[steward] clone warning: could not clone (will init fresh)"

    bd init --prefix "$BEADS_PREFIX" --force
    spire init --prefix="$BEADS_PREFIX" --standalone 2>/dev/null || true
    echo "[steward] init complete"
else
    echo "[steward] PV has data — skipping clone"
fi

# Ensure routes include our prefix
ROUTES_FILE="/data/.beads/routes.jsonl"
if ! grep -q "\"prefix\":\"${BEADS_PREFIX}-\"" "$ROUTES_FILE" 2>/dev/null; then
    echo "{\"prefix\":\"${BEADS_PREFIX}-\",\"path\":\".\"}" >> "$ROUTES_FILE"
fi

# Clean stale locks from previous pod (PV survives restarts, locks don't)
rm -f /data/.beads/dolt-server.lock /data/.beads/dolt-server.pid

# Pin dolt port and start server
bd dolt set port 3307 2>/dev/null || true
echo "[steward] starting dolt server..."
bd dolt start
echo "[steward] dolt server running (port 3307)"

# Shut down dolt cleanly on container exit
trap 'echo "[steward] shutting down dolt..."; bd dolt stop 2>/dev/null; exit 0' TERM INT

# Register steward
spire register steward "Spire steward — automated work coordinator" 2>/dev/null || true

# Start the bead bridge
/bead-bridge.sh &
BRIDGE_PID=$!
echo "[steward] bead bridge started (PID $BRIDGE_PID)"

echo "[steward] ready. interval=$STEWARD_INTERVAL"

# Run the steward loop. Stale/timeout thresholds come from spire.yaml
# (mounted as configmap at /etc/spire/spire.yaml, steward reads via repoconfig.Load).
exec spire steward \
    --interval="$STEWARD_INTERVAL" \
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"}
