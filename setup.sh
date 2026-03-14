#!/usr/bin/env bash
set -euo pipefail

# setup.sh — Set up Spire as the beads hub for Awell repos
#
# Run after cloning spire:
#   cd ~/awell/spire && ./setup.sh
#
# Steps:
#   1. Install/upgrade beads (includes dolt)
#   2. Set up central dolt server (LaunchAgent + env vars)
#   3. Verify managed repos exist
#   4. Initialize beads hub in spire
#   5. Configure redirects and routes in satellite repos
#   6. Set up Cursor integration (MCP server + rules)
#   7. Build spire CLI

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
AWELL_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
HUB_DIR="$SCRIPT_DIR"

# ─── Repo registry ───────────────────────────────────────────────────────────
# Format: prefix|directory_name
# All paths are relative to $AWELL_DIR
# The first entry is the hub (spire); the rest are satellites.
REPOS=(
  "spi|spire"
  "pan|panels"
  "gro|grove"
  "rel|release-management"
)

HUB_PREFIX="spi"
HUB_REPO="spire"
DOLT_PORT=3307

# ─── Helpers ──────────────────────────────────────────────────────────────────
info()  { echo -e "  → $*"; }
ok()    { echo -e "  ✓ $*"; }
warn()  { echo -e "  ⚠ $*"; }

echo ""
echo "=== Spire Beads Setup ==="
echo "    Awell directory: $AWELL_DIR"
echo ""

# ─── Step 1: Install/upgrade beads ───────────────────────────────────────────
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

# ─── Step 2: Central dolt server ─────────────────────────────────────────────
echo "── 2. Central dolt server ──"

DOLT_DIR="/opt/homebrew/var/dolt"
DOLT_LOG_DIR="/opt/homebrew/var/log"
PLIST_NAME="com.local.dolt-server"
PLIST_PATH="$HOME/Library/LaunchAgents/$PLIST_NAME.plist"

# Ensure directories
mkdir -p "$DOLT_DIR" "$DOLT_LOG_DIR" "$HOME/Library/LaunchAgents"

# Initialize dolt database directory if needed
if [ ! -d "$DOLT_DIR/.dolt" ]; then
  info "Initializing dolt database..."
  (cd "$DOLT_DIR" && dolt init --name "awell" --email "dev@awellhealth.com" 2>/dev/null) || true
  ok "Dolt database initialized at $DOLT_DIR"
else
  ok "Dolt database already initialized"
fi

# Write dolt config (port 3307 per beads convention)
cat > "$DOLT_DIR/config.yaml" << EOF
listener:
  host: "127.0.0.1"
  port: $DOLT_PORT
  max_connections: 100
EOF
ok "Dolt config written ($DOLT_DIR/config.yaml, port $DOLT_PORT)"

# Create LaunchAgent plist
DOLT_BIN="$(command -v dolt)"
cat > "$PLIST_PATH" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$PLIST_NAME</string>
    <key>ProgramArguments</key>
    <array>
        <string>$DOLT_BIN</string>
        <string>sql-server</string>
        <string>--config</string>
        <string>config.yaml</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DOLT_DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$DOLT_LOG_DIR/dolt.log</string>
    <key>StandardErrorPath</key>
    <string>$DOLT_LOG_DIR/dolt.error.log</string>
</dict>
</plist>
EOF
ok "LaunchAgent written ($PLIST_PATH)"

# (Re)load the LaunchAgent
launchctl bootout "gui/$(id -u)/$PLIST_NAME" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_PATH"
ok "Dolt server started (port $DOLT_PORT)"

# Add env vars to ~/.zshrc if not already present
ZSHRC="$HOME/.zshrc"
if ! grep -q "BEADS_DOLT_SERVER_HOST" "$ZSHRC" 2>/dev/null; then
  info "Adding beads env vars to ~/.zshrc..."
  cat >> "$ZSHRC" << EOF

# Beads — central dolt server (added by spire/setup.sh)
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

# ─── Step 3: Verify repos exist ──────────────────────────────────────────────
echo "── 3. Verify repos ──"

MISSING=()
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$AWELL_DIR/$repo"
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
  ok "Beads already initialized in spire"
else
  info "Initializing beads in spire..."
  cd "$HUB_DIR"
  bd init --prefix "$HUB_PREFIX"
  ok "Beads initialized with prefix $HUB_PREFIX-"
fi

# Configure DoltHub remote if not already set
REMOTE_COUNT=$(bd dolt remote list 2>/dev/null | grep -c "origin" || echo "0")
if [ "$REMOTE_COUNT" = "0" ]; then
  info "No DoltHub remote configured."
  info "To enable daemon sync, run:"
  info "  cd $HUB_DIR && bd dolt remote add origin <dolthub-url>"
