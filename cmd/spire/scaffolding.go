package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// spireWorkProtocol is the work lifecycle section added to CLAUDE.md.
// Used by both writeCLAUDEMD and the hooks (PostCompact/SubagentStart).
func spireWorkProtocol(prefix string) string {
	return fmt.Sprintf(`## Spire — Work Coordination

This repo uses [Spire](SPIRE.md) for work tracking and coordination (prefix: **%s**).

### Creating work
- `+"`spire file \"Title\" -t task -p 2`"+` — file new work (type: task/bug/feature/epic/chore, priority: 0-4)

### Working on a bead
- `+"`spire claim <bead-id>`"+` — claim before starting (atomic: pull → verify → set in_progress → push)
- `+"`spire focus <bead-id>`"+` — assemble context and see your workflow molecule (design → implement → review → merge)

### Completing work
When done, close things in order:
1. `+"`bd close <step-id>`"+` for each molecule step (design, implement, review, merge)
2. `+"`bd close <bead-id>`"+` to close the bead itself
3. `+"`bd dolt push`"+` to push state to remote

Never leave a bead in_progress without closing it when done. Subagents must close their own molecule steps.

### Commit format
`+"```"+`
<type>(<bead-id>): <message>
`+"```"+`
Types: feat, fix, chore, docs, refactor, test
`, prefix)
}

// writeSpireMD writes SPIRE.md to the repo root with agent session instructions.
// Skips if the file already exists.
func writeSpireMD(repoPath, prefix string) error {
	path := filepath.Join(repoPath, "SPIRE.md")
	content := fmt.Sprintf(`# SPIRE.md — Agent Work Instructions

This repo is connected to Spire (prefix: **%s**). Use Spire for work coordination.

## Session lifecycle

`+"```"+`bash
spire up                        # ensure services are running
spire collect                   # check inbox + read your context brief
spire claim <bead-id>           # claim a task (atomic: pull → verify → set in_progress → push)
spire focus <bead-id>           # assemble full context for the task
# ... do the work ...
bd close <step-id>              # close each molecule step as you complete it
bd close <bead-id>              # close the bead when all work is done
bd dolt push                    # push state to remote
spire send <agent> "done" --ref <bead-id>   # notify others
`+"```"+`

## Completing work

When you finish a task, you MUST close things in order:

1. **Close molecule steps** — `+"`spire focus <bead-id>`"+` shows your workflow molecule.
   Close each step (design, implement, review, merge) with `+"`bd close <step-id>`"+`
2. **Close the bead** — `+"`bd close <bead-id>`"+`
3. **Push state** — `+"`bd dolt push`"+`
4. **Notify** — `+"`spire send <agent> \"done\" --ref <bead-id>`"+` if assigned via mail

## Filing work

`+"```"+`bash
spire file "Title" -t task -p 2             # file from anywhere (prefix auto-detected in repo)
spire file "Title" --prefix %s -t bug -p 1 # explicit prefix
`+"```"+`

## Messaging

`+"```"+`bash
spire register <name> [context]  # register as an agent, optionally with a brief
spire collect                    # check inbox (also prints your context brief)
spire send <to> "message" --ref <bead-id>
spire read <bead-id>             # mark a message as read
`+"```"+`

## Commit format

Always reference the bead in commit messages:

`+"```"+`
<type>(<bead-id>): <message>
`+"```"+`

Examples:
- `+"`feat(spi-a3f8): add OAuth2 support`"+`
- `+"`fix(xserver-0hy): handle nil pointer in rate limiter`"+`
- `+"`chore(pan-b7d0): upgrade dependencies`"+`

Types: `+"`feat`"+`, `+"`fix`"+`, `+"`chore`"+`, `+"`docs`"+`, `+"`refactor`"+`, `+"`test`"+`

## Key conventions

- **Claim before working**: prevents double-work
- **Priority**: -p 0 (critical) → -p 4 (nice-to-have)
- **Types**: task, bug, feature, epic, chore
- **Epics** auto-sync to Linear — do not create Linear issues manually
`, prefix, prefix)
	return os.WriteFile(path, []byte(content), 0644)
}

