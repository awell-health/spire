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

# Initialize beads pointing at the shared dolt service.
# --database spi connects to the existing DB (no new project ID).
if [ ! -f /data/.beads/metadata.json ]; then
    echo "[steward] initializing beads (dolt server: $DOLT_HOST:$DOLT_PORT)..."
    git init -q 2>/dev/null || true
    bd init --database spi --prefix "$BEADS_PREFIX" \
        --server-host "$DOLT_HOST" --server-port "$DOLT_PORT" 2>/dev/null || true
    [ -d /data/.beads ] || { echo "[steward] FATAL: bd init failed"; exit 1; }
    spire init --prefix="$BEADS_PREFIX" --standalone 2>/dev/null || true
    # Kill any local dolt server that bd init started.
    bd dolt stop 2>/dev/null || true
    rm -f /data/.beads/dolt-server.port /data/.beads/dolt-server.pid /data/.beads/dolt-server.lock
    # Align project ID with whatever the server has.
    _SPID=$(DOLT_CLI_PASSWORD="" dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --user root --no-tls sql -q \
        "USE spi; SELECT value FROM metadata WHERE \`key\`='_project_id'" -r csv 2>/dev/null | tail -1)
    [ -n "$_SPID" ] && [ "$_SPID" != "value" ] && \
        jq --arg p "$_SPID" '.project_id=$p' /data/.beads/metadata.json > /tmp/m.json && mv /tmp/m.json /data/.beads/metadata.json
fi

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

# Register steward
spire register steward "Spire steward — automated work coordinator" 2>/dev/null || true

# Start the bead bridge
/bead-bridge.sh &
BRIDGE_PID=$!
echo "[steward] bead bridge started (PID $BRIDGE_PID)"

echo "[steward] ready. interval=$STEWARD_INTERVAL"

# Run the steward loop.
# --no-assign: managed agents get work via operator (SpireWorkloads), not messages.
exec spire steward \
    --interval="$STEWARD_INTERVAL" \
    --no-assign \
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"}
