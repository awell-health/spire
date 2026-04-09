#!/usr/bin/env bash
# Spire context injection hook for Claude Code.
# Reads the hook event from stdin and outputs additionalContext.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

EVENT=$(cat 2>/dev/null || true)
HOOK_EVENT=$(echo "$EVENT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hook_event_name',''))" 2>/dev/null || echo "")

SPIRE_MD=""
if [ -f "$REPO_ROOT/SPIRE.md" ]; then
    SPIRE_MD=$(cat "$REPO_ROOT/SPIRE.md")
fi

case "$HOOK_EVENT" in
    SessionStart)
        COLLECT=$(spire collect 2>/dev/null || echo "No messages.")
        CONTEXT="# Spire Context (prefix: spi)

${SPIRE_MD}

## Current inbox
${COLLECT}"
        ;;
    PostCompact)
        CONTEXT="# Spire Context (re-injected after compaction, prefix: spi)

${SPIRE_MD}"
        ;;
    SubagentStart)
        CONTEXT="# Spire Work Protocol (prefix: spi)

You are a subagent in a Spire-managed repo. Follow this protocol:

${SPIRE_MD}

IMPORTANT: When you complete work on a bead, you MUST:
1. Close each molecule step: bd close <step-id>
2. Close the bead: bd close <bead-id>
3. Push state: bd dolt push
Never leave beads or molecule steps open after completing work."
        ;;
    *)
        echo "{}"
        exit 0
        ;;
esac

python3 -c "
import json, sys
print(json.dumps({
    'hookSpecificOutput': {
        'additionalContext': sys.stdin.read()
    }
}))
" <<< "$CONTEXT"
