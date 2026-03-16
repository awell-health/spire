#!/usr/bin/env bash
set -euo pipefail

# setup.sh — Set up Spire as a beads hub for multi-repo coordination
#
# Run after cloning spire:
#   cd spire && ./setup.sh
#
# Satellite repos are listed in satellites.conf (one per line, format: prefix|path).
# If satellites.conf doesn't exist, only the hub is configured.
#
# Steps:
#   1. Install/upgrade beads (includes dolt)
#   2. Set up dolt data directory + env vars
#   3. Read satellite repo registry
#   4. Initialize beads hub
#   5. Configure redirects and routes in satellite repos
#   6. Set up Cursor integration (MCP server + rules)
#   7. Build spire CLI

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PARENT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
HUB_DIR="$SCRIPT_DIR"
HUB_NAME="$(basename "$HUB_DIR")"

# ─── Hub prefix ──────────────────────────────────────────────────────────────
# Read from .beads/config.yaml if it exists, otherwise derive from directory name
if [ -f "$HUB_DIR/.beads/config.yaml" ] && command -v grep >/dev/null 2>&1; then
  CONFIGURED_PREFIX=$(grep -m1 'issue-prefix:' "$HUB_DIR/.beads/config.yaml" 2>/dev/null | sed 's/.*: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/' || echo "")
fi
HUB_PREFIX="${CONFIGURED_PREFIX:-${HUB_NAME:0:3}}"
DOLT_PORT=3307

# ─── Satellite repo registry ────────────────────────────────────────────────
# Read from satellites.conf if it exists. Format: prefix|relative-path
# Paths are relative to the parent directory of this repo.
SATELLITES=()
SATELLITES_CONF="$HUB_DIR/satellites.conf"
if [ -f "$SATELLITES_CONF" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    # Skip comments and blank lines
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    [[ -z "${line// /}" ]] && continue
    SATELLITES+=("$line")
  done < "$SATELLITES_CONF"
fi

# Build full REPOS array: hub first, then satellites
REPOS=("$HUB_PREFIX|$HUB_NAME")
for sat in "${SATELLITES[@]}"; do
  REPOS+=("$sat")
done

# ─── Helpers ──────────────────────────────────────────────────────────────────
info()  { echo -e "  → $*"; }
ok()    { echo -e "  ✓ $*"; }
warn()  { echo -e "  ⚠ $*"; }

# Detect git user for dolt init
GIT_NAME="$(git config user.name 2>/dev/null || echo "spire")"
GIT_EMAIL="$(git config user.email 2>/dev/null || echo "spire@localhost")"

echo ""
echo "=== Spire Setup ==="
echo "    Hub: $HUB_DIR (prefix: $HUB_PREFIX-)"
echo "    Satellites: ${#SATELLITES[@]}"
echo ""

# ─── Step 1: Install/upgrade beads ───────────────────────────────────────────
# Installs beads via Homebrew (which includes dolt as a dependency).
# If already installed, upgrades to latest.
echo "── 1. Install beads ──"

if command -v brew >/dev/null 2>&1; then
  if command -v bd >/dev/null 2>&1; then
    info "beads already installed, upgrading..."
    brew upgrade beads 2>/dev/null && ok "beads upgraded" || ok "beads already at latest"
  else
    info "Installing beads..."
    brew install beads
    ok "beads installed"
  fi
else
  echo "  Error: Homebrew not found. Install it first: https://brew.sh"
  exit 1
fi

BD_VERSION="$(bd --version 2>/dev/null || echo 'unknown')"
ok "beads ready ($BD_VERSION)"
echo ""

# ─── Step 2: Dolt data directory + env vars ──────────────────────────────────
# Ensures the dolt data directory exists and is initialized.
# Adds BEADS_DOLT_SERVER_* env vars to ~/.zshrc so all repos connect
# to a central server instead of spawning per-project embedded dolt instances.
# The actual dolt server is started by `spire up`, not by setup.sh.
echo "── 2. Dolt data directory ──"

DOLT_DIR="/opt/homebrew/var/dolt"

# Ensure directory
mkdir -p "$DOLT_DIR"

# Initialize dolt database directory if needed
if [ ! -d "$DOLT_DIR/.dolt" ]; then
  info "Initializing dolt database..."
  (cd "$DOLT_DIR" && dolt init --name "$GIT_NAME" --email "$GIT_EMAIL" 2>/dev/null) || true
  ok "Dolt database initialized at $DOLT_DIR"
else
  ok "Dolt database already initialized"
fi

# Add env vars to ~/.zshrc if not already present
ZSHRC="$HOME/.zshrc"
if ! grep -q "BEADS_DOLT_SERVER_HOST" "$ZSHRC" 2>/dev/null; then
  info "Adding beads env vars to ~/.zshrc..."
  cat >> "$ZSHRC" << EOF

# Beads — central dolt server (added by spire setup.sh)
export BEADS_DOLT_SERVER_HOST="127.0.0.1"
export BEADS_DOLT_SERVER_PORT="$DOLT_PORT"
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_AUTO_START=0
EOF
  ok "Env vars added to ~/.zshrc"
else
  ok "Env vars already in ~/.zshrc"
fi

# Source for this session
export BEADS_DOLT_SERVER_HOST="127.0.0.1"
export BEADS_DOLT_SERVER_PORT="$DOLT_PORT"
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_AUTO_START=0

echo ""

# ─── Step 3: Verify repos ────────────────────────────────────────────────────
# Checks that each repo in the registry exists as a sibling directory.
# Missing repos are skipped in later steps (routes, redirects, Cursor config).
# They can be cloned later and setup.sh re-run — it's idempotent.
echo "── 3. Verify repos ──"

MISSING=()
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$PARENT_DIR/$repo"
  if [ -d "$repo_path" ]; then
    ok "$prefix- → $repo"
  else
    warn "$prefix- → $repo (not found — will skip)"
    MISSING+=("$prefix")
  fi
done

echo ""

# ─── Step 4: Initialize beads hub ────────────────────────────────────────────
echo "── 4. Initialize beads hub ──"

if [ -d "$HUB_DIR/.beads/dolt" ]; then
  ok "Beads already initialized"
else
  info "Initializing beads..."
  cd "$HUB_DIR"
  bd init --prefix "$HUB_PREFIX"
  ok "Beads initialized with prefix $HUB_PREFIX-"
fi

# Configure DoltHub remote if not already set
REMOTE_COUNT=$(bd dolt remote list 2>/dev/null | grep -c "origin" || echo "0")
if [ "$REMOTE_COUNT" = "0" ]; then
  info "No DoltHub remote configured."
  info "To enable remote sync, run:"
  info "  bd dolt remote add origin <dolthub-url>"
else
  ok "DoltHub remote 'origin' configured"
fi

# Create webhook_queue table if not exists
info "Ensuring webhook_queue table exists..."
DOLT_CLI_PASSWORD="" dolt --host 127.0.0.1 --port "$DOLT_PORT" --user root --no-tls --use-db "$HUB_PREFIX" \
  sql -q "CREATE TABLE IF NOT EXISTS webhook_queue (
  id          VARCHAR(36) PRIMARY KEY,
  event_type  VARCHAR(64) NOT NULL,
  linear_id   VARCHAR(32) NOT NULL,
  payload     JSON NOT NULL,
  processed   BOOLEAN NOT NULL DEFAULT 0,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);" 2>/dev/null && ok "webhook_queue table ready" || warn "Could not create webhook_queue table (run manually)"

