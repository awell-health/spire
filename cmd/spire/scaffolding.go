package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/cmd/spire/embedded"
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
// Uses the embedded template and substitutes the repo prefix.
// Skips if the file already exists.
func writeSpireMD(repoPath, prefix string) error {
	path := filepath.Join(repoPath, "SPIRE.md")
	content := strings.ReplaceAll(embedded.SpireMDTemplate, "{{.Prefix}}", prefix)
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
        'additionalContext': sys.stdin.read(),
        'hookEventName': '$HOOK_EVENT'
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

// installSpireSkills installs bundled Spire skills into Claude and Codex skill
// directories, then copies global Claude spire-* skills into the project.
func installSpireSkills(claudeDir string) {
	home, err := os.UserHomeDir()
	if err != nil {
		installBundledSpireSkills(filepath.Join(claudeDir, "skills"))
		installBundledCodexSkills()
		return
	}

	globalSkillsDir := filepath.Join(home, ".claude", "skills")
	projectSkillsDir := filepath.Join(claudeDir, "skills")

	// Seed the home-level and repo-level skill trees from the bundled assets so
	// the skill ships with the Spire binary instead of relying on preexisting
	// ~/.claude content.
	installBundledSpireSkills(globalSkillsDir)
	installBundledSpireSkills(projectSkillsDir)
	installBundledCodexSkills()

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

func installBundledSpireSkills(dstRoot string) {
	_ = copyEmbeddedDir(embedded.Skills, "skills", dstRoot)
}

func installBundledCodexSkills() {
	root := codexHomeDir()
	if root == "" {
		return
	}
	_ = copyEmbeddedDir(embedded.Skills, "skills", filepath.Join(root, "skills"))
}

func codexHomeDir() string {
	if v := os.Getenv("CODEX_HOME"); strings.TrimSpace(v) != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func copyEmbeddedDir(srcFS fs.FS, srcRoot, dstRoot string) error {
	return fs.WalkDir(srcFS, srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstRoot, 0755)
		}
		dstPath := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0755)
		}
		data, err := fs.ReadFile(srcFS, path)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, data, 0644)
	})
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
