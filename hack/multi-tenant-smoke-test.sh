#!/usr/bin/env bash
# Multi-tenant smoke test for the Spire helm chart.
#
# Installs two independent releases (spire-a and spire-b) into two
# separate namespaces using different bead prefixes, then verifies that
# pods come up and the two releases don't share state.
#
# Usage:
#   IMAGE_TAG=v0.42.0 bash hack/multi-tenant-smoke-test.sh
#   KEEP_NAMESPACES=1 bash hack/multi-tenant-smoke-test.sh   # skip cleanup
#
# Optional env vars:
#   IMAGE_TAG           image tag for steward/agent (default: latest)
#   DOLTHUB_REMOTE_A    DoltHub remote path for release A (enables real sync)
#   DOLTHUB_REMOTE_B    DoltHub remote path for release B
#   DOLT_CREDS_FILE     path to a .jwk file from ~/.dolt/creds
#   DOLT_CREDS_KEY_ID   key id (filename without .jwk) matching the jwk
#   ANTHROPIC_API_KEY   anthropic key (used if set)
#   KEEP_NAMESPACES     set to 1 to skip teardown
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="${ROOT}/helm/spire"

REL_A="spire-a"
REL_B="spire-b"
NS_A="spire-a"
NS_B="spire-b"
PREFIX_A="a"
PREFIX_B="b"
TIMEOUT="${TIMEOUT:-180s}"
IMAGE_TAG="${IMAGE_TAG:-latest}"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "=== $* ==="; }

# --- prereqs -----------------------------------------------------------------
step "Checking prerequisites"
command -v helm >/dev/null || fail "helm not found on PATH"
command -v kubectl >/dev/null || fail "kubectl not found on PATH"
kubectl config current-context >/dev/null 2>&1 || fail "kubectl has no current context"

CTX="$(kubectl config current-context)"
echo "kubectl context: $CTX"
echo "helm version:    $(helm version --short)"
echo "chart path:      $CHART"
echo "image tag:       $IMAGE_TAG"

# --- preflight: no hardcoded prefix/namespace in templates -------------------
step "Preflight: scanning templates for hardcoded 'spi' prefix / 'spire' namespace"
TEMPLATES="$CHART/templates"

# Look for a string literal "spi" (the default prefix) — must be absent so
# templates rely on .Values.beads.prefix instead.
if grep -R --include='*.yaml' -n '"spi"' "$TEMPLATES"; then
  fail "templates contain hardcoded \"spi\" literal — must be parameterized"
fi

# Look for `namespace: spire` as a literal YAML assignment — must be absent so
# resources use .Values.namespace / .Release.Namespace instead.
if grep -R --include='*.yaml' -nE '^\s*namespace:\s*spire\s*$' "$TEMPLATES"; then
  fail "templates contain hardcoded 'namespace: spire' — must be parameterized"
fi

echo "preflight: OK"

# --- install helper ----------------------------------------------------------
install_release() {
  local rel="$1" ns="$2" prefix="$3"
  step "Installing $rel into namespace $ns (prefix=$prefix)"

  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -

  # Pre-create dolt creds secret if the test runner supplied one.
  if [[ -n "${DOLT_CREDS_FILE:-}" && -n "${DOLT_CREDS_KEY_ID:-}" ]]; then
    kubectl -n "$ns" delete secret dolt-creds --ignore-not-found
    kubectl -n "$ns" create secret generic dolt-creds \
      --from-file="${DOLT_CREDS_KEY_ID}.jwk=${DOLT_CREDS_FILE}"
  fi

  local set_args=(
    --set "namespace=$ns"
    --set "createNamespace=false"
    --set "beads.prefix=$prefix"
    --set "images.steward.tag=$IMAGE_TAG"
    --set "images.agent.tag=$IMAGE_TAG"
  )

  # Release-specific DoltHub remote.
  local remote_var="DOLTHUB_REMOTE_${rel##*-}"  # DOLTHUB_REMOTE_A / _B
  remote_var="$(echo "$remote_var" | tr '[:lower:]' '[:upper:]')"
  local remote="${!remote_var:-}"
  if [[ -n "$remote" ]]; then
    set_args+=(--set "dolthub.remote=$remote")
  fi
  if [[ -n "${DOLT_CREDS_KEY_ID:-}" ]]; then
    set_args+=(--set "dolthub.credsSecretName=dolt-creds")
    set_args+=(--set "dolthub.keyId=$DOLT_CREDS_KEY_ID")
  fi
  if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
    set_args+=(--set "anthropic.apiKey=$ANTHROPIC_API_KEY")
  fi

  helm upgrade --install "$rel" "$CHART" \
    --namespace "$ns" \
    "${set_args[@]}" \
    --wait --timeout "$TIMEOUT"
}

