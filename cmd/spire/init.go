package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdInit(args []string) error {
	fmt.Println(spireLogo)

	// Parse flags
	var flagPrefix, flagSatellite string
	var flagStandalone, flagHub bool
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prefix" && i+1 < len(args):
			i++
			flagPrefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			flagPrefix = strings.TrimPrefix(args[i], "--prefix=")
		case args[i] == "--satellite" && i+1 < len(args):
			i++
			flagSatellite = args[i]
		case strings.HasPrefix(args[i], "--satellite="):
			flagSatellite = strings.TrimPrefix(args[i], "--satellite=")
		case args[i] == "--standalone":
			flagStandalone = true
		case args[i] == "--hub":
			flagHub = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire init [--prefix=<pfx>] [--hub|--standalone|--satellite=<hub>]", args[i])
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Check if already init'd
	if existing := findInstanceByPath(cfg, cwd); existing != nil {
		fmt.Printf("  This directory is already init'd as %q (%s).\n", existing.Prefix, existing.Role)
		fmt.Printf("    prefix:   %s\n", existing.Prefix)
		fmt.Printf("    role:     %s\n", existing.Role)
		fmt.Printf("    database: %s\n", existing.Database)
		if existing.Hub != "" {
			fmt.Printf("    hub:      %s\n", existing.Hub)
		}
		fmt.Println()
		fmt.Print("  Re-initialize? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y") {
			return nil
		}
		// Remove old entry so we can re-create
		delete(cfg.Instances, existing.Prefix)
	}

	firstInit := len(cfg.Instances) == 0

	// --- Shell env injection ---
	if !cfg.Shell.Configured {
		profile, injected := injectShellEnv()
		if injected {
			cfg.Shell.Configured = true
			cfg.Shell.Profile = profile
			fmt.Printf("  Shell env vars added to %s\n", profile)
		}
	} else {
		fmt.Printf("  Shell env: already configured (%s)\n", cfg.Shell.Profile)
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	// --- Prefix ---
	prefix := flagPrefix
	if prefix == "" {
		defaultPrefix := currentDirName()
		if len(defaultPrefix) > 3 {
			defaultPrefix = defaultPrefix[:3]
		}
		fmt.Printf("  Prefix for this repo [%s]: ", defaultPrefix)
		input, _ := reader.ReadString('\n')
		prefix = strings.TrimSpace(input)
		if prefix == "" {
			prefix = defaultPrefix
		}
	}

	// Check prefix uniqueness
	if _, exists := cfg.Instances[prefix]; exists {
		return fmt.Errorf("prefix %q is already in use by %s", prefix, cfg.Instances[prefix].Path)
	}

	// --- Role ---
	role := ""
	hubPrefix := ""

	switch {
	case flagSatellite != "":
		role = "satellite"
		hubPrefix = flagSatellite
		if _, ok := cfg.Instances[hubPrefix]; !ok {
			return fmt.Errorf("hub %q not found in config — init the hub first", hubPrefix)
		}
		if cfg.Instances[hubPrefix].Role != "hub" {
			return fmt.Errorf("%q is not a hub (role: %s)", hubPrefix, cfg.Instances[hubPrefix].Role)
		}
	case flagStandalone:
		role = "standalone"
	case flagHub:
		role = "hub"
	default:
		// Interactive role selection
		if firstInit {
			fmt.Println("  Role for this repo:")
			fmt.Println("    1. Hub (other repos can connect as satellites)")
			fmt.Println("    2. Standalone (single repo)")
			fmt.Print("  [1]: ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			switch input {
			case "", "1":
				role = "hub"
			case "2":
				role = "standalone"
			default:
				return fmt.Errorf("invalid selection: %q", input)
			}
		} else {
			// Find existing hubs for satellite option
			var hubs []string
			for k, inst := range cfg.Instances {
				if inst.Role == "hub" {
					hubs = append(hubs, k)
				}
			}

			fmt.Println("  Role for this repo:")
			optNum := 1
			if len(hubs) > 0 {
				for _, h := range hubs {
					fmt.Printf("    %d. Satellite of %s (%s)\n", optNum, h, cfg.Instances[h].Path)
					optNum++
				}
			}
			hubOpt := optNum
			fmt.Printf("    %d. Hub (other repos can connect as satellites)\n", optNum)
			optNum++
			standaloneOpt := optNum
			fmt.Printf("    %d. Standalone (single repo)\n", optNum)

			defaultOpt := "1"
			fmt.Printf("  [%s]: ", defaultOpt)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			if input == "" {
				input = defaultOpt
			}

			choice := 0
			fmt.Sscanf(input, "%d", &choice)
			switch {
			case len(hubs) > 0 && choice >= 1 && choice <= len(hubs):
				role = "satellite"
				hubPrefix = hubs[choice-1]
			case choice == hubOpt:
				role = "hub"
			case choice == standaloneOpt:
				role = "standalone"
			default:
				return fmt.Errorf("invalid selection: %q", input)
			}
		}
	}

	fmt.Println()
	fmt.Printf("  Initializing %s as %s (prefix: %s-)...\n", currentDirName(), role, prefix)
	fmt.Println()

	// --- Set up beads ---
	database := prefix
	if role == "satellite" {
		database = hubPrefix
	}

	if role == "satellite" {
		// Satellite: create .beads/redirect
		hubInst := cfg.Instances[hubPrefix]
		hubBeads := filepath.Join(hubInst.Path, ".beads")
		relPath, err := filepath.Rel(cwd, hubBeads)
		if err != nil {
			relPath = hubBeads // fallback to absolute
		}
		os.MkdirAll(filepath.Join(cwd, ".beads"), 0755)
		if err := os.WriteFile(filepath.Join(cwd, ".beads", "redirect"), []byte(relPath+"\n"), 0644); err != nil {
			return fmt.Errorf("write redirect: %w", err)
		}
		fmt.Printf("  Redirect → %s\n", relPath)
	} else {
		// Hub/standalone: run bd init
		// Check if .beads already exists locally with the right prefix
		localBeads := filepath.Join(cwd, ".beads")
		if _, statErr := os.Stat(localBeads); statErr == nil {
			// Already initialized locally — check prefix matches
			existingPrefix, _ := bd("config", "get", "issue-prefix")
			existingPrefix = strings.TrimSpace(existingPrefix)
			if existingPrefix != "" && !strings.Contains(existingPrefix, "(not set)") {
				if existingPrefix == prefix {
					fmt.Printf("  Beads already initialized (prefix: %s-)\n", prefix)
				} else {
					return fmt.Errorf("beads already initialized with prefix %q (wanted %q) — use bd init --force --prefix %s to reinitialize", existingPrefix, prefix, prefix)
				}
			}
		} else {
			// No local .beads — need to init. Use --force to skip the
			// "already initialized" check that bd raises when it detects
			// a running dolt server. Don't pre-create the database —
			// let bd init create it so the commit history is consistent.
			_, initErr := bd("init", "--force", "--prefix", prefix)
			if initErr != nil {
				return fmt.Errorf("bd init failed: %w\n  Try: bd init --force --prefix %s", initErr, prefix)
			}
			fmt.Printf("  Beads initialized (prefix: %s-)\n", prefix)
		}
	}

	// --- Write .envrc ---
	envrcPath := filepath.Join(cwd, ".envrc")
	envrcContent := fmt.Sprintf("export SPIRE_IDENTITY=\"%s\"\n", prefix)
	if data, err := os.ReadFile(envrcPath); err == nil {
		if !strings.Contains(string(data), "SPIRE_IDENTITY") {
			// Append
			os.WriteFile(envrcPath, append(data, []byte("\n"+envrcContent)...), 0644)
		}
	} else {
		os.WriteFile(envrcPath, []byte(envrcContent), 0644)
	}
	fmt.Printf("  .envrc written (SPIRE_IDENTITY=%s)\n", prefix)

	// --- Register in config ---
	inst := &Instance{
		Path:     cwd,
		Prefix:   prefix,
		Role:     role,
		Database: database,
	}
	if role == "satellite" {
		inst.Hub = hubPrefix
		// Add to hub's satellite list
		hubInst := cfg.Instances[hubPrefix]
		if !containsStr(hubInst.Satellites, prefix) {
			hubInst.Satellites = append(hubInst.Satellites, prefix)
		}
	}
	cfg.Instances[prefix] = inst

	// --- Regenerate routes for satellite/hub ---
	if role == "satellite" {
		if err := regenerateRoutes(cfg, hubPrefix); err != nil {
			fmt.Printf("  Warning: route regeneration failed: %s\n", err)
		} else {
			fmt.Println("  Routes regenerated")
		}
	}

	// --- Save config ---
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Println("  Config saved")

	// --- Install editor config (Cursor + Claude Code) ---
	if err := installEditorConfig(cwd, prefix, cfg); err != nil {
		fmt.Printf("  Warning: editor config install failed: %s\n", err)
	}

	// --- SPIRE.md ---
	if err := writeSpireMD(cwd, prefix); err != nil {
		fmt.Printf("  Warning: SPIRE.md write failed: %s\n", err)
	} else {
		fmt.Println("  SPIRE.md written")
	}

	// --- AGENTS.md ---
	offerAgentsMDUpdate(reader, cwd)

	// --- Next steps ---
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Println("    spire up                     # start dolt server + daemon")
	if role == "hub" {
		fmt.Println("    spire init --satellite=" + prefix + "   # connect a satellite repo")
	}
	fmt.Println("    spire connect linear         # sync epics to Linear")
	fmt.Println("    spire repo list              # see all init'd repos")
	fmt.Println()

	return nil
}

// injectShellEnv detects the user's shell, finds the profile, and appends env vars.
// Returns (profile path, true) if injected, or ("", false) if skipped/failed.
func injectShellEnv() (string, bool) {
	shell := os.Getenv("SHELL")
	profile := ""

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	switch {
	case strings.HasSuffix(shell, "/zsh"):
		profile = filepath.Join(home, ".zshrc")
	case strings.HasSuffix(shell, "/bash"):
		// Prefer .bash_profile on macOS, .bashrc on Linux
		profile = filepath.Join(home, ".bash_profile")
		if _, err := os.Stat(profile); os.IsNotExist(err) {
			profile = filepath.Join(home, ".bashrc")
		}
	case strings.HasSuffix(shell, "/fish"):
		profile = filepath.Join(home, ".config", "fish", "config.fish")
	default:
		profile = filepath.Join(home, ".profile")
	}

	// Check if already present
	if data, err := os.ReadFile(profile); err == nil {
		if strings.Contains(string(data), "BEADS_DOLT_SERVER_HOST") {
			return profile, false
		}
	}

	block := `
# Beads — central dolt server (added by spire init)
export BEADS_DOLT_SERVER_HOST="127.0.0.1"
export BEADS_DOLT_SERVER_PORT="3307"
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_AUTO_START=0
`

	f, err := os.OpenFile(profile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", false
	}
	defer f.Close()
	f.WriteString(block)

	return profile, true
}

// regenerateRoutes rebuilds routes.jsonl for a hub and all its satellites.
func regenerateRoutes(cfg *SpireConfig, hubPrefix string) error {
	hubInst, ok := cfg.Instances[hubPrefix]
	if !ok {
		return fmt.Errorf("hub %q not in config", hubPrefix)
	}

	// Build routes: hub + all its satellites
	var lines []string
	lines = append(lines, fmt.Sprintf(`{"prefix":"%s-","path":"."}`, hubPrefix))
	for _, satPrefix := range hubInst.Satellites {
		lines = append(lines, fmt.Sprintf(`{"prefix":"%s-","path":"."}`, satPrefix))
	}
	routesContent := strings.Join(lines, "\n") + "\n"

	// Write to hub
	hubRoutes := filepath.Join(hubInst.Path, ".beads", "routes.jsonl")
	if err := os.WriteFile(hubRoutes, []byte(routesContent), 0644); err != nil {
		return fmt.Errorf("write hub routes: %w", err)
	}

	// Write to each satellite
	for _, satPrefix := range hubInst.Satellites {
		satInst, ok := cfg.Instances[satPrefix]
		if !ok {
			continue
		}
		satRoutes := filepath.Join(satInst.Path, ".beads", "routes.jsonl")
		os.WriteFile(satRoutes, []byte(routesContent), 0644)
	}

	return nil
}

// containsStr checks if a slice contains a string.
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// installEditorConfig installs Cursor rules and Claude Code MCP config in the repo.
func installEditorConfig(repoPath, prefix string, cfg *SpireConfig) error {
	mcpServerPath := findMCPServerPath(cfg)

	cursorOK := installCursorConfig(repoPath, prefix, mcpServerPath)
	claudeOK := installClaudeConfig(repoPath, prefix, mcpServerPath)

	if cursorOK {
		fmt.Println("  Cursor: spire rule + MCP server configured")
	}
	if claudeOK {
		fmt.Println("  Claude Code: MCP server configured")
	}
	if !cursorOK && !claudeOK {
		fmt.Println("  Editor config: skipped (MCP server path unknown — set SPIRE_REPO)")
	}

	return nil
}

// findMCPServerPath locates the spire MCP server index.js.
// Checks: SPIRE_REPO env > hub instance path > sibling of executable.
func findMCPServerPath(cfg *SpireConfig) string {
	candidates := []string{}

	if env := os.Getenv("SPIRE_REPO"); env != "" {
		candidates = append(candidates, filepath.Join(env, "packages", "mcp-server", "index.js"))
	}

	// Look in hub instance paths
	for _, inst := range cfg.Instances {
		if inst.Role == "hub" || inst.Role == "standalone" {
			candidates = append(candidates, filepath.Join(inst.Path, "packages", "mcp-server", "index.js"))
		}
	}

	// Look relative to the spire binary
	if exe, err := os.Executable(); err == nil {
		// Binary might be at /opt/homebrew/bin/spire — check share dir
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "..", "share", "spire", "mcp-server", "index.js"),
			filepath.Join(dir, "..", "lib", "spire", "mcp-server", "index.js"),
		)
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// installCursorConfig writes .cursor/rules/spire-messaging.mdc and patches .cursor/mcp.json.
func installCursorConfig(repoPath, prefix, mcpServerPath string) bool {
	spireRepoPath := findSpireRepoForCursor()

	// Copy the .mdc rule if we can find it
	ruleSrc := ""
	if spireRepoPath != "" {
		ruleSrc = filepath.Join(spireRepoPath, "cursor", "spire-messaging.mdc")
	}

	cursorDir := filepath.Join(repoPath, ".cursor")
	rulesDir := filepath.Join(cursorDir, "rules")

	if ruleSrc != "" {
		if data, err := os.ReadFile(ruleSrc); err == nil {
			os.MkdirAll(rulesDir, 0755)
			os.WriteFile(filepath.Join(rulesDir, "spire-messaging.mdc"), data, 0644)
		}
	}

	if mcpServerPath == "" {
		return ruleSrc != ""
	}

	// Patch .cursor/mcp.json
	os.MkdirAll(cursorDir, 0755)
	mcpJSON := filepath.Join(cursorDir, "mcp.json")
	patchMCPJSON(mcpJSON, prefix, mcpServerPath)
	return true
}

// installClaudeConfig writes .claude/settings.local.json, .mcp.json, and copies skills.
func installClaudeConfig(repoPath, prefix, mcpServerPath string) bool {
	if mcpServerPath == "" {
		return false
	}

	claudeDir := filepath.Join(repoPath, ".claude")
	os.MkdirAll(claudeDir, 0755)

	// .claude/settings.local.json — enable project MCP servers
	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		os.WriteFile(settingsPath, []byte("{\n  \"enableAllProjectMcpServers\": true\n}\n"), 0644)
	}

	// .mcp.json at repo root — Claude Code picks this up
	mcpJSON := filepath.Join(repoPath, ".mcp.json")
	patchMCPJSON(mcpJSON, prefix, mcpServerPath)

	// Copy spire skills to .claude/skills/ so they appear in this project's skill list
	installSpireSkills(claudeDir)

	return true
}