// writeSpireHooks writes SessionStart, PostCompact, and SubagentStart hooks
// to .claude/settings.json. Merges with existing hooks if the file exists.
func writeSpireHooks(repoPath, prefix string) {
	settingsPath := filepath.Join(repoPath, ".claude", "settings.json")
	os.MkdirAll(filepath.Join(repoPath, ".claude"), 0755)

	// Load existing settings
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	// Check if hooks already configured
	if hooks, ok := settings["hooks"]; ok {
		if hooksMap, ok := hooks.(map[string]interface{}); ok {
			if _, hasSession := hooksMap["SessionStart"]; hasSession {
				fmt.Println("  Hooks already configured")
				return
			}
		}
	}

	// Write the hook script
	hookScript := filepath.Join(repoPath, ".claude", "spire-hook.sh")
	scriptContent := fmt.Sprintf(`#!/usr/bin/env bash
# Spire context injection hook for Claude Code.
# Reads the hook event from stdin and outputs additionalContext.

EVENT=$(cat 2>/dev/null || true)
HOOK_EVENT=$(echo "$EVENT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hook_event_name',''))" 2>/dev/null || echo "")

SPIRE_MD=""
if [ -f "%s/SPIRE.md" ]; then
    SPIRE_MD=$(cat "%s/SPIRE.md")
fi

case "$HOOK_EVENT" in
    SessionStart)
        COLLECT=$(spire collect 2>/dev/null || echo "No messages.")
        CONTEXT="# Spire Context (prefix: %s)

${SPIRE_MD}

## Current inbox
${COLLECT}"
        ;;
    PostCompact)
        CONTEXT="# Spire Context (re-injected after compaction, prefix: %s)

${SPIRE_MD}"
        ;;
    SubagentStart)
        CONTEXT="# Spire Work Protocol (prefix: %s)

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
`, repoPath, repoPath, prefix, prefix, prefix)

	if err := os.WriteFile(hookScript, []byte(scriptContent), 0755); err != nil {
		fmt.Printf("  Warning: could not write hook script: %s\n", err)
		return
	}

	// Build hooks config
	hookEntry := []interface{}{
		map[string]interface{}{
			"hooks": []interface{}{
				map[string]interface{}{
					"type":    "command",
					"command": hookScript,
					"timeout": 10,
				},
			},
		},
	}

	hooksMap := make(map[string]interface{})
	if existing, ok := settings["hooks"]; ok {
		if existingMap, ok := existing.(map[string]interface{}); ok {
			hooksMap = existingMap
		}
	}
	hooksMap["SessionStart"] = hookEntry
	hooksMap["PostCompact"] = hookEntry
	hooksMap["SubagentStart"] = hookEntry
	settings["hooks"] = hooksMap

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Printf("  Warning: could not marshal settings: %s\n", err)
		return
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0644); err != nil {
		fmt.Printf("  Warning: could not write settings: %s\n", err)
		return
	}

	fmt.Println("  Hooks configured (SessionStart, PostCompact, SubagentStart)")
}

// installSpireSkills copies spire-* skill directories from ~/.claude/skills/
// into the project's .claude/skills/ directory.
func installSpireSkills(claudeDir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	globalSkillsDir := filepath.Join(home, ".claude", "skills")
	projectSkillsDir := filepath.Join(claudeDir, "skills")

	entries, err := os.ReadDir(globalSkillsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only copy spire-* skills
		if !strings.HasPrefix(entry.Name(), "spire") {
			continue
		}
		srcDir := filepath.Join(globalSkillsDir, entry.Name())
		dstDir := filepath.Join(projectSkillsDir, entry.Name())
		copyDir(srcDir, dstDir)
	}
}

// copyDir recursively copies a directory tree from src to dst.
func copyDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	os.MkdirAll(dst, 0755)
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyDir(srcPath, dstPath)
			continue
		}
		srcFile, err := os.Open(srcPath)
		if err != nil {
			continue
		}
		dstFile, err := os.Create(dstPath)
		if err != nil {
			srcFile.Close()
			continue
		}
		io.Copy(dstFile, srcFile)
		srcFile.Close()
		dstFile.Close()
	}
}
