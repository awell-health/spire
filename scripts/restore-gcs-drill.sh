#!/usr/bin/env bash
# Wraps steps 3.1–3.5 of docs/runbooks/gcs-restore.md: create a test
# namespace, provision a blank PVC sized for the production dolt DB,
# stage GCS credentials, run a one-shot Pod that does
# `dolt backup restore`, and verify the restored commit hash.
#
# This script does NOT install the helm chart or run the bead-graph or
# gateway smoke tests — those steps require operator judgement (selecting
# spot-check IDs, picking a disposable bead for the mutation test, etc.)
# and are documented in §3.6 onward of the runbook.
#
# Idempotency: re-running with the same flags is safe up to the point
# where the PVC already has a populated `<DB>/.dolt` directory. The
# restore Pod refuses to overwrite a non-empty PVC; wipe the namespace
# (or recreate the PVC) to retry.
set -euo pipefail

NAMESPACE=""
BUCKET=""
PREFIX=""
REMOTE_NAME="gcs-backup"
DB_NAME="spi"
PVC_SIZE="5Gi"
DOLT_IMAGE="dolthub/dolt-sql-server:latest"
GCP_SECRET_NAME="spire-gcp-sa"
GCP_SECRET_FILE=""
DRY_RUN=0

usage() {
  cat <<EOF
Usage: $0 --namespace <ns> --bucket <bucket> [options]

Required:
  --namespace <ns>          Test namespace to create (e.g. spire-restore-drill)
  --bucket <bucket>         Production backup.gcs.bucket value

Optional:
  --prefix <prefix>         Production backup.gcs.prefix (default: empty)
  --remote-name <name>      backup.remoteName (default: $REMOTE_NAME)
  --db-name <name>          beads.database || beads.prefix (default: $DB_NAME)
  --pvc-size <size>         Match production dolt.storage.size (default: $PVC_SIZE)
  --dolt-image <image>      Dolt image (default: $DOLT_IMAGE)
  --gcp-secret-name <name>  Existing GCP SA Secret in --namespace (skip --gcp-secret-file)
  --gcp-secret-file <path>  Local SA JSON path to load into a new Secret named $GCP_SECRET_NAME
  --dry-run                 Print resources and commands without applying

Provide exactly one of --gcp-secret-name or --gcp-secret-file.
EOF
  exit 2
}

while [[ $# -gt 0 ]]; do
  # Accept both `--flag value` and `--flag=value` forms.
  case "$1" in
    --*=*) FLAG="${1%%=*}"; VALUE="${1#*=}"; shift; set -- "$FLAG" "$VALUE" "$@" ;;
  esac
  case "$1" in
    --namespace) NAMESPACE="${2:?}"; shift 2 ;;
    --bucket) BUCKET="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:-}"; shift 2 ;;
    --remote-name) REMOTE_NAME="${2:?}"; shift 2 ;;
    --db-name) DB_NAME="${2:?}"; shift 2 ;;
    --pvc-size) PVC_SIZE="${2:?}"; shift 2 ;;
    --dolt-image) DOLT_IMAGE="${2:?}"; shift 2 ;;
    --gcp-secret-name) GCP_SECRET_NAME="${2:?}"; shift 2 ;;
    --gcp-secret-file) GCP_SECRET_FILE="${2:?}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown flag: $1" >&2; usage ;;
  esac
done

if [[ -z "$NAMESPACE" || -z "$BUCKET" ]]; then
  echo "ERROR: --namespace and --bucket are required" >&2
  usage
fi

if [[ -n "$GCP_SECRET_FILE" && ! -f "$GCP_SECRET_FILE" ]]; then
  echo "ERROR: --gcp-secret-file does not exist: $GCP_SECRET_FILE" >&2
  exit 1
fi

run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "+ $*"
  else
    "$@"
  fi
}

apply() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "--- would apply ---"
    cat
    echo "--- end ---"
  else
    kubectl apply -n "$NAMESPACE" -f -
  fi
}

create_test_namespace() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "+ kubectl create namespace $NAMESPACE (skipped if exists)"
    return
  fi
  if kubectl get ns "$NAMESPACE" >/dev/null 2>&1; then
    echo "namespace $NAMESPACE already exists"
  else
    kubectl create namespace "$NAMESPACE"
  fi
}

create_test_pvc() {
  cat <<EOF | apply
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-spire-dolt-0
  labels:
    app.kubernetes.io/name: spire-dolt
    app.kubernetes.io/component: beads-storage
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: $PVC_SIZE
EOF
}

