#!/usr/bin/env bash
#
# check-lifecycle-gate.sh — lifecycle CI gate (spi-sqqero)
#
# Fails when *any* direct bead.status write is detected outside
# pkg/lifecycle. pkg/lifecycle is the sole sanctioned writer of
# bead.status; status mutations elsewhere must flow through
# lifecycle.RecordEvent (see pkg/lifecycle/README.md).
#
# History:
#   - Landing 1 (spi-91ohmn): introduced the gate in soft mode against
#     scripts/lifecycle-gate-allowlist.txt — only NEW direct writes failed.
#   - Landing 2 (spi-g8a1nz): hardened to fail on any match. The
#     grandfathered allowlist is gone; there is no soft mode.
#
# Patterns matched (in *.go, excluding *_test.go, vendor/, and pkg/lifecycle/):
#   1. UpdateBead(...)  with a "status" key on the same line
#   2. .Status = "..."  string-literal assignment
#   3. "status": "..."  literal map/struct key with a string value
#
# Run locally:
#   bash scripts/check-lifecycle-gate.sh
#   make lifecycle-gate

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$REPO_ROOT"

# Find candidate Go files. Excludes:
#   - test files (*_test.go)
#   - vendored dependencies (vendor/)
#   - the lifecycle package itself (pkg/lifecycle/) — it owns status writes
#   - hidden dirs (.git, etc.)
collect_matches() {
  # -E: extended regex; combined alternation matches any of the three patterns.
  # The trailing `|| true` keeps `set -e` from killing us on no-match.
  find . \
    -type f -name '*.go' \
    -not -name '*_test.go' \
    -not -path './vendor/*' \
    -not -path './pkg/lifecycle/*' \
    -not -path './.git/*' \
    -print0 \
  | xargs -0 grep -HnE \
      -e 'UpdateBead[^"]*"status"' \
      -e '\.Status[[:space:]]*=[[:space:]]*"[^"]*"' \
      -e '"status":[[:space:]]*"' \
      2>/dev/null \
  | sed -E 's/^\.\///' \
  | sort -u \
  || true
}

CURRENT="$(collect_matches)"

if [[ -n "$CURRENT" ]]; then
  count="$(printf '%s\n' "$CURRENT" | grep -c . || true)"
  echo "lifecycle-gate: FAIL — $count direct bead.status write(s) detected outside pkg/lifecycle:" >&2
  echo "" >&2
  printf '%s\n' "$CURRENT" | sed 's/^/  /' >&2
  echo "" >&2
  echo "Status mutations must flow through pkg/lifecycle.RecordEvent." >&2
  echo "See pkg/lifecycle/README.md for migration guidance." >&2
  exit 1
fi

echo "lifecycle-gate: ok (no direct bead.status writes outside pkg/lifecycle)"