# Write .envrc for the hub
if [ ! -f "$HUB_DIR/.envrc" ]; then
  echo "export SPIRE_IDENTITY=\"$HUB_PREFIX\"" > "$HUB_DIR/.envrc"
  ok "Hub .envrc created (SPIRE_IDENTITY=$HUB_PREFIX)"
else
  ok "Hub .envrc already exists"
fi

echo ""

# ─── Step 5: Routes and redirects ────────────────────────────────────────────
echo "── 5. Configure routes and redirects ──"

# Build routes.jsonl — all prefixes point to "." (hub's db)
# Satellites resolve "." through their redirect to the hub
ROUTES=""
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  [[ " ${MISSING[*]:-} " =~ " $prefix " ]] && continue
  ROUTES+='{"prefix":"'"$prefix"'-","path":"."}'$'\n'
done

# Write routes to hub
printf '%s' "$ROUTES" > "$HUB_DIR/.beads/routes.jsonl"
ok "Hub routes.jsonl updated"

# Set up each satellite repo
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$PARENT_DIR/$repo"

  # Skip hub and missing repos
  [ "$prefix" = "$HUB_PREFIX" ] && continue
  [[ " ${MISSING[*]:-} " =~ " $prefix " ]] && continue

  # Create .beads dir with redirect + routes
  mkdir -p "$repo_path/.beads"
  echo "../$HUB_NAME/.beads" > "$repo_path/.beads/redirect"
  printf '%s' "$ROUTES" > "$repo_path/.beads/routes.jsonl"

  ok "$repo: redirect + routes configured"

  # Set spire identity for this repo
  ENVRC="$repo_path/.envrc"
  if [ -f "$ENVRC" ]; then
    if ! grep -q "SPIRE_IDENTITY" "$ENVRC" 2>/dev/null; then
      echo "export SPIRE_IDENTITY=\"$prefix\"" >> "$ENVRC"
    fi
  else
    echo "export SPIRE_IDENTITY=\"$prefix\"" > "$ENVRC"
  fi
