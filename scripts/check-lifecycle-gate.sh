#!/usr/bin/env bash
#
# check-lifecycle-gate.sh — Landing 1 CI gate (spi-sqqero / spi-8j30wh)
#
# Fails when a *new* direct bead.status write lands outside pkg/lifecycle.
# Existing call sites are seeded into scripts/lifecycle-gate-allowlist.txt
# and grandfathered. Landing 2 will delete them; for now the gate only
# enforces "no new additions".
#
# Patterns matched (in *.go, excluding *_test.go, vendor/, and pkg/lifecycle/):
#   1. UpdateBead(...)  with a "status" key on the same line
#   2. .Status = "..."  string-literal assignment
#   3. "status": "..."  literal map/struct key with a string value
#
# Output format (one entry per matching line):
#   <path>:<trimmed-match-text>
#
# Line numbers are intentionally omitted so the allowlist stays stable
# when surrounding lines shift. Each unique (file, trimmed-line) tuple is
# one entry; duplicates within a file collapse via sort -u.
#
# Run locally:
#   bash scripts/check-lifecycle-gate.sh
#
# When migrating a grandfathered call site through pkg/lifecycle in
# Landing 2, *delete* the corresponding entry from the allowlist and
# re-run. When intentionally adding a new direct write (rare — should
# almost always go through lifecycle.RecordEvent), regenerate the
# allowlist with:
#   bash scripts/check-lifecycle-gate.sh --regenerate

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ALLOWLIST="$SCRIPT_DIR/lifecycle-gate-allowlist.txt"

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
  | xargs -0 grep -HE \
      -e 'UpdateBead[^"]*"status"' \
      -e '\.Status[[:space:]]*=[[:space:]]*"[^"]*"' \
      -e '"status":[[:space:]]*"' \
      2>/dev/null \
  | sed -E 's/^\.\///; s/[[:space:]]+/ /g; s/^([^:]+): */\1:/' \
  | sort -u \
  || true
}

REGENERATE=0
if [[ "${1:-}" == "--regenerate" ]]; then
  REGENERATE=1
fi

CURRENT="$(collect_matches)"

if [[ "$REGENERATE" -eq 1 ]]; then
  printf '%s\n' "$CURRENT" > "$ALLOWLIST"
  count="$(printf '%s\n' "$CURRENT" | grep -c . || true)"
  echo "lifecycle-gate: regenerated allowlist with $count entries → $ALLOWLIST"
  exit 0
fi

if [[ ! -f "$ALLOWLIST" ]]; then
  echo "lifecycle-gate: ERROR — allowlist not found at $ALLOWLIST" >&2
  echo "lifecycle-gate: run 'bash scripts/check-lifecycle-gate.sh --regenerate' to seed it." >&2
  exit 2
fi

# Diff allowlist (expected) against current (actual). New lines in the
# current set that aren't in the allowlist are violations.
DIFF_OUTPUT="$(diff <(sort -u "$ALLOWLIST") <(printf '%s\n' "$CURRENT") || true)"
ADDITIONS="$(printf '%s\n' "$DIFF_OUTPUT" | sed -n 's/^> //p')"
REMOVALS="$(printf '%s\n' "$DIFF_OUTPUT" | sed -n 's/^< //p')"

EXIT=0

if [[ -n "$ADDITIONS" ]]; then
  echo "lifecycle-gate: FAIL — new direct bead.status writes detected outside pkg/lifecycle:" >&2
  echo "" >&2
  printf '%s\n' "$ADDITIONS" | sed 's/^/  /' >&2
  echo "" >&2
  echo "Status mutations must flow through pkg/lifecycle.RecordEvent (see pkg/lifecycle/README.md)." >&2
  echo "If you are intentionally migrating a grandfathered call site in Landing 2, update the" >&2
  echo "allowlist by removing its entry. If you have a sanctioned new direct write (rare), run:" >&2
  echo "  bash scripts/check-lifecycle-gate.sh --regenerate" >&2
  EXIT=1
fi

if [[ -n "$REMOVALS" ]]; then
  # Removals are expected during Landing 2 migrations. Don't fail on them,
  # but note them so a stale allowlist gets noticed in review.
  echo "lifecycle-gate: note — entries in allowlist no longer match (consider trimming):" >&2
  printf '%s\n' "$REMOVALS" | sed 's/^/  /' >&2
fi

if [[ "$EXIT" -eq 0 && -z "$ADDITIONS" && -z "$REMOVALS" ]]; then
  count="$(printf '%s\n' "$CURRENT" | grep -c . || true)"
  echo "lifecycle-gate: ok ($count grandfathered entries match allowlist)"
fi

exit "$EXIT"
