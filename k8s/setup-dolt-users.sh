#!/usr/bin/env bash
# setup-dolt-users.sh — generates random passwords for dolt SQL users
# and stores them in the spire-credentials k8s secret.
# Run once after the dolt server is up, or on password rotation.
set -euo pipefail

NAMESPACE="${1:-spire}"
DOLT_HOST="${DOLT_HOST:-spire-dolt}"
DOLT_PORT="${DOLT_PORT:-3306}"

gen_password() { openssl rand -base64 24 | tr -d '/+=' | head -c 24; }

STEWARD_PASS=$(gen_password)
WIZARD_PASS=$(gen_password)
ARCHIVIST_PASS=$(gen_password)
JB_PASS=$(gen_password)

echo "=== Creating dolt SQL users ==="

# Create users via SQL (root has no password by default)
kubectl exec -n "$NAMESPACE" deploy/spire-dolt -c dolt -- sh -c "
DOLT_CLI_PASSWORD='' dolt --host 127.0.0.1 --port $DOLT_PORT --user root --no-tls sql -q \"
CREATE USER IF NOT EXISTS 'steward'@'%' IDENTIFIED BY '$STEWARD_PASS';
CREATE USER IF NOT EXISTS 'wizard'@'%' IDENTIFIED BY '$WIZARD_PASS';
CREATE USER IF NOT EXISTS 'archivist'@'%' IDENTIFIED BY '$ARCHIVIST_PASS';
CREATE USER IF NOT EXISTS 'jb'@'%' IDENTIFIED BY '$JB_PASS';
GRANT ALL ON *.* TO 'steward'@'%';
GRANT ALL ON *.* TO 'wizard'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE, EXECUTE ON *.* TO 'archivist'@'%';
GRANT ALL ON *.* TO 'jb'@'%';
\"
"

echo "=== Storing passwords in k8s secret ==="

kubectl patch secret spire-credentials -n "$NAMESPACE" --type merge -p "{
  \"stringData\": {
    \"DOLT_REMOTE_USER_STEWARD\": \"steward\",
    \"DOLT_REMOTE_PASSWORD_STEWARD\": \"$STEWARD_PASS\",
    \"DOLT_REMOTE_USER_WIZARD\": \"wizard\",
    \"DOLT_REMOTE_PASSWORD_WIZARD\": \"$WIZARD_PASS\",
    \"DOLT_REMOTE_USER_ARCHIVIST\": \"archivist\",
    \"DOLT_REMOTE_PASSWORD_ARCHIVIST\": \"$ARCHIVIST_PASS\",
    \"DOLT_REMOTE_USER_JB\": \"jb\",
    \"DOLT_REMOTE_PASSWORD_JB\": \"$JB_PASS\"
  }
}"

echo ""
echo "=== Done ==="
echo "Users: steward, wizard, archivist, jb"
echo "Passwords stored in secret/$NAMESPACE/spire-credentials"
echo ""
echo "Your local credentials (save these):"
echo "  DOLT_REMOTE_USER=jb"
echo "  DOLT_REMOTE_PASSWORD=$JB_PASS"
echo ""
echo "To connect locally:"
echo "  kubectl port-forward -n $NAMESPACE svc/spire-dolt 50051:50051"
echo "  DOLT_REMOTE_PASSWORD=$JB_PASS dolt clone --user jb http://localhost:50051/spi"
