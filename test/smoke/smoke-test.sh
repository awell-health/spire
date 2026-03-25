#!/bin/bash
set -e

PASS=0
FAIL=0
TESTS=()

pass() {
  echo "  ✓ $1"
  PASS=$((PASS + 1))
  TESTS+=("PASS: $1")
}

fail() {
  echo "  ✗ $1: $2"
  FAIL=$((FAIL + 1))
  TESTS+=("FAIL: $1: $2")
}

assert_success() {
  local desc="$1"
  shift
  if output=$("$@" 2>&1); then
    pass "$desc"
  else
    fail "$desc" "$output"
  fi
}

assert_contains() {
  local desc="$1"
  local expected="$2"
  shift 2
  if output=$("$@" 2>&1); then
    if echo "$output" | grep -q "$expected"; then
      pass "$desc"
    else
      fail "$desc" "expected '$expected' in output: $output"
    fi
  else
    fail "$desc" "command failed: $output"
  fi
}

assert_creates_id() {
  local desc="$1"
  shift
  if output=$("$@" 2>&1); then
    if echo "$output" | grep -qE '^[a-z]+-[a-z0-9]+$'; then
      pass "$desc (created: $output)"
    else
      fail "$desc" "expected bead ID, got: $output"
    fi
  else
    fail "$desc" "command failed: $output"
  fi
}

echo "=== Spire Smoke Test ==="
echo ""

# --- Version ---
echo "Version:"
assert_contains "spire version" "spire" spire version
echo ""

# --- Tower ---
echo "Tower:"
# Provide dolt identity non-interactively
dolt config --global --add user.name "smoke-test" 2>/dev/null || true
dolt config --global --add user.email "smoke@test.local" 2>/dev/null || true

assert_success "tower create" spire tower create --name smoke
assert_contains "tower list shows smoke" "smoke" spire tower list
echo ""

# --- Repo ---
echo "Repo:"
mkdir -p /home/linuxbrew/test-repo && cd /home/linuxbrew/test-repo
assert_success "repo add" spire repo add --repo-url https://example.com/test.git --prefix tst
assert_contains "repo list shows tst" "tst" spire repo list
echo ""

# --- Services ---
echo "Services:"
assert_success "spire up" spire up
assert_contains "status shows dolt running" "running" spire status
echo ""

# --- Design beads ---
echo "Design beads:"
assert_creates_id "spire design creates bead" spire design "Test design bead" -p 2
echo ""

# --- File beads ---
echo "Filing work:"
assert_creates_id "spire file task" spire file "Test task" -t task -p 2
assert_creates_id "spire file bug" spire file "Test bug" -t bug -p 3
assert_creates_id "spire file epic" spire file "Test epic" -t epic -p 1
echo ""

# --- Board ---
echo "Board:"
assert_contains "board shows task" "Test task" spire board --json
assert_contains "board --json has ready key" "ready" spire board --json
echo ""

# --- Focus ---
echo "Focus:"
TASK_ID=$(spire file "Focus target" -t task -p 2 2>&1)
assert_contains "focus shows task" "Focus target" spire focus "$TASK_ID"
echo ""

# --- Claim ---
echo "Claim:"
assert_success "claim task" spire claim "$TASK_ID"
assert_contains "board shows claimed task" "$TASK_ID" spire board --json
echo ""

# --- Repo remove ---
echo "Repo remove:"
assert_success "repo remove by prefix" spire repo remove tst
echo ""

# --- Shutdown ---
echo "Shutdown:"
assert_success "spire down" spire down
echo ""

# --- Summary ---
echo "=== Results ==="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo ""

if [ $FAIL -gt 0 ]; then
  echo "FAILURES:"
  for t in "${TESTS[@]}"; do
    if [[ "$t" == FAIL* ]]; then
      echo "  $t"
    fi
  done
  exit 1
fi

echo "All tests passed."
