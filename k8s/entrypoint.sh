#!/usr/bin/env bash
set -e

echo "[steward] starting up..."

: "${BEADS_PREFIX:=spi}"
: "${STEWARD_INTERVAL:=2m}"

# .beads/ is seeded by the initContainer from the beads-seed ConfigMap.
# No bd init, no project ID alignment, no DoltHub remotes.
[ -f /data/.beads/metadata.json ] || { echo "[steward] FATAL: .beads/metadata.json missing (initContainer failed?)"; exit 1; }

git config --global user.name "spire-steward"
git config --global user.email "steward@spire.local"

cd /data

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
    ${STEWARD_AGENTS:+--agents="$STEWARD_AGENTS"}
