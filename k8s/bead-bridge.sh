#!/usr/bin/env bash
# bead-bridge: reads ready beads from local dolt and creates SpireWorkload CRs.
# Runs as a background loop in the steward pod.
set -uo pipefail

: "${BRIDGE_INTERVAL:=30}"
: "${BRIDGE_NAMESPACE:=spire}"

log() { echo "[bridge] $(date -u +%H:%M:%S) $*"; }

create_workload() {
    local id="$1" title="$2" priority="$3" btype="$4"

    # k8s name: lowercase, dots→hyphens
    local name
    name="bead-$(echo "$id" | tr '[:upper:]' '[:lower:]' | tr '.' '-')"

    # Extract prefix (e.g., "spi" from "spi-abc")
    local prefix
    prefix="$(echo "$id" | cut -d- -f1)"

    # Check if already exists
    if kubectl get spireworkload "$name" -n "$BRIDGE_NAMESPACE" &>/dev/null; then
        return 0
    fi

    cat <<EOF | kubectl apply -f - 2>&1
apiVersion: spire.awell.io/v1alpha1
kind: SpireWorkload
metadata:
  name: ${name}
  namespace: ${BRIDGE_NAMESPACE}
  labels:
    spire.awell.io/bead-id: "${id}"
    spire.awell.io/prefix: "${prefix}"
spec:
  beadId: "${id}"
  title: "$(echo "$title" | sed 's/"/\\"/g')"
  priority: ${priority}
  type: "${btype}"
  prefixes:
    - "${prefix}-"
EOF
}

log "starting (interval=${BRIDGE_INTERVAL}s)"

while true; do
    # Get ready beads
    beads=$(cd /data && bd ready --json 2>/dev/null) || { sleep "$BRIDGE_INTERVAL"; continue; }

    count=$(echo "$beads" | jq -r 'length')
    created=0

    for i in $(seq 0 $((count - 1))); do
        # Skip message beads (labels contain "msg")
        labels=$(echo "$beads" | jq -r ".[$i].labels // [] | join(\",\")")
        case "$labels" in *msg*) continue ;; esac

        id=$(echo "$beads" | jq -r ".[$i].id")
        title=$(echo "$beads" | jq -r ".[$i].title")
        priority=$(echo "$beads" | jq -r ".[$i].priority // 3")
        btype=$(echo "$beads" | jq -r ".[$i].issue_type // \"task\"")

        if create_workload "$id" "$title" "$priority" "$btype" >/dev/null 2>&1; then
            created=$((created + 1))
        fi
    done

    if [ "$created" -gt 0 ]; then
        log "created $created SpireWorkload(s) from $count ready bead(s)"
    fi

    sleep "$BRIDGE_INTERVAL"
done
