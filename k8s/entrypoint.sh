#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"
: "${STEWARD_BACKEND:=}"
: "${STEWARD_METRICS_PORT:=0}"

# .beads/ is seeded by the initContainer at $BEADS_DIR (per-database
# layout: <dataRoot>/<database>/.beads — see helm/spire/templates/_helpers.tpl).
# No bd init, no project ID alignment, no DoltHub remotes.
: "${BEADS_DIR:?BEADS_DIR not set — container env must plumb it from spire.beadsDir}"
[ -f "$BEADS_DIR/metadata.json" ] || { echo "[steward] FATAL: $BEADS_DIR/metadata.json missing (initContainer failed?)"; exit 1; }

git config --global user.name "spire-steward"
git config --global user.email "steward@spire.local"

cd "$(dirname "$BEADS_DIR")"

# Wait for dolt
echo "[steward] waiting for dolt..."
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

# Register steward
spire register steward "Spire steward — automated work coordinator" 2>/dev/null || true

# Start the bead bridge
/bead-bridge.sh &
echo "[steward] bead bridge started (PID $!)"

echo "[steward] ready. interval=$STEWARD_INTERVAL"

exec spire steward \
    --interval="$STEWARD_INTERVAL" \
    --no-assign \
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"} \
    ${STEWARD_BACKEND:+--backend="$STEWARD_BACKEND"} \
    ${STEWARD_METRICS_PORT:+--metrics-port="$STEWARD_METRICS_PORT"}
