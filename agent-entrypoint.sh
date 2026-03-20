#!/usr/bin/env bash
set -uo pipefail

AGENT_NAME="${SPIRE_AGENT_NAME:-}"
WORKSPACE_DIR="${SPIRE_WORKSPACE_DIR:-/workspace}"
STATE_DIR="${SPIRE_STATE_DIR:-/data}"
COMMS_DIR="${SPIRE_COMMS_DIR:-/comms}"
AGENT_PREFIX="${SPIRE_AGENT_PREFIX:-wrk}"
REPO_URL="${SPIRE_REPO_URL:-}"
CLONE_BRANCH="${SPIRE_REPO_BRANCH:-main}"
BASE_BRANCH="${SPIRE_REPO_BRANCH:-main}"
BRANCH_PATTERN="feat/{bead-id}"
BEAD_ID="${SPIRE_BEAD_ID:-}"
ASSIGNMENT_MESSAGE_ID=""

MODEL="claude-sonnet-4-6"
MAX_TURNS="50"
TIMEOUT_RAW="30m"
INSTALL_CMD=""
TEST_CMD=""
BUILD_CMD=""
LINT_CMD=""

RUN_RESULT="running"
RUN_SUMMARY=""
BRANCH_NAME=""
COMMIT_SHA=""
BEAD_TITLE=""
TESTS_PASSED="false"
STARTED_AT="$(date -u +%FT%TZ)"
CLAUDE_STARTED_AT=""
EXIT_CODE=0

AGENT_PID=""
HEARTBEAT_PID=""
MONITOR_PID=""

INBOX_PATH="$COMMS_DIR/inbox.json"
RESULT_PATH="$COMMS_DIR/result.json"
ALIVE_PATH="$COMMS_DIR/wizard-alive"
STOP_PATH="$COMMS_DIR/stop"
STOP_REQUESTED_PATH="$COMMS_DIR/stop.requested"
STEER_PATH="$COMMS_DIR/steer"
STEER_LOG="$COMMS_DIR/steer.log"
AGENT_LOG="$COMMS_DIR/agent.log"
PROMPT_FILE="$COMMS_DIR/prompt.txt"
FOCUS_FILE="$COMMS_DIR/focus.txt"
BEAD_JSON_PATH="$COMMS_DIR/bead.json"
REPO_CONFIG_PATH="$COMMS_DIR/repo-config.txt"

log() {
  printf '[agent] %s\n' "$*" >&2
}

# elapsed_since returns seconds since an ISO timestamp.
elapsed_since() {
  local start_epoch now_epoch
  start_epoch="$(date -d "$1" +%s 2>/dev/null || date -jf "%FT%TZ" "$1" +%s 2>/dev/null || echo 0)"
  now_epoch="$(date +%s)"
  echo $(( now_epoch - start_epoch ))
}

fatal() {
  RUN_RESULT="${RUN_RESULT:-error}"
  if [ "$RUN_RESULT" = "running" ]; then
    RUN_RESULT="error"
  fi
  RUN_SUMMARY="$1"
  log "$1"
  exit 1
}

cleanup_background() {
  if [ -n "$MONITOR_PID" ]; then
    kill "$MONITOR_PID" >/dev/null 2>&1 || true
    wait "$MONITOR_PID" >/dev/null 2>&1 || true
  fi
  if [ -n "$HEARTBEAT_PID" ]; then
    kill "$HEARTBEAT_PID" >/dev/null 2>&1 || true
    wait "$HEARTBEAT_PID" >/dev/null 2>&1 || true
  fi
}