// installSpireSkills copies spire skills from ~/.claude/skills/ into .claude/skills/.
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
		} else {
			if data, err := os.ReadFile(srcPath); err == nil {
				os.WriteFile(dstPath, data, 0644)
			}
		}
	}
}

// patchMCPJSON adds the spire server entry to an MCP JSON config file.
// Creates the file if it doesn't exist; preserves existing entries.
func patchMCPJSON(path, prefix, mcpServerPath string) {
	type mcpServer struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	type mcpConfig struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}

	cfg := mcpConfig{MCPServers: make(map[string]mcpServer)}

	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cfg)
		if cfg.MCPServers == nil {
			cfg.MCPServers = make(map[string]mcpServer)
		}
	}

	cfg.MCPServers["spire"] = mcpServer{
		Command: "node",
		Args:    []string{mcpServerPath},
		Env:     map[string]string{"SPIRE_IDENTITY": prefix},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, append(data, '\n'), 0644)
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
spire send <agent> "done" --ref <bead-id>   # notify others
`+"```"+`

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

// offerAgentsMDUpdate prompts to add a SPIRE.md reference to AGENTS.md if it exists.
func offerAgentsMDUpdate(reader *bufio.Reader, repoPath string) {
	agentsPath := filepath.Join(repoPath, "AGENTS.md")
	data, err := os.ReadFile(agentsPath)
	if err != nil {
		return // no AGENTS.md, nothing to do
	}
	if strings.Contains(string(data), "SPIRE.md") {
		return // already references it
	}

	fmt.Print("  Add SPIRE.md reference to AGENTS.md? [Y/n] ")
	answer, _ := reader.ReadString('\n')
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "n") {
		return
	}

	addition := "\n## Work coordination\n\nThis repo uses Spire for agent work coordination. See [SPIRE.md](SPIRE.md) for the session lifecycle, work claiming, and inter-agent messaging.\n"
	updated := append(data, []byte(addition)...)
	if err := os.WriteFile(agentsPath, updated, 0644); err != nil {
		fmt.Printf("  Warning: could not update AGENTS.md: %s\n", err)
	} else {
		fmt.Println("  AGENTS.md updated")
	}
}

// findSpireRepoForCursor finds the spire repo path for copying cursor rules.
func findSpireRepoForCursor() string {
	if env := os.Getenv("SPIRE_REPO"); env != "" {
		return env
	}
	if cfg, err := loadConfig(); err == nil {
		for _, inst := range cfg.Instances {
			if inst.Role == "hub" || inst.Role == "standalone" {
				rulePath := filepath.Join(inst.Path, "cursor", "spire-messaging.mdc")
				if _, err := os.Stat(rulePath); err == nil {
					return inst.Path
				}
			}
		}
	}
	return ""
}
