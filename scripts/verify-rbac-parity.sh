#!/usr/bin/env bash
# verify-rbac-parity.sh — Strict equality check between the helm chart's
# operator ClusterRole and the controller-gen–generated
# operator/config/rbac/role.yaml.
#
# Why strict (not superset): the chart's ClusterRole is hand-maintained,
# and the spi-p5nr8i class was "markers say the operator needs it; chart
# doesn't ship it" — a subset violation. The opposite direction (chart
# over-grants beyond what markers declare) is also drift: it bypasses
# the audit trail markers provide and invites scope creep. Strict
# equality flags both directions.
#
# Hard-coded paths — this script assumes it is run from the repo root
# (see spire.yaml).
set -euo pipefail

CHART_DIR="helm/spire"
GEN_ROLE="operator/config/rbac/role.yaml"

if [ ! -d "$CHART_DIR" ]; then
  echo "ERROR: chart dir $CHART_DIR not found (run from repo root)" >&2
  exit 2
fi
if [ ! -f "$GEN_ROLE" ]; then
  echo "ERROR: generated role $GEN_ROLE not found; run 'make manifests' in operator/" >&2
  exit 2
fi

command -v helm >/dev/null 2>&1 || {
  echo "ERROR: helm not found on PATH" >&2
  exit 2
}
command -v python3 >/dev/null 2>&1 || {
  echo "ERROR: python3 not found on PATH" >&2
  exit 2
}

tmp_chart_raw=$(mktemp)
tmp_chart=$(mktemp)
tmp_gen=$(mktemp)
trap 'rm -f "$tmp_chart_raw" "$tmp_chart" "$tmp_gen"' EXIT

# Render the chart with default values to a temp file so the
# normalization python script can read it from disk (mixing a pipe
# into stdin with an inline script via heredoc confuses bash).
helm template "$CHART_DIR" > "$tmp_chart_raw"

# Normalize the chart's operator ClusterRole rules into a
# deterministic, comparable YAML shape. "operator ClusterRole" is
# matched by metadata.name containing "operator" — the chart scopes
# the name to the release, so the actual value is
# "<release>-operator".
python3 - "$tmp_chart_raw" "$tmp_chart" <<'PY'
import sys, yaml

in_path, out_path = sys.argv[1], sys.argv[2]
with open(in_path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
roles = [
    d for d in docs
    if d.get("kind") == "ClusterRole"
    and "operator" in d.get("metadata", {}).get("name", "")
]
if not roles:
    sys.stderr.write("ERROR: no operator ClusterRole found in chart output\n")
    sys.exit(2)
if len(roles) > 1:
    names = ", ".join(r["metadata"]["name"] for r in roles)
    sys.stderr.write(
        f"ERROR: multiple operator ClusterRoles in chart output: {names}\n"
    )
    sys.exit(2)

def norm_rule(r):
    return {
        "apiGroups": sorted(r.get("apiGroups", []) or [""]),
        "resources": sorted(r.get("resources", []) or []),
        "verbs": sorted(r.get("verbs", []) or []),
    }

rules = [norm_rule(r) for r in (roles[0].get("rules") or [])]
rules.sort(key=lambda r: (
    ",".join(r["apiGroups"]),
    ",".join(r["resources"]),
    ",".join(r["verbs"]),
))
with open(out_path, "w") as f:
    yaml.safe_dump(rules, f, sort_keys=True, default_flow_style=False)
PY

# Normalize the generated role.yaml the same way so diff is
# meaningful.
python3 - "$GEN_ROLE" "$tmp_gen" <<'PY'
import sys, yaml

in_path, out_path = sys.argv[1], sys.argv[2]
with open(in_path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
roles = [d for d in docs if d.get("kind") == "ClusterRole"]
if not roles:
    sys.stderr.write(f"ERROR: no ClusterRole found in {in_path}\n")
    sys.exit(2)
if len(roles) > 1:
    sys.stderr.write(f"ERROR: multiple ClusterRoles in {in_path}\n")
    sys.exit(2)

def norm_rule(r):
    return {
        "apiGroups": sorted(r.get("apiGroups", []) or [""]),
        "resources": sorted(r.get("resources", []) or []),
        "verbs": sorted(r.get("verbs", []) or []),
    }

rules = [norm_rule(r) for r in (roles[0].get("rules") or [])]
rules.sort(key=lambda r: (
    ",".join(r["apiGroups"]),
    ",".join(r["resources"]),
    ",".join(r["verbs"]),
))
with open(out_path, "w") as f:
    yaml.safe_dump(rules, f, sort_keys=True, default_flow_style=False)
PY

if ! diff -u "$tmp_gen" "$tmp_chart"; then
  echo "" >&2
  echo "ERROR: Helm chart operator ClusterRole does not match $GEN_ROLE" >&2
  echo "  Left  (-): $GEN_ROLE  (controller-gen output from +kubebuilder:rbac markers)" >&2
  echo "  Right (+): helm/spire chart operator ClusterRole (hand-maintained)" >&2
  echo "Reconcile by updating the chart template to match the generated role." >&2
  exit 1
fi

echo "verify-rbac-parity: OK (chart and generated role agree)"