write_result() {
  local completed_at
  completed_at="$(date -u +%FT%TZ)"

  mkdir -p "$COMMS_DIR"
  # Compute time splits.
  local startup_secs=0 working_secs=0 total_secs=0
  total_secs="$(elapsed_since "$STARTED_AT")"
  if [ -n "$CLAUDE_STARTED_AT" ]; then
    startup_secs="$(elapsed_since "$STARTED_AT")"
    startup_secs=$(( startup_secs - $(elapsed_since "$CLAUDE_STARTED_AT") ))
    working_secs=$(( total_secs - startup_secs ))
  fi

  jq -n \
    --arg agentName "$AGENT_NAME" \
    --arg beadId "$BEAD_ID" \
    --arg result "$RUN_RESULT" \
    --arg summary "$RUN_SUMMARY" \
    --arg branch "$BRANCH_NAME" \
    --arg commit "$COMMIT_SHA" \
    --arg startedAt "$STARTED_AT" \
    --arg claudeStartedAt "${CLAUDE_STARTED_AT:-}" \
    --arg completedAt "$completed_at" \
    --arg model "$MODEL" \
    --arg maxTurns "$MAX_TURNS" \
    --arg timeout "$TIMEOUT_RAW" \
    --arg testsPassed "$TESTS_PASSED" \
    --argjson startupSeconds "$startup_secs" \
    --argjson workingSeconds "$working_secs" \
    --argjson totalSeconds "$total_secs" \
    '{
      agentName: $agentName,
      beadId: $beadId,
      result: $result,
      summary: $summary,
      branch: $branch,
      commit: $commit,
      startedAt: $startedAt,
      claudeStartedAt: $claudeStartedAt,
      completedAt: $completedAt,
      model: $model,
      maxTurns: ($maxTurns | tonumber? // $maxTurns),
      timeout: $timeout,
      testsPassed: ($testsPassed == "true"),
      startupSeconds: $startupSeconds,
      workingSeconds: $workingSeconds,
      totalSeconds: $totalSeconds
    }' >"$RESULT_PATH"
}

finalize() {
  local status="$1"
  trap - EXIT
  cleanup_background

  if [ "$RUN_RESULT" = "running" ]; then
    if [ "$status" -eq 0 ]; then
      RUN_RESULT="success"
      if [ -z "$RUN_SUMMARY" ]; then
        RUN_SUMMARY="completed successfully"
      fi
    else
      RUN_RESULT="error"
      if [ -z "$RUN_SUMMARY" ]; then
        RUN_SUMMARY="wizard exited with status $status"
      fi
    fi
  fi

  write_result
  exit "$status"
}
trap 'finalize "$?"' EXIT

require_env() {
  local name="$1"
  local value="$2"
  if [ -z "$value" ]; then
    fatal "$name must be set"
  fi
}

ensure_dirs() {
  mkdir -p "$WORKSPACE_DIR" "$STATE_DIR" "$COMMS_DIR"
  : >"$STEER_LOG"
}

start_wizard_heartbeat() {
  (
    while true; do
      date -u +%FT%TZ >"$ALIVE_PATH"
      sleep 5
    done
  ) &
  HEARTBEAT_PID="$!"
}

setup_github_auth() {
  if [ -n "${GITHUB_TOKEN:-}" ] && command -v gh >/dev/null 2>&1; then
    printf '%s\n' "$GITHUB_TOKEN" | gh auth login --with-token >/dev/null 2>&1 || true
    gh auth setup-git >/dev/null 2>&1 || true
  fi
}

clone_workspace_if_needed() {
  if [ -d "$WORKSPACE_DIR/.git" ]; then
    return 0
  fi

  require_env "SPIRE_REPO_URL" "$REPO_URL"

  setup_github_auth
  log "cloning $REPO_URL into $WORKSPACE_DIR"
  if ! git clone --depth=1 --branch "$CLONE_BRANCH" "$REPO_URL" "$WORKSPACE_DIR"; then
    fatal "failed to clone $REPO_URL at branch $CLONE_BRANCH"
  fi
}

extract_repo_field() {
  local key="$1"
  awk -v key="$key" '
    $0 ~ "^  " key ": " {
      sub("^  " key ": ", "", $0)
      print
      exit
    }
  ' "$REPO_CONFIG_PATH"
}

