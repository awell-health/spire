#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"
: "${DOLT_HOST:=spire-dolt.spire.svc}"
: "${DOLT_PORT:=3306}"
: "${DOLT_REMOTE_URL:=https://doltremoteapi.dolthub.com/awell/spire}"
: "${DOLT_CREDS_KEY:=}"

# Set up DoltHub JWK credentials.
# Creds must be at $HOME/.dolt/creds/ — dolt looks relative to HOME.
if [ -d /creds ] && [ -n "$DOLT_CREDS_KEY" ]; then
    mkdir -p "$HOME/.dolt/creds"
    cp /creds/*.jwk "$HOME/.dolt/creds/" 2>/dev/null || true
    cat > "$HOME/.dolt/config_global.json" <<DOLTCFG
{"user.creds":"$DOLT_CREDS_KEY","user.name":"spire-steward","user.email":"steward@spire.local"}
DOLTCFG
    echo "[steward] DoltHub credentials configured (key: $DOLT_CREDS_KEY)"
fi

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

    # Align project ID with the dolt server's existing project.
    # The steward uses emptyDir, so bd init generates a new project ID on each restart.
    # The dolt server retains the canonical project ID.
    SERVER_PROJECT_ID=$(DOLT_CLI_PASSWORD="" dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --user root --no-tls sql -q \
        "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'" -r csv 2>/dev/null | tail -1)
    if [ -n "$SERVER_PROJECT_ID" ] && [ "$SERVER_PROJECT_ID" != "value" ]; then
        jq --arg pid "$SERVER_PROJECT_ID" '.project_id = $pid' /data/.beads/metadata.json > /tmp/meta.json \
            && mv /tmp/meta.json /data/.beads/metadata.json
        echo "[steward] aligned project ID with dolt server: $SERVER_PROJECT_ID"
    fi
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

# Configure DoltHub as the dolt remote (for bd dolt pull/push)
echo "[steward] configuring remote: $DOLT_REMOTE_URL"
bd dolt remote remove origin 2>/dev/null || true
bd dolt remote add origin "$DOLT_REMOTE_URL" 2>/dev/null || true

# Initial pull from DoltHub to sync cluster state
echo "[steward] pulling from DoltHub..."
if bd dolt pull 2>&1; then
    echo "[steward] DoltHub pull complete"
else
    echo "[steward] WARNING: DoltHub pull failed (will retry on next cycle)"
fi

# Re-align project ID after pull (DoltHub may have changed it)
SERVER_PROJECT_ID=$(DOLT_CLI_PASSWORD="" dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --user root --no-tls sql -q \
    "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'" -r csv 2>/dev/null | tail -1)
if [ -n "$SERVER_PROJECT_ID" ] && [ "$SERVER_PROJECT_ID" != "value" ]; then
    CURRENT_PID=$(jq -r '.project_id' /data/.beads/metadata.json 2>/dev/null)
    if [ "$CURRENT_PID" != "$SERVER_PROJECT_ID" ]; then
        jq --arg pid "$SERVER_PROJECT_ID" '.project_id = $pid' /data/.beads/metadata.json > /tmp/meta.json \
            && mv /tmp/meta.json /data/.beads/metadata.json
        echo "[steward] re-aligned project ID after pull: $SERVER_PROJECT_ID"
    fi
fi

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
