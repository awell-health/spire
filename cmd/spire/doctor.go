package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkStatus represents the result of a single doctor check.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusMissing
	statusOutdated
)

func (s checkStatus) String() string {
	switch s {
	case statusOK:
		return "OK"
	case statusMissing:
		return "MISSING"
	case statusOutdated:
		return "OUTDATED"
	default:
		return "UNKNOWN"
	}
}

// checkResult holds the outcome of one doctor check.
type checkResult struct {
	Name    string
	Status  checkStatus
	Detail  string
	FixFunc func() // nil if no fix available
}

func cmdDoctor(args []string) error {
	// Parse --fix flag
	fix := false
	for _, arg := range args {
		switch arg {
		case "--fix":
			fix = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire doctor [--fix]", arg)
		}
	}

	cwd, err := realCwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		return fmt.Errorf("this directory is not initialized — run spire init first")
	}
	prefix := inst.Prefix

	// Run all checks
	checks := []checkResult{
		checkCLAUDEMD(cwd),
		checkSPIREMD(cwd),
		checkSettingsJSON(cwd),
		checkSpireHookSH(cwd),
		checkSpireSkills(cwd),
	}

	// Report
	hasIssues := false
	for _, c := range checks {
		icon := "+"
		if c.Status != statusOK {
			icon = "!"
			hasIssues = true
		}
		fmt.Printf("  [%s] %-40s %s", icon, c.Name, c.Status)
		if c.Detail != "" {
			fmt.Printf("  (%s)", c.Detail)
		}
		fmt.Println()
	}

	if !hasIssues {
		fmt.Println("\n  All checks passed.")
		return nil
	}

	if !fix {
		fmt.Println("\n  Run `spire doctor --fix` to repair issues.")
		return nil
	}

	// Fix mode
	fmt.Println()
	fixed := 0
	for _, c := range checks {
		if c.Status == statusOK {
			continue
		}
		if c.FixFunc == nil {
			fmt.Printf("  %-40s no auto-fix available\n", c.Name)
			continue
		}
		fmt.Printf("  Fixing %s...\n", c.Name)
		c.FixFunc()
		fixed++
	}

	// Re-run checks after fix to show updated status.
	// We need the prefix for the fix functions, but the checks themselves
	// don't need it — pass prefix via closures in the fix functions.
	_ = prefix // used in fix closures below

	fmt.Printf("\n  Fixed %d issue(s). Re-checking:\n\n", fixed)
	reChecks := []checkResult{
		checkCLAUDEMD(cwd),
		checkSPIREMD(cwd),
		checkSettingsJSON(cwd),
		checkSpireHookSH(cwd),
		checkSpireSkills(cwd),
	}
	for _, c := range reChecks {
		icon := "+"
		if c.Status != statusOK {
			icon = "!"
		}
		fmt.Printf("  [%s] %-40s %s", icon, c.Name, c.Status)
		if c.Detail != "" {
			fmt.Printf("  (%s)", c.Detail)
		}
		fmt.Println()
	}

	return nil
}

// checkCLAUDEMD verifies CLAUDE.md exists and contains the ## Spire section.
func checkCLAUDEMD(repoPath string) checkResult {
	path := filepath.Join(repoPath, "CLAUDE.md")
	name := "CLAUDE.md (## Spire section)"

	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "file does not exist",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				section := spireWorkProtocol(prefix)
				content := "# CLAUDE.md\n\n" + section
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					fmt.Printf("    Warning: could not write CLAUDE.md: %s\n", err)
				} else {
					fmt.Println("    CLAUDE.md created")
				}
			},
		}
	}

	if strings.Contains(string(data), "## Spire") {
		return checkResult{Name: name, Status: statusOK}
	}

	return checkResult{
		Name:   name,
		Status: statusOutdated,
		Detail: "file exists but missing ## Spire section",
		FixFunc: func() {
			prefix := detectPrefixFromPath(repoPath)
			section := spireWorkProtocol(prefix)
			updated := append(data, []byte("\n"+section)...)
			if err := os.WriteFile(path, updated, 0644); err != nil {
				fmt.Printf("    Warning: could not update CLAUDE.md: %s\n", err)
			} else {
				fmt.Println("    CLAUDE.md updated (Spire section appended)")
			}
		},
	}
}