normalize_repo_command() {
  local value="$1"
  case "$value" in
    "" | "(none)" | "# (none needed)")
      printf ''
      ;;
    *)
      printf '%s' "$value"
      ;;
  esac
}

load_repo_config() {
  if ! (cd "$WORKSPACE_DIR" && spire config repo >"$REPO_CONFIG_PATH"); then
    fatal "failed to resolve repo config from $WORKSPACE_DIR"
  fi

  MODEL="$(extract_repo_field model)"
  MAX_TURNS="$(extract_repo_field max-turns)"
  TIMEOUT_RAW="$(extract_repo_field timeout)"
  INSTALL_CMD="$(normalize_repo_command "$(extract_repo_field install)")"
  TEST_CMD="$(normalize_repo_command "$(extract_repo_field test)")"
  BUILD_CMD="$(normalize_repo_command "$(extract_repo_field build)")"
  LINT_CMD="$(normalize_repo_command "$(extract_repo_field lint)")"

  local cfg_base
  cfg_base="$(extract_repo_field base)"
  if [ -n "$cfg_base" ]; then
    BASE_BRANCH="$cfg_base"
  fi

  local cfg_pattern
  cfg_pattern="$(extract_repo_field pattern)"
  if [ -n "$cfg_pattern" ]; then
    BRANCH_PATTERN="$cfg_pattern"
  fi
}

