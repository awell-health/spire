#!/bin/sh
# Spire PostToolUse hook — logs tool invocations to a JSONL counter file.
read -r input
tool=$(echo "$input" | jq -r '.tool_name // .tool // empty' 2>/dev/null)
if [ -n "$tool" ]; then
  echo "{\"tool\":\"$tool\"}" >> "/Users/jb/awell/spire/.worktrees/spi-dt13w-feature/.spire-tool-counts.jsonl"
fi