done

echo ""

# ─── Step 6: Cursor integration ──────────────────────────────────────────────
echo "── 6. Cursor integration ──"

# Install all workspace dependencies
if [ -f "$HUB_DIR/pnpm-workspace.yaml" ]; then
  info "Installing monorepo dependencies (pnpm)..."
  (cd "$HUB_DIR" && pnpm install --silent 2>/dev/null)
  ok "Dependencies installed"
fi

MCP_SERVER_PATH="$HUB_DIR/packages/mcp-server/index.js"
CURSOR_RULE_SRC="$HUB_DIR/cursor/spire-messaging.mdc"

# Add spire MCP server + rule to each repo's .cursor config
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$PARENT_DIR/$repo"

  # Skip missing repos
  [[ " ${MISSING[*]:-} " =~ " $prefix " ]] && continue

  # Ensure .cursor directory exists
  mkdir -p "$repo_path/.cursor/rules"

  # Copy the spire messaging rule
  if [ -f "$CURSOR_RULE_SRC" ]; then
    cp -f "$CURSOR_RULE_SRC" "$repo_path/.cursor/rules/spire-messaging.mdc"
  fi

  # Add spire MCP server to .cursor/mcp.json
  MCP_JSON="$repo_path/.cursor/mcp.json"
  if [ -f "$MCP_JSON" ]; then
    # Check if spire server already configured
    if ! grep -q '"spire"' "$MCP_JSON" 2>/dev/null; then
      node -e "
        const fs = require('fs');
        const cfg = JSON.parse(fs.readFileSync('$MCP_JSON', 'utf8'));
        cfg.mcpServers = cfg.mcpServers || {};
        cfg.mcpServers.spire = {
          command: 'node',
          args: ['$MCP_SERVER_PATH'],
          env: { SPIRE_IDENTITY: '$prefix' }
        };
        fs.writeFileSync('$MCP_JSON', JSON.stringify(cfg, null, 2) + '\n');
      "
      ok "$repo: spire MCP server added to .cursor/mcp.json"
    else
      ok "$repo: spire MCP server already in .cursor/mcp.json"
    fi
  else
    cat > "$MCP_JSON" << EOF
{
  "mcpServers": {
    "spire": {
      "command": "node",
      "args": ["$MCP_SERVER_PATH"],
      "env": {
        "SPIRE_IDENTITY": "$prefix"
      }
    }
  }
}
EOF
    ok "$repo: .cursor/mcp.json created"
  fi
done

echo ""

# ── Step 7: Build spire CLI ────────────────────────────────────────────────
echo "── 7. Build spire CLI ──"

if ! command -v go >/dev/null 2>&1; then
  warn "Go not found — skipping spire CLI build"
  warn "Install Go and run: cd $HUB_DIR && go build -o ~/.local/bin/spire ./cmd/spire"
else
  mkdir -p "$HOME/.local/bin"
  info "Building spire CLI..."
  (cd "$HUB_DIR" && go build -o "$HOME/.local/bin/spire" ./cmd/spire)
  ok "spire installed to ~/.local/bin/spire"

  # Register the hub as an agent
  "$HOME/.local/bin/spire" register "$HUB_PREFIX" >/dev/null 2>&1 || true

  # Ensure ~/.local/bin is in PATH
  if ! echo "$PATH" | grep -q "$HOME/.local/bin"; then
    if ! grep -q '.local/bin' "$ZSHRC" 2>/dev/null; then
      echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$ZSHRC"
      ok "Added ~/.local/bin to PATH in ~/.zshrc"
    fi
    export PATH="$HOME/.local/bin:$PATH"
  fi
fi

echo ""
echo "=== Setup complete ==="
echo ""
echo "Repos:"
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  if [[ " ${MISSING[*]:-} " =~ " $prefix " ]]; then
    echo "  ⚠ $prefix- → $repo (missing)"
  elif [ "$prefix" = "$HUB_PREFIX" ]; then
    echo "  ✓ $prefix- → $repo (hub)"
  else
    echo "  ✓ $prefix- → $repo (satellite)"
  fi
done
echo ""
if [ ${#SATELLITES[@]} -eq 0 ]; then
  echo "No satellites configured. To add one, create satellites.conf:"
  echo "  echo 'web|my-web-app' >> satellites.conf"
  echo "  ./setup.sh"
  echo ""
fi
echo "Next steps:"
echo "  spire up                 # start dolt server + daemon"
echo "  spire connect linear     # set up Linear integration"
echo "  spire collect            # check inbox"
echo "  bd list                  # see all issues"
echo ""
echo "Restart your shell or run: source ~/.zshrc"