else
  ok "DoltHub remote 'origin' configured"
fi

echo ""

# ─── Step 5: Routes and redirects ────────────────────────────────────────────
echo "── 5. Configure routes and redirects ──"

# Build routes.jsonl — all prefixes point to "." (spire's db)
# Satellites resolve "." through their redirect to spire
ROUTES=""
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  [[ " ${MISSING[*]:-} " =~ " $prefix " ]] && continue
  ROUTES+='{"prefix":"'"$prefix"'-","path":"."}'$'\n'
done

# Write routes to spire
printf '%s' "$ROUTES" > "$HUB_DIR/.beads/routes.jsonl"
ok "Spire routes.jsonl updated"

# Set up each satellite repo
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$AWELL_DIR/$repo"

  # Skip hub and missing repos
  [ "$prefix" = "$HUB_PREFIX" ] && continue
  [[ " ${MISSING[*]:-} " =~ " $prefix " ]] && continue

  # Create .beads dir with redirect + routes
  mkdir -p "$repo_path/.beads"
  echo "../spire/.beads" > "$repo_path/.beads/redirect"
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

# Install MCP server dependencies
if [ -f "$HUB_DIR/mcp-server/package.json" ]; then
  info "Installing spire MCP server dependencies..."
  (cd "$HUB_DIR/mcp-server" && npm install --silent 2>/dev/null)
  ok "MCP server dependencies installed"
fi

MCP_SERVER_PATH="$HUB_DIR/mcp-server/index.js"
CURSOR_RULE_SRC="$HUB_DIR/cursor/spire-messaging.mdc"

# Add spire MCP server + rule to each repo's .cursor config
for entry in "${REPOS[@]}"; do
  prefix="${entry%%|*}"
  repo="${entry##*|}"
  repo_path="$AWELL_DIR/$repo"

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
      # Insert spire server into existing mcpServers object
      # Use node for reliable JSON manipulation
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
    # Create new mcp.json
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
  "$HOME/.local/bin/spire" register spi >/dev/null 2>&1 || true

  # Ensure ~/.local/bin is in PATH
  if ! echo "$PATH" | grep -q "$HOME/.local/bin"; then
    if ! grep -q '.local/bin' "$ZSHRC" 2>/dev/null; then
      echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$ZSHRC"
      ok "Added ~/.local/bin to PATH in ~/.zshrc"
    fi
    export PATH="$HOME/.local/bin:$PATH"
  fi
fi

# ── Step 8: Spire daemon LaunchAgent ──────────────────────────────────────
echo "── 8. Spire daemon LaunchAgent ──"

SPIRE_PLIST_NAME="com.awell.spire-daemon"
SPIRE_PLIST_PATH="$HOME/Library/LaunchAgents/$SPIRE_PLIST_NAME.plist"
SPIRE_BIN="$HOME/.local/bin/spire"

if [ -x "$SPIRE_BIN" ]; then
  cat > "$SPIRE_PLIST_PATH" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$SPIRE_PLIST_NAME</string>
    <key>ProgramArguments</key>
    <array>
        <string>$SPIRE_BIN</string>
        <string>daemon</string>
        <string>--interval</string>
        <string>2m</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$HUB_DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$DOLT_LOG_DIR/spire-daemon.log</string>
    <key>StandardErrorPath</key>
    <string>$DOLT_LOG_DIR/spire-daemon.error.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SPIRE_IDENTITY</key>
        <string>spi</string>
        <key>BEADS_DOLT_SERVER_HOST</key>
        <string>127.0.0.1</string>
        <key>BEADS_DOLT_SERVER_PORT</key>
        <string>$DOLT_PORT</string>
        <key>BEADS_DOLT_SERVER_MODE</key>
        <string>1</string>
        <key>BEADS_DOLT_AUTO_START</key>
        <string>0</string>
    </dict>
</dict>
</plist>
EOF
  ok "LaunchAgent written ($SPIRE_PLIST_PATH)"

  # (Re)load the LaunchAgent
  launchctl bootout "gui/$(id -u)/$SPIRE_PLIST_NAME" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$SPIRE_PLIST_PATH"
  ok "Spire daemon started"
else
  warn "Spire binary not found at $SPIRE_BIN — skipping daemon LaunchAgent"
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
echo "Usage:"
echo "  cd ~/awell/panels && bd create --rig=pan 'Fix widget bug' -p 2 -t task"
echo "  cd ~/awell/spire  && bd list   # see all issues across repos"
echo ""
echo "Restart your shell or run: source ~/.zshrc"
