#!/usr/bin/env bash
set -e

echo "[mayor] starting up..."

# Required env vars
: "${DOLTHUB_REMOTE:?DOLTHUB_REMOTE must be set}"
: "${MAYOR_PREFIX:=mayor}"
: "${MAYOR_INTERVAL:=2m}"
: "${MAYOR_STALE_THRESHOLD:=4h}"

# Configure git identity (required by dolt)
git config --global user.name "spire-mayor"
git config --global user.email "mayor@spire.local"

# Initialize beads if not already done
if [ ! -d /data/.beads ]; then
    echo "[mayor] initializing beads database..."
    cd /data
    git init -q
    bd init --prefix "$MAYOR_PREFIX" --force
    echo "[mayor] syncing from DoltHub: $DOLTHUB_REMOTE"
    spire init --prefix="$MAYOR_PREFIX" --standalone
    bd dolt remote add origin "$DOLTHUB_REMOTE" 2>/dev/null || true
    spire sync --hard "$DOLTHUB_REMOTE"
    echo "[mayor] initial sync complete"
fi

cd /data

# Register mayor as an agent
spire register mayor "Spire mayor — automated work coordinator" 2>/dev/null || true

echo "[mayor] ready. interval=$MAYOR_INTERVAL, stale=$MAYOR_STALE_THRESHOLD"

# Run the mayor loop
exec spire mayor \
    --interval="$MAYOR_INTERVAL" \
    --stale-threshold="$MAYOR_STALE_THRESHOLD" \
    ${MAYOR_AGENTS:+--agents="$MAYOR_AGENTS"}