collect_context_paths() {
  local -a paths=()
  while IFS= read -r path; do
    [ -n "$path" ] && paths+=("$path")
  done < <(awk '
    /^context:$/ { in_context = 1; next }
    in_context && /^[^ ]/ { in_context = 0 }
    in_context && /^  - / {
      sub(/^  - /, "", $0)
      print
    }
  ' "$REPO_CONFIG_PATH")

  if [ "${#paths[@]}" -eq 0 ]; then
    [ -e "$WORKSPACE_DIR/CLAUDE.md" ] && paths+=("CLAUDE.md")
    [ -e "$WORKSPACE_DIR/SPIRE.md" ] && paths+=("SPIRE.md")
    [ -d "$WORKSPACE_DIR/docs/superpowers/specs" ] && paths+=("docs/superpowers/specs")
  fi

  printf '%s\n' "${paths[@]}"
}

setup_state_repo() {
  cd "$STATE_DIR" || fatal "failed to enter $STATE_DIR"

  if [ ! -d .git ]; then
    git init -q || fatal "failed to initialize git in $STATE_DIR"
  fi

  git config user.name "$AGENT_NAME" >/dev/null 2>&1 || true
  git config user.email "${SPIRE_GIT_EMAIL:-$AGENT_NAME@spire.local}" >/dev/null 2>&1 || true

  # Configure dolt credentials before any dolt operations
  local cred_file
  cred_file=$(ls /root/.dolt/creds/*.jwk 2>/dev/null | head -1)
  if [ -n "$cred_file" ]; then
    local key_id
    key_id=$(basename "$cred_file" .jwk)
    dolt config --global --set user.creds "$key_id" 2>/dev/null || true
    log "dolt credential configured: $key_id"
  fi

  export BEADS_DOLT_SERVER_PORT=3307

  # Copy pre-baked beads snapshot if available (avoids 2min DoltHub clone)
  if [ ! -d .beads ] && [ -d /beads-snapshot/.beads ]; then
    log "restoring pre-baked beads snapshot"
    cp -a /beads-snapshot/. .
  elif [ ! -d .beads ]; then
    log "initializing beads from scratch"
    bd init --force --prefix "$AGENT_PREFIX" >/dev/null || fatal "failed to initialize beads state"
    bd dolt set port 3307 2>/dev/null || true
    echo '{"prefix":"spi-","path":"."}' >> .beads/routes.jsonl 2>/dev/null || true
  fi

  # Start dolt server on fixed port
  rm -f .beads/dolt-server.lock .beads/dolt-server.pid .beads/dolt-server.port
  bd dolt start || log "dolt start warning"

  # Wait for dolt to be reachable (up to 15s)
  local tries=0
  while ! bd dolt test >/dev/null 2>&1 && [ $tries -lt 15 ]; do
    sleep 1
    tries=$((tries + 1))
  done
  if [ $tries -ge 15 ]; then
    log "warning: dolt server not reachable after 15s, continuing anyway"
  fi

  # Pull latest from shared dolt server via remotesapi
  if [ -n "${DOLT_REMOTE_URL:-}" ]; then
    export DOLT_REMOTE_PASSWORD="${DOLT_REMOTE_PASSWORD:-}"
    bd dolt remote add origin "$DOLT_REMOTE_URL" 2>/dev/null || true
    log "pulling beads from dolt server..."
    bd dolt pull >/dev/null 2>&1 || log "pull warning: could not sync (will work with local state)"
  elif [ -n "${DOLTHUB_REMOTE:-}" ]; then
    bd dolt remote add origin "$DOLTHUB_REMOTE" >/dev/null 2>&1 || true
    log "pulling beads from DoltHub..."
    bd dolt pull >/dev/null 2>&1 || log "pull warning: could not sync (will work with local state)"
  fi

  export SPIRE_IDENTITY="$AGENT_NAME"

  spire register "$AGENT_NAME" "Managed autonomous wizard" >/dev/null 2>&1 || true
}

resolve_assignment() {
  if [ -n "$BEAD_ID" ]; then
    return 0
  fi

  if [ ! -f "$INBOX_PATH" ]; then
    fatal "no SPIRE_BEAD_ID provided and no inbox found at $INBOX_PATH"
  fi

  local assignment
  assignment="$(
    jq -r '
      [.[] | {
        id,
        priority: (.priority // 4),
        ref: ([.labels[]? | select(startswith("ref:")) | ltrimstr("ref:")][0] // "")
      }]
      | map(select(.ref != ""))
      | sort_by(.priority)
      | if length == 0 then "" else (.[0].id + "\t" + .[0].ref) end
    ' "$INBOX_PATH" 2>/dev/null
  )"

  if [ -z "$assignment" ] || [ "$assignment" = "null" ]; then
    fatal "no referenced bead found in $INBOX_PATH"
  fi

  IFS=$'\t' read -r ASSIGNMENT_MESSAGE_ID BEAD_ID <<<"$assignment"
  if [ -z "$BEAD_ID" ]; then
    fatal "failed to resolve bead assignment from inbox"
  fi
}

claim_and_focus() {
  cd "$STATE_DIR" || fatal "failed to enter $STATE_DIR"

  log "claiming $BEAD_ID"
  if ! spire claim "$BEAD_ID" >"$COMMS_DIR/claim.json"; then
    fatal "failed to claim bead $BEAD_ID"
  fi

  if ! bd show "$BEAD_ID" --json >"$BEAD_JSON_PATH"; then
    fatal "failed to load bead $BEAD_ID"
  fi

  if ! spire focus "$BEAD_ID" >"$FOCUS_FILE"; then
    fatal "failed to assemble focus context for $BEAD_ID"
  fi

  BEAD_TITLE="$(jq -r '.[0].title // empty' "$BEAD_JSON_PATH" 2>/dev/null)"
  if [ -z "$BEAD_TITLE" ]; then
    BEAD_TITLE="$BEAD_ID"
  fi

  if [ -n "$ASSIGNMENT_MESSAGE_ID" ]; then
    spire read "$ASSIGNMENT_MESSAGE_ID" >/dev/null 2>&1 || true
  fi
}

prepare_branch() {
  cd "$WORKSPACE_DIR" || fatal "failed to enter $WORKSPACE_DIR"

  setup_github_auth

  git config user.name "$AGENT_NAME" >/dev/null 2>&1 || true
  git config user.email "${SPIRE_GIT_EMAIL:-$AGENT_NAME@spire.local}" >/dev/null 2>&1 || true

  if ! git remote get-url origin >/dev/null 2>&1 && [ -n "$REPO_URL" ]; then
    git remote add origin "$REPO_URL" >/dev/null 2>&1 || true
  fi

  git fetch origin "$BASE_BRANCH" --depth=1 >/dev/null 2>&1 || true

  BRANCH_NAME="${BRANCH_PATTERN//\{bead-id\}/$BEAD_ID}"
  if [ -z "$BRANCH_NAME" ]; then
    BRANCH_NAME="feat/$BEAD_ID"
  fi

  if git rev-parse --verify "origin/$BASE_BRANCH" >/dev/null 2>&1; then
    git checkout -B "$BRANCH_NAME" "origin/$BASE_BRANCH" >/dev/null || fatal "failed to create branch $BRANCH_NAME"
  elif git rev-parse --verify "$BASE_BRANCH" >/dev/null 2>&1; then
    git checkout -B "$BRANCH_NAME" "$BASE_BRANCH" >/dev/null || fatal "failed to create branch $BRANCH_NAME"
  else
    git checkout -B "$BRANCH_NAME" >/dev/null || fatal "failed to create branch $BRANCH_NAME"
  fi
}

run_workspace_cmd() {
  local name="$1"
  local command="$2"

  if [ -z "$command" ]; then
    return 0
  fi

  log "running $name: $command"
  (cd "$WORKSPACE_DIR" && sh -lc "$command")
}

install_dependencies() {
  if ! run_workspace_cmd install "$INSTALL_CMD"; then
    fatal "dependency install failed"
  fi
}

duration_to_seconds() {
  local raw="$1"
  case "$raw" in
    *h)
      printf '%s' "$(( ${raw%h} * 3600 ))"
      ;;
    *m)
      printf '%s' "$(( ${raw%m} * 60 ))"
      ;;
    *s)
      printf '%s' "${raw%s}"
      ;;
    *)
      printf '%s' "$raw"
      ;;
  esac
}

build_prompt() {
  local context_block
  context_block=""
  while IFS= read -r path; do
    if [ -n "$path" ]; then
      context_block="${context_block}- ${path}
"
    fi
  done < <(collect_context_paths)

  if [ -z "$context_block" ]; then
    context_block="- CLAUDE.md
- SPIRE.md
"
  fi

  cat >"$PROMPT_FILE" <<EOF
You are Spire autonomous wizard ${AGENT_NAME}.

Task:
- bead: ${BEAD_ID}
- title: ${BEAD_TITLE}
- base branch: ${BASE_BRANCH}
- feature branch: ${BRANCH_NAME}
- target model: ${MODEL}
- max turns: ${MAX_TURNS}
- hard timeout: ${TIMEOUT_RAW}

Before making changes:
1. Read the focus context in ${FOCUS_FILE}.
2. Read the bead JSON in ${BEAD_JSON_PATH}.
3. Read the repo context paths below. If a path is a directory, inspect only the relevant files.

Repo context paths:
${context_block}
Validation commands:
- install: ${INSTALL_CMD:-"(none)"}
- lint: ${LINT_CMD:-"(none)"}
- build: ${BUILD_CMD:-"(none)"}
- test: ${TEST_CMD:-"(none)"}

Constraints:
- Do not create a PR.
- Prefer leaving file changes for the wrapper to commit and push.
- If ${STEER_LOG} has content, treat it as the latest steering input and check it before major decisions.
- If ${STOP_PATH} appears, stop cleanly.

Focus context:
$(cat "$FOCUS_FILE")

Bead JSON:
$(cat "$BEAD_JSON_PATH")
EOF
}

monitor_control() {
  while true; do
    if [ -f "$STEER_PATH" ]; then
      cat "$STEER_PATH" >>"$STEER_LOG" 2>/dev/null || true
      printf '\n' >>"$STEER_LOG"
      rm -f "$STEER_PATH"
      log "captured steer message"
    fi

    if [ -f "$STOP_PATH" ]; then
      date -u +%FT%TZ >"$STOP_REQUESTED_PATH"
      if [ -n "$AGENT_PID" ]; then
        kill -TERM "$AGENT_PID" >/dev/null 2>&1 || true
      fi
      return 0
    fi

    sleep 2
  done
}

run_agent_command() {
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "$TIMEOUT_RAW")"

  local agent_cmd
  agent_cmd="${SPIRE_AGENT_CMD:-}"
  if [ -z "$agent_cmd" ]; then
    if ! command -v claude >/dev/null 2>&1; then
      fatal "claude is not installed and SPIRE_AGENT_CMD is not set"
    fi
    agent_cmd='claude --dangerously-skip-permissions --print "$(cat "$SPIRE_AGENT_PROMPT_FILE")"'
  fi

  export SPIRE_AGENT_PROMPT_FILE="$PROMPT_FILE"
  export SPIRE_AGENT_MODEL="$MODEL"
  export SPIRE_AGENT_MAX_TURNS="$MAX_TURNS"
  export SPIRE_BEAD_ID="$BEAD_ID"
  export SPIRE_BRANCH_NAME="$BRANCH_NAME"

  : >"$AGENT_LOG"
  rm -f "$STOP_REQUESTED_PATH"

  CLAUDE_STARTED_AT="$(date -u +%FT%TZ)"
  log "starting agent command (startup took $(elapsed_since "$STARTED_AT")s)"
  timeout --signal=TERM --kill-after=10s "${timeout_seconds}s" sh -lc "$agent_cmd" \
    > >(tee -a "$AGENT_LOG") 2>&1 &
  AGENT_PID="$!"

  monitor_control &
  MONITOR_PID="$!"

  wait "$AGENT_PID"
  local agent_rc="$?"

  kill "$MONITOR_PID" >/dev/null 2>&1 || true
  wait "$MONITOR_PID" >/dev/null 2>&1 || true
  MONITOR_PID=""
  AGENT_PID=""

  if [ -f "$STOP_REQUESTED_PATH" ]; then
    RUN_RESULT="stopped"
    RUN_SUMMARY="stop requested during agent execution"
    return 0
  fi

  case "$agent_rc" in
    0)
      return 0
      ;;
    124)
      RUN_RESULT="timeout"
      RUN_SUMMARY="agent timed out after ${TIMEOUT_RAW}"
      return 0
      ;;
    *)
      RUN_RESULT="error"
      RUN_SUMMARY="agent exited with status ${agent_rc}"
      return 0
      ;;
  esac
}

run_validation() {
  if [ "$RUN_RESULT" != "running" ]; then
    return 0
  fi

  if ! run_workspace_cmd lint "$LINT_CMD"; then
    RUN_RESULT="test_failure"
    RUN_SUMMARY="lint failed"
    return 0
  fi

  if ! run_workspace_cmd build "$BUILD_CMD"; then
    RUN_RESULT="test_failure"
    RUN_SUMMARY="build failed"
    return 0
  fi

  if [ -n "$TEST_CMD" ]; then
    if ! run_workspace_cmd test "$TEST_CMD"; then
      RUN_RESULT="test_failure"
      RUN_SUMMARY="test command failed"
      return 0
    fi
  fi

  TESTS_PASSED="true"
}

sanitize_title() {
  printf '%s' "$1" | tr '\n' ' ' | tr -s ' ' | cut -c1-72
}

has_branch_work() {
  cd "$WORKSPACE_DIR" || return 1

  if [ -n "$(git status --porcelain)" ]; then
    return 0
  fi

  if git rev-parse --verify "origin/$BASE_BRANCH" >/dev/null 2>&1; then
    local ahead
    ahead="$(git rev-list --count "origin/$BASE_BRANCH..HEAD" 2>/dev/null || printf '0')"
    [ "${ahead:-0}" -gt 0 ] && return 0
  fi

  return 1
}

commit_if_needed() {
  cd "$WORKSPACE_DIR" || fatal "failed to enter $WORKSPACE_DIR"

  if [ -z "$(git status --porcelain)" ]; then
    return 0
  fi

  local summary
  summary="$(sanitize_title "$BEAD_TITLE")"
  local message
  if [ "$RUN_RESULT" = "running" ]; then
    message="feat(${BEAD_ID}): ${summary}"
  else
    message="feat(${BEAD_ID}): partial progress"
  fi

  git add -A || return 1
  if git diff --cached --quiet; then
    return 0
  fi

  git commit -m "$message" >/dev/null
}

push_branch() {
  cd "$WORKSPACE_DIR" || fatal "failed to enter $WORKSPACE_DIR"

  if ! has_branch_work; then
    if [ "$RUN_RESULT" = "running" ]; then
      RUN_RESULT="error"
      RUN_SUMMARY="agent finished without producing any tracked changes"
    fi
    return 0
  fi

  if ! commit_if_needed; then
    if [ "$RUN_RESULT" = "running" ]; then
      RUN_RESULT="error"
      RUN_SUMMARY="failed to commit branch changes"
    fi
    return 0
  fi

  setup_github_auth
  if ! git push -u origin "$BRANCH_NAME"; then
    if [ "$RUN_RESULT" = "running" ]; then
      RUN_RESULT="error"
      RUN_SUMMARY="failed to push branch ${BRANCH_NAME}"
    fi
    return 0
  fi

  COMMIT_SHA="$(git rev-parse HEAD 2>/dev/null || true)"
}

update_bead_state() {
  if [ -z "$COMMIT_SHA" ]; then
    return 0
  fi

  cd "$STATE_DIR" || fatal "failed to enter $STATE_DIR"

  local note
  note="Wizard ${AGENT_NAME} pushed branch ${BRANCH_NAME}"
  if [ -n "$COMMIT_SHA" ]; then
    note="${note} @ ${COMMIT_SHA}"
  fi
  if [ "$RUN_RESULT" != "running" ]; then
    note="${note} (result: ${RUN_RESULT})"
  fi

  bd comments add "$BEAD_ID" "$note" >/dev/null 2>&1 || true

  if [ "$RUN_RESULT" = "running" ]; then
    if ! bd close "$BEAD_ID" --reason "Completed on branch ${BRANCH_NAME}" >/dev/null 2>&1; then
      RUN_RESULT="error"
      RUN_SUMMARY="branch pushed but failed to close bead ${BEAD_ID}"
    fi
  fi

  if ! bd dolt push >/dev/null 2>&1; then
    if [ "$RUN_RESULT" = "running" ]; then
      RUN_RESULT="error"
      RUN_SUMMARY="branch pushed but failed to push bead state"
    fi
    return 0
  fi

  if [ "$RUN_RESULT" = "running" ]; then
    RUN_RESULT="success"
    RUN_SUMMARY="validated and pushed branch ${BRANCH_NAME}"
  fi
}

main() {
  require_env "SPIRE_AGENT_NAME" "$AGENT_NAME"

  ensure_dirs
  start_wizard_heartbeat
  clone_workspace_if_needed
  load_repo_config
  setup_state_repo
  resolve_assignment
  claim_and_focus
  prepare_branch
  install_dependencies
  build_prompt
  run_agent_command
  run_validation
  push_branch
  update_bead_state

  if [ "$RUN_RESULT" = "success" ]; then
    EXIT_CODE=0
  else
    if [ -z "$RUN_SUMMARY" ]; then
      RUN_SUMMARY="wizard finished with result ${RUN_RESULT}"
    fi
    EXIT_CODE=1
  fi

  exit "$EXIT_CODE"
}

main "$@"
