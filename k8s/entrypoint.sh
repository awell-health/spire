#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"
: "${DOLT_HOST:=spire-dolt.spire.svc}"
: "${DOLT_PORT:=3306}"
: "${DOLT_REMOTE_URL:=http://spire-dolt:50051/spi}"
# DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD injected from k8s secret
export DOLT_REMOTE_PASSWORD="${DOLT_REMOTE_PASSWORD:-}"

# Configure git identity
git config --global user.name "spire-steward"
git config --global user.email "steward@spire.local"

cd /data

# Initialize beads to point at the dolt service.
# bd init creates the local .beads config; the dolt server is external.
if [ ! -f /data/.beads/metadata.json ]; then
    echo "[steward] initializing beads (dolt server: $DOLT_HOST:$DOLT_PORT)..."
    git init -q 2>/dev/null || true
    BEADS_DOLT_SERVER_HOST="$DOLT_HOST" \
    BEADS_DOLT_SERVER_PORT="$DOLT_PORT" \
    bd init --prefix "$BEADS_PREFIX" --force
    spire init --prefix="$BEADS_PREFIX" --standalone 2>/dev/null || true
fi

# Point beads at the external dolt service.
bd dolt set host "$DOLT_HOST" 2>/dev/null || true
bd dolt set port "$DOLT_PORT" 2>/dev/null || true
# Remove dolt-server.port file — it overrides metadata and is created by bd init.
# For external dolt servers, this file must not exist.
rm -f /data/.beads/dolt-server.port /data/.beads/dolt-server.pid /data/.beads/dolt-server.lock

# Ensure routes
ROUTES_FILE="/data/.beads/routes.jsonl"
if ! grep -q "\"prefix\":\"${BEADS_PREFIX}-\"" "$ROUTES_FILE" 2>/dev/null; then
    echo "{\"prefix\":\"${BEADS_PREFIX}-\",\"path\":\".\"}" >> "$ROUTES_FILE"
fi

# Wait for dolt service to be reachable
echo "[steward] waiting for dolt at $DOLT_HOST:$DOLT_PORT..."
tries=0
while ! bd dolt test >/dev/null 2>&1 && [ $tries -lt 30 ]; do
    sleep 2
    tries=$((tries + 1))
done

if bd dolt test >/dev/null 2>&1; then
    echo "[steward] dolt connected"
else
    echo "[steward] WARNING: dolt not reachable after 60s, continuing anyway"
fi

# Configure remotesapi as the dolt remote (for bd dolt pull/push)
echo "[steward] configuring remote: $DOLT_REMOTE_URL (user: $DOLT_REMOTE_USER)"
bd dolt remote add origin "$DOLT_REMOTE_URL" 2>/dev/null || true

# Register steward
spire register steward "Spire steward — automated work coordinator" 2>/dev/null || true

# Start the bead bridge
/bead-bridge.sh &
BRIDGE_PID=$!
echo "[steward] bead bridge started (PID $BRIDGE_PID)"

echo "[steward] ready. interval=$STEWARD_INTERVAL"

# Run the steward loop. Stale/timeout come from spire.yaml (ConfigMap).
exec spire steward \
    --interval="$STEWARD_INTERVAL" \
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"}