// checkSPIREMD verifies SPIRE.md exists and contains the ## Completing work section.
func checkSPIREMD(repoPath string) checkResult {
	path := filepath.Join(repoPath, "SPIRE.md")
	name := "SPIRE.md (## Completing work section)"

	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "file does not exist",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				if err := writeSpireMD(repoPath, prefix); err != nil {
					fmt.Printf("    Warning: SPIRE.md write failed: %s\n", err)
				} else {
					fmt.Println("    SPIRE.md created")
				}
			},
		}
	}

	if strings.Contains(string(data), "## Completing work") {
		return checkResult{Name: name, Status: statusOK}
	}

	return checkResult{
		Name:   name,
		Status: statusOutdated,
		Detail: "file exists but missing ## Completing work section",
		FixFunc: func() {
			prefix := detectPrefixFromPath(repoPath)
			// Regenerate the whole file to ensure the section is present
			if err := writeSpireMD(repoPath, prefix); err != nil {
				fmt.Printf("    Warning: SPIRE.md write failed: %s\n", err)
			} else {
				fmt.Println("    SPIRE.md regenerated")
			}
		},
	}
}

// checkSettingsJSON verifies .claude/settings.json exists and contains
// SessionStart, PostCompact, and SubagentStart hooks.
func checkSettingsJSON(repoPath string) checkResult {
	path := filepath.Join(repoPath, ".claude", "settings.json")
	name := ".claude/settings.json (hooks)"

	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "file does not exist",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "file exists but is not valid JSON",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	hooks, ok := settings["hooks"]
	if !ok {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "no hooks section",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	hooksMap, ok := hooks.(map[string]interface{})
	if !ok {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "hooks section is not a map",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	var missing []string
	for _, hookName := range []string{"SessionStart", "PostCompact", "SubagentStart"} {
		if _, exists := hooksMap[hookName]; !exists {
			missing = append(missing, hookName)
		}
	}

	if len(missing) > 0 {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "missing hooks: " + strings.Join(missing, ", "),
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	return checkResult{Name: name, Status: statusOK}
}

// checkSpireHookSH verifies .claude/spire-hook.sh exists and is executable.
func checkSpireHookSH(repoPath string) checkResult {
	path := filepath.Join(repoPath, ".claude", "spire-hook.sh")
	name := ".claude/spire-hook.sh (executable)"

	info, err := os.Stat(path)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "file does not exist",
			FixFunc: func() {
				prefix := detectPrefixFromPath(repoPath)
				writeSpireHooks(repoPath, prefix)
			},
		}
	}

	if info.Mode()&0111 == 0 {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "file exists but is not executable",
			FixFunc: func() {
				if err := os.Chmod(path, 0755); err != nil {
					fmt.Printf("    Warning: could not chmod spire-hook.sh: %s\n", err)
				} else {
					fmt.Println("    spire-hook.sh made executable")
				}
			},
		}
	}

	return checkResult{Name: name, Status: statusOK}
}

// checkSpireSkills verifies .claude/skills/spire-work/ directory exists.
func checkSpireSkills(repoPath string) checkResult {
	dir := filepath.Join(repoPath, ".claude", "skills", "spire-work")
	name := ".claude/skills/spire-work/"

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "directory does not exist",
			FixFunc: func() {
				claudeDir := filepath.Join(repoPath, ".claude")
				installSpireSkills(claudeDir)
				// Verify it worked
				if _, err := os.Stat(dir); err != nil {
					fmt.Println("    Warning: skills directory still missing after install (source may not exist in ~/.claude/skills/)")
				} else {
					fmt.Println("    spire-work skills installed")
				}
			},
		}
	}

	return checkResult{Name: name, Status: statusOK}
}

// detectPrefixFromPath looks up the prefix for a given repo path from config.
func detectPrefixFromPath(repoPath string) string {
	cfg, err := loadConfig()
	if err != nil {
		return "spi" // fallback
	}
	inst := findInstanceByPath(cfg, repoPath)
	if inst == nil {
		return "spi" // fallback
	}
	return inst.Prefix
}
