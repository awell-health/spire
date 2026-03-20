#!/usr/bin/env bash
# Repro: CALL dolt_fetch() fails with PermissionDenied in dolthub/dolt-sql-server Docker image
# while dolt clone succeeds with the same credentials in the same container.
#
# Local (macOS) dolt sql-server: CALL dolt_fetch() works fine.
# Docker dolthub/dolt-sql-server: CALL dolt_fetch() returns PermissionDenied.
#
# The issue appears to be that the Docker image's entrypoint sets HOME=/etc/spire
# (read-only) and stores auto-generated credentials at /var/lib/dolt/.dolt/creds/
# which interfere with user-provided JWK credentials.
#
# Prerequisites:
#   - dolt CLI installed locally
#   - Valid DoltHub credentials (dolt login)
#   - Docker running
#   - Access to a DoltHub database (change REMOTE below)
#
# Usage: ./dolt-fetch-repro.sh
set -euo pipefail

REMOTE="${1:-https://doltremoteapi.dolthub.com/awell/spire}"
DB_NAME="spi"

echo "=== Step 1: Local repro (expected: works) ==="
TMPDIR=$(mktemp -d)
cd "$TMPDIR"
dolt clone "$REMOTE" "$DB_NAME"
cd "$DB_NAME"

dolt sql-server --host 0.0.0.0 --port 3399 &
SERVER_PID=$!
sleep 3

echo "--- CALL dolt_fetch via local server ---"
DOLT_CLI_PASSWORD="" dolt --host 127.0.0.1 --port 3399 --user root --no-tls sql -q \
  "USE $DB_NAME; CALL dolt_fetch('origin', 'main')" 2>&1 | tail -3
LOCAL_EXIT=$?

kill $SERVER_PID 2>/dev/null; wait $SERVER_PID 2>/dev/null
rm -rf "$TMPDIR"

echo ""
echo "Local result: $([ $LOCAL_EXIT -eq 0 ] && echo 'SUCCESS' || echo 'FAILED')"
echo ""

echo "=== Step 2: Docker repro (expected: fails with PermissionDenied) ==="
echo "Copy your dolt creds into a temp dir..."

CREDS_DIR=$(mktemp -d)
ACTIVE_KEY=$(cat ~/.dolt/config_global.json | python3 -c "import sys,json; print(json.load(sys.stdin).get('user.creds',''))")
cp ~/.dolt/creds/*.jwk "$CREDS_DIR/"
echo "{\"user.creds\":\"$ACTIVE_KEY\",\"user.name\":\"repro\",\"user.email\":\"repro@test\"}" > "$CREDS_DIR/config_global.json"

echo "Running in Docker..."
docker run --rm \
  -v "$CREDS_DIR:/creds:ro" \
  dolthub/dolt-sql-server:latest \
  bash -c "
    # Copy creds (resolve symlinks, writable location)
    mkdir -p /var/lib/dolt/.dolt/creds
    cp /creds/*.jwk /var/lib/dolt/.dolt/creds/
    cp /creds/config_global.json /var/lib/dolt/.dolt/config_global.json

    echo '--- creds check ---'
    cd /var/lib/dolt && dolt creds check

    echo '--- clone (expected: works) ---'
    cd /var/lib/dolt && dolt clone $REMOTE $DB_NAME 2>&1 | tail -1

    echo '--- start server ---'
    cd /var/lib/dolt && dolt sql-server --host 0.0.0.0 --port 3306 --data-dir /var/lib/dolt &
    sleep 5

    echo '--- CALL dolt_fetch via Docker server (expected: PermissionDenied) ---'
    DOLT_CLI_PASSWORD='' dolt --host 127.0.0.1 --port 3306 --user root --no-tls sql -q \
      \"USE $DB_NAME; CALL dolt_fetch('origin', 'main')\" 2>&1 | tail -5
  "

rm -rf "$CREDS_DIR"

echo ""
echo "If local succeeded but Docker failed, the issue is in how"
echo "dolthub/dolt-sql-server handles JWK credentials for remote operations."