stage_gcp_credentials() {
  if [[ -n "$GCP_SECRET_FILE" ]]; then
    if [[ "$DRY_RUN" -eq 1 ]]; then
      echo "+ kubectl -n $NAMESPACE create secret generic $GCP_SECRET_NAME --from-file=key.json=$GCP_SECRET_FILE"
    else
      kubectl -n "$NAMESPACE" create secret generic "$GCP_SECRET_NAME" \
        --from-file=key.json="$GCP_SECRET_FILE" \
        --dry-run=client -o yaml | kubectl apply -n "$NAMESPACE" -f -
    fi
  else
    if [[ "$DRY_RUN" -eq 0 ]] && ! kubectl -n "$NAMESPACE" get secret "$GCP_SECRET_NAME" >/dev/null 2>&1; then
      echo "ERROR: secret $GCP_SECRET_NAME not found in $NAMESPACE; pass --gcp-secret-file or pre-create it" >&2
      exit 1
    fi
  fi
}

spin_up_dolt_pod() {
  cat <<EOF | apply
apiVersion: v1
kind: Pod
metadata:
  name: dolt-restore
spec:
  restartPolicy: Never
  containers:
    - name: dolt
      image: $DOLT_IMAGE
      command: ["bash", "-c"]
      args:
        - |
          set -euo pipefail
          cd /var/lib/dolt
          if [ -d "${DB_NAME}/.dolt" ]; then
            echo "PVC already has ${DB_NAME}/.dolt — refusing to overwrite. Wipe the PVC and retry."
            exit 1
          fi
          BACKUP_URL="gs://${BUCKET}/${PREFIX}"
          echo "restoring \$BACKUP_URL into ${DB_NAME}"
          dolt backup restore "\$BACKUP_URL" "${DB_NAME}"
          cd "${DB_NAME}"
          echo "restored HEAD:"
          dolt log --oneline -n 1
      env:
        - name: DOLT_ROOT_PATH
          value: /var/lib/dolt
        - name: GOOGLE_APPLICATION_CREDENTIALS
          value: /var/secrets/gcp/key.json
      volumeMounts:
        - name: data
          mountPath: /var/lib/dolt
        - name: gcp-sa
          mountPath: /var/secrets/gcp
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data-spire-dolt-0
    - name: gcp-sa
      secret:
        secretName: $GCP_SECRET_NAME
        defaultMode: 0400
EOF
}

verify_commit_hash() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "+ kubectl wait --for=condition=Ready pod/dolt-restore -n $NAMESPACE --timeout=300s"
    echo "+ kubectl logs -n $NAMESPACE pod/dolt-restore"
    return
  fi
  echo "waiting for dolt-restore Pod to finish (status.phase=Succeeded)..."
  for _ in $(seq 1 60); do
    PHASE=$(kubectl get pod dolt-restore -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    case "$PHASE" in
      Succeeded) break ;;
      Failed) echo "ERROR: dolt-restore Pod failed"; kubectl logs -n "$NAMESPACE" pod/dolt-restore || true; exit 1 ;;
    esac
    sleep 5
  done
  if [[ "$PHASE" != "Succeeded" ]]; then
    echo "ERROR: dolt-restore Pod did not reach Succeeded within 5m (current: $PHASE)" >&2
    kubectl logs -n "$NAMESPACE" pod/dolt-restore || true
    exit 1
  fi
  echo "--- restore log ---"
  kubectl logs -n "$NAMESPACE" pod/dolt-restore
  echo "--- end ---"
}

print_validation_commands() {
  cat <<EOF

--- next steps (manual, see docs/runbooks/gcs-restore.md §3.6 onward) ---

1. Install the chart into the test namespace, reusing the restored PVC:
     helm install spire-drill ./helm/spire -n $NAMESPACE -f /tmp/drill-values.yaml

2. Validate bead-graph integrity (§4):
     DOLT="kubectl exec -n $NAMESPACE spire-dolt-0 -c dolt -- \\
       dolt --host 127.0.0.1 --port 3306 --user root --no-tls -p '' sql -q"
     \$DOLT "USE $DB_NAME; SELECT 'issues' AS t, COUNT(*) FROM issues
              UNION ALL SELECT 'comments',     COUNT(*) FROM comments
              UNION ALL SELECT 'dependencies', COUNT(*) FROM dependencies;"

3. Smoke-test the gateway (§5):
     kubectl port-forward -n $NAMESPACE svc/spire-gateway 3030:3030 &
     curl -fsS -H "Authorization: Bearer \$API_TOKEN" http://127.0.0.1:3030/api/v1/tower

4. Tear down (§8):
     helm uninstall spire-drill -n $NAMESPACE
     kubectl delete namespace $NAMESPACE
EOF
}

create_test_namespace
create_test_pvc
stage_gcp_credentials
spin_up_dolt_pod
verify_commit_hash
print_validation_commands