install_release "$REL_A" "$NS_A" "$PREFIX_A"
install_release "$REL_B" "$NS_B" "$PREFIX_B"

# --- verify isolation --------------------------------------------------------
step "Verifying both releases have pods Running"
for ns in "$NS_A" "$NS_B"; do
  echo "-- pods in $ns --"
  kubectl get pods -n "$ns" -o wide
  not_running="$(kubectl get pods -n "$ns" \
    -o jsonpath='{range .items[?(@.status.phase!="Running")]}{.metadata.name}{"\n"}{end}')"
  if [[ -n "$not_running" ]]; then
    fail "non-Running pods in $ns: $not_running"
  fi
done

step "Verifying PVCs are namespace-scoped"
kubectl get pvc -n "$NS_A" -o name | grep -q spire-dolt-data \
  || fail "expected dolt PVC in $NS_A"
kubectl get pvc -n "$NS_B" -o name | grep -q spire-dolt-data \
  || fail "expected dolt PVC in $NS_B"

step "Verifying BEADS_PREFIX differs per release"
got_a="$(kubectl exec -n "$NS_A" deploy/spire-steward -c steward -- \
  sh -c 'printf %s "$BEADS_PREFIX"')"
got_b="$(kubectl exec -n "$NS_B" deploy/spire-steward -c steward -- \
  sh -c 'printf %s "$BEADS_PREFIX"')"
echo "$NS_A BEADS_PREFIX='$got_a'"
echo "$NS_B BEADS_PREFIX='$got_b'"
[[ "$got_a" == "$PREFIX_A" ]] || fail "$NS_A BEADS_PREFIX='$got_a', want '$PREFIX_A'"
[[ "$got_b" == "$PREFIX_B" ]] || fail "$NS_B BEADS_PREFIX='$got_b', want '$PREFIX_B'"

step "Verifying operator scopes to its own namespace"
args_a="$(kubectl get deploy/spire-operator -n "$NS_A" \
  -o jsonpath='{.spec.template.spec.containers[0].args}')"
args_b="$(kubectl get deploy/spire-operator -n "$NS_B" \
  -o jsonpath='{.spec.template.spec.containers[0].args}')"
echo "$NS_A operator args: $args_a"
echo "$NS_B operator args: $args_b"
echo "$args_a" | grep -q -- "--namespace=$NS_A" \
  || fail "$NS_A operator does not target its own namespace"
echo "$args_b" | grep -q -- "--namespace=$NS_B" \
  || fail "$NS_B operator does not target its own namespace"

step "Verifying operator logs don't reference the other namespace"
if kubectl logs -n "$NS_A" deploy/spire-operator --tail=200 2>/dev/null | grep -q "$NS_B"; then
  fail "$NS_A operator logs reference $NS_B"
fi
if kubectl logs -n "$NS_B" deploy/spire-operator --tail=200 2>/dev/null | grep -q "$NS_A"; then
  fail "$NS_B operator logs reference $NS_A"
fi

step "Verifying secrets are per-namespace (no cross-reference)"
# Secrets with the same name in different namespaces are distinct objects —
# the isolation check is that neither release references secrets from the
# other namespace via secretKeyRef.
refs_a="$(kubectl get deploy -n "$NS_A" -o json \
  | grep -oE 'secretKeyRef[^}]*namespace[^}]*' || true)"
refs_b="$(kubectl get deploy -n "$NS_B" -o json \
  | grep -oE 'secretKeyRef[^}]*namespace[^}]*' || true)"
if echo "$refs_a" | grep -q "$NS_B"; then
  fail "$NS_A deploys reference secrets from $NS_B"
fi
if echo "$refs_b" | grep -q "$NS_A"; then
  fail "$NS_B deploys reference secrets from $NS_A"
fi

# --- cleanup -----------------------------------------------------------------
if [[ "${KEEP_NAMESPACES:-0}" == "1" ]]; then
  echo
  echo "KEEP_NAMESPACES=1 — leaving $NS_A and $NS_B in place for debugging"
else
  step "Cleaning up"
  helm uninstall "$REL_A" -n "$NS_A" || true
  helm uninstall "$REL_B" -n "$NS_B" || true
  kubectl delete namespace "$NS_A" "$NS_B" --ignore-not-found --wait=false
fi

echo
echo "PASS: multi-tenant smoke test"
