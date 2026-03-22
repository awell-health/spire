package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	bdpkg "github.com/awell-health/spire/pkg/bd"
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
	Name     string
	Status   checkStatus
	Detail   string
	FixFunc  func() // nil if no fix available
	Optional bool   // if true, doesn't count as failure in summary
}

// checkCategory groups related checks under a heading.
type checkCategory struct {
	Name   string
	Checks []checkResult
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

	// Build check categories. System and Tower always run.
	// Repo checks only run if the current directory is a registered instance.
	categories := []checkCategory{
		{
			Name: "System",
			Checks: []checkResult{
				checkDoltBinary(),
				checkDoltServer(),
				checkDocker(),
			},
		},
		{
			Name: "Tower",
			Checks: []checkResult{
				checkTowerConfig(cwd),
				checkTowerBeadsDir(),
				checkCredentials(),
			},
		},
	}

	cfg, err := loadConfig()
	if err != nil {
		// Config load failed — tower check will catch the detail, but we
		// still show system checks.
		cfg = &SpireConfig{Instances: make(map[string]*Instance)}
	}

	// Add registration migration check if dolt is reachable and we have instances
	if doltIsReachable() && len(cfg.Instances) > 0 && cfg.ActiveTower != "" {
		categories[1].Checks = append(categories[1].Checks, checkRepoMigration(cfg))
	}

	inst := findInstanceByPath(cfg, cwd)
	if inst != nil {
		categories = append(categories, checkCategory{
			Name: "Repo",
			Checks: []checkResult{
				checkCLAUDEMD(cwd),
				checkSPIREMD(cwd),
				checkSettingsJSON(cwd),
				checkSpireHookSH(cwd),
				checkSpireSkills(cwd),
			},
		})
	}

	// Report
	totalChecks := 0
	passedChecks := 0
	hasFixable := false
	var allChecks []checkResult

	for _, cat := range categories {
		fmt.Println(cat.Name)
		for _, c := range cat.Checks {
			allChecks = append(allChecks, c)
			totalChecks++
			if c.Status != statusOK && !c.Optional {
				hasFixable = hasFixable || c.FixFunc != nil
			}

			icon := "+"
			if c.Status == statusOK {
				passedChecks++
			} else if !c.Optional {
				icon = "!"
			}

			optTag := ""
			if c.Optional {
				optTag = " [optional]"
			}

			if c.Detail != "" {
				fmt.Printf("  [%s] %-40s %-10s (%s)%s\n", icon, c.Name, c.Status, c.Detail, optTag)
			} else {
				fmt.Printf("  [%s] %-40s %s%s\n", icon, c.Name, c.Status, optTag)
			}
		}
		fmt.Println()
	}

	// Summary
	fmt.Printf("%d of %d checks passed.\n", passedChecks, totalChecks)

	if passedChecks == totalChecks {
		return nil
	}

	if !fix {
		if hasFixable {
			fmt.Println("Run `spire doctor --fix` to repair fixable issues.")
		}
		return nil
	}

	// Fix mode
	fmt.Println()
	fixed := 0
	for _, c := range allChecks {
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

	fmt.Printf("\n  Fixed %d issue(s). Re-checking:\n\n", fixed)

	// Re-run all checks to show updated status
	reCfg, _ := loadConfig()
	if reCfg == nil {
		reCfg = &SpireConfig{Instances: make(map[string]*Instance)}
	}
	reCategories := []checkCategory{
		{
			Name: "System",
			Checks: []checkResult{
				checkDoltBinary(),
				checkDoltServer(),
				checkDocker(),
			},
		},
		{
			Name: "Tower",
			Checks: []checkResult{
				checkTowerConfig(cwd),
				checkTowerBeadsDir(),
				checkCredentials(),
			},
		},
	}
	if doltIsReachable() && len(reCfg.Instances) > 0 && reCfg.ActiveTower != "" {
		reCategories[1].Checks = append(reCategories[1].Checks, checkRepoMigration(reCfg))
	}
	if inst != nil {
		reCategories = append(reCategories, checkCategory{
			Name: "Repo",
			Checks: []checkResult{
				checkCLAUDEMD(cwd),
				checkSPIREMD(cwd),
				checkSettingsJSON(cwd),
				checkSpireHookSH(cwd),
				checkSpireSkills(cwd),
			},
		})
	}

	for _, cat := range reCategories {
		fmt.Println(cat.Name)
		for _, c := range cat.Checks {
			icon := "+"
			if c.Status != statusOK && !c.Optional {
				icon = "!"
			}
			optTag := ""
			if c.Optional {
				optTag = " [optional]"
			}
			if c.Detail != "" {
				fmt.Printf("  [%s] %-40s %-10s (%s)%s\n", icon, c.Name, c.Status, c.Detail, optTag)
			} else {
				fmt.Printf("  [%s] %-40s %s%s\n", icon, c.Name, c.Status, optTag)
			}
		}
		fmt.Println()
	}

	return nil
}

// --- System checks ---

// checkDoltBinary verifies a dolt binary is available.
// Checks the managed path first, then system PATH.
func checkDoltBinary() checkResult {
	name := "dolt binary"

	// Check managed binary first
	managedPath := filepath.Join(doltGlobalDir(), "bin", "dolt")
	if info, err := os.Stat(managedPath); err == nil && !info.IsDir() {
		ver := doltVersionOutput(managedPath)
		if doltVersionOK(managedPath) {
			return checkResult{
				Name:   name,
				Status: statusOK,
				Detail: managedPath + " " + ver,
			}
		}
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: fmt.Sprintf("%s %s (need v%s)", managedPath, ver, doltRequiredVersion),
			FixFunc: func() {
				if err := doltDownload(); err != nil {
					fmt.Printf("    Failed to download dolt: %s\n", err)
				} else {
					fmt.Println("    dolt binary updated")
				}
			},
		}
	}

	// Fall back to system PATH
	sysPath, err := exec.LookPath("dolt")
	if err == nil {
		ver := doltVersionOutput(sysPath)
		if doltVersionOK(sysPath) {
			return checkResult{
				Name:   name,
				Status: statusOK,
				Detail: sysPath + " " + ver,
			}
		}
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: fmt.Sprintf("%s %s (need v%s)", sysPath, ver, doltRequiredVersion),
			FixFunc: func() {
				if err := doltDownload(); err != nil {
					fmt.Printf("    Failed to download dolt: %s\n", err)
				} else {
					fmt.Println("    managed dolt binary installed (takes precedence over PATH)")
				}
			},
		}
	}

	return checkResult{
		Name:   name,
		Status: statusMissing,
		Detail: "not found — run spire up to auto-install",
		FixFunc: func() {
			if err := doltDownload(); err != nil {
				fmt.Printf("    Failed to download dolt: %s\n", err)
			} else {
				fmt.Println("    dolt binary installed")
			}
		},
	}
}

// doltVersionOutput runs `dolt version` and returns a trimmed version string.
func doltVersionOutput(doltPath string) string {
	out, err := exec.Command(doltPath, "version").Output()
	if err != nil {
		return "(unknown version)"
	}
	// Output is like "dolt version 1.46.1\n"
	s := strings.TrimSpace(string(out))
	if strings.HasPrefix(s, "dolt version ") {
		return "v" + strings.TrimPrefix(s, "dolt version ")
	}
	return s
}

// checkDoltServer verifies the dolt server is running and reachable.
func checkDoltServer() checkResult {
	name := "dolt server"

	pid, running, reachable := doltServerStatus()
	if running && reachable {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: fmt.Sprintf("port %s, pid %d", doltPort(), pid),
		}
	}
	if running && !reachable {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: fmt.Sprintf("process running (pid %d) but port %s not reachable", pid, doltPort()),
		}
	}
	// Not running. Check if the port is reachable anyway (external process).
	if reachable {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: fmt.Sprintf("port %s (external process)", doltPort()),
		}
	}
	return checkResult{
		Name:   name,
		Status: statusMissing,
		Detail: "not running — run spire up",
		FixFunc: func() {
			// Ensure dolt binary exists before trying to start the server
			if _, err := doltEnsureBinary(); err != nil {
				fmt.Printf("    Cannot start server: dolt binary not available: %s\n", err)
				return
			}
			pid, err := doltStart()
			if err != nil {
				fmt.Printf("    Failed to start dolt server: %s\n", err)
			} else {
				fmt.Printf("    dolt server started (pid %d)\n", pid)
			}
		},
	}
}

// checkDocker verifies Docker is available. This is an optional check.
func checkDocker() checkResult {
	name := "docker"

	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return checkResult{
			Name:     name,
			Status:   statusMissing,
			Detail:   "not available — process mode (--mode=process) available as alternative",
			Optional: true,
		}
	}
	ver := strings.TrimSpace(string(out))
	return checkResult{
		Name:     name,
		Status:   statusOK,
		Detail:   "v" + ver,
		Optional: true,
	}
}

// --- Tower checks ---

// checkTowerConfig verifies ~/.config/spire/config.json exists and the current
// directory is a registered instance.
func checkTowerConfig(cwd string) checkResult {
	name := "tower config"

	cp, err := configPath()
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "cannot determine config path",
		}
	}
	if _, err := os.Stat(cp); os.IsNotExist(err) {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "config.json does not exist — run spire init",
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "config.json exists but cannot be loaded: " + err.Error(),
		}
	}

	if len(cfg.Instances) == 0 {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "no repos registered — run spire init",
		}
	}

	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		return checkResult{
			Name:   name,
			Status: statusOutdated,
			Detail: "current directory not registered — run spire init",
		}
	}

	return checkResult{
		Name:   name,
		Status: statusOK,
		Detail: fmt.Sprintf("prefix: %s, role: %s", inst.Prefix, inst.Role),
	}
}

// checkTowerBeadsDir verifies the active tower's .beads/ directory exists in the
// dolt data dir. If the tower config exists but .beads/ is missing, it can be
// regenerated (same bootstrap as tower attach).
func checkTowerBeadsDir() checkResult {
	name := "tower .beads/ data"

	cfg, err := loadConfig()
	if err != nil || cfg.ActiveTower == "" {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: "no active tower (skipped)",
		}
	}

	tower, err := loadTowerConfig(cfg.ActiveTower)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: "tower config not loadable (skipped)",
		}
	}

	dataDir := doltDataDir()
	beadsDir := filepath.Join(dataDir, tower.Database, ".beads")
	metaPath := filepath.Join(beadsDir, "metadata.json")
	configYAMLPath := filepath.Join(beadsDir, "config.yaml")

	metaOK := fileExists(metaPath)
	configOK := fileExists(configYAMLPath)

	if metaOK && configOK {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: beadsDir,
		}
	}

	var missingFiles []string
	if !metaOK {
		missingFiles = append(missingFiles, "metadata.json")
	}
	if !configOK {
		missingFiles = append(missingFiles, "config.yaml")
	}

	return checkResult{
		Name:   name,
		Status: statusMissing,
		Detail: fmt.Sprintf("missing: %s in %s", strings.Join(missingFiles, ", "), beadsDir),
		FixFunc: func() {
			if err := os.MkdirAll(beadsDir, 0755); err != nil {
				fmt.Printf("    Failed to create .beads/ dir: %s\n", err)
				return
			}
			if !metaOK {
				beadsMeta := map[string]any{
					"project_id":    tower.ProjectID,
					"database":      "dolt",
					"backend":       "dolt",
					"dolt_mode":     "server",
					"dolt_database": tower.Database,
				}
				metaBytes, _ := json.MarshalIndent(beadsMeta, "", "  ")
				if err := os.WriteFile(metaPath, append(metaBytes, '\n'), 0644); err != nil {
					fmt.Printf("    Failed to write metadata.json: %s\n", err)
					return
				}
				fmt.Println("    metadata.json regenerated")
			}
			if !configOK {
				configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\n", doltHost(), doltPort())
				if err := os.WriteFile(configYAMLPath, []byte(configYAML), 0644); err != nil {
					fmt.Printf("    Failed to write config.yaml: %s\n", err)
					return
				}
				fmt.Println("    config.yaml regenerated")
			}
		},
	}
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// checkRepoMigration checks if local config instances are missing from the
// dolt repos table and offers to insert them. Requires dolt to be running.
func checkRepoMigration(cfg *SpireConfig) checkResult {
	name := "repo registrations in dolt"

	tower, err := loadTowerConfig(cfg.ActiveTower)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: "no active tower (skipped)",
		}
	}

	// Query the repos table to see which prefixes are already registered
	query := fmt.Sprintf("SELECT prefix FROM `%s`.repos", tower.Database)
	out, err := rawDoltQuery(query)
	if err != nil {
		// Table might not exist
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: "repos table not queryable (skipped)",
		}
	}

	// Parse prefixes from dolt's pipe-delimited tabular output
	doltPrefixes := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		// Skip separators (+---+) and empty lines
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		// Parse pipe-delimited rows: | prefix_value |
		if strings.HasPrefix(line, "|") {
			for _, p := range strings.Split(line, "|") {
				p = strings.TrimSpace(p)
				if p != "" && p != "prefix" {
					doltPrefixes[p] = true
				}
			}
		}
	}

	// Find instances belonging to this tower that are not in the repos table
	var missing []*Instance
	for _, inst := range cfg.Instances {
		if inst.Prefix == "" {
			continue
		}
		// Only consider instances that belong to the active tower
		if inst.Tower != "" && inst.Tower != cfg.ActiveTower {
			continue
		}
		if inst.Database != "" && inst.Database != tower.Database {
			continue
		}
		if !doltPrefixes[inst.Prefix] {
			missing = append(missing, inst)
		}
	}

	if len(missing) == 0 {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: fmt.Sprintf("all %d local registrations present in dolt", len(cfg.Instances)),
		}
	}

	var prefixes []string
	for _, inst := range missing {
		prefixes = append(prefixes, inst.Prefix)
	}

	return checkResult{
		Name:   name,
		Status: statusOutdated,
		Detail: fmt.Sprintf("local-only: %s", strings.Join(prefixes, ", ")),
		FixFunc: func() {
			// Use a bd client with the tower's BeadsDir for proper database
			// context, matching the pattern in register_repo.go.
			client := bdpkg.NewClient()
			client.BeadsDir = filepath.Join(doltDataDir(), tower.Database, ".beads")

			migrated := 0
			for _, inst := range missing {
				repoURL := ""
				if inst.Path != "" {
					cmd := exec.Command("git", "-C", inst.Path, "remote", "get-url", "origin")
					if urlOut, err := cmd.Output(); err == nil {
						repoURL = strings.TrimSpace(string(urlOut))
					}
				}
				if repoURL == "" {
					repoURL = "unknown"
				}

				insertSQL := fmt.Sprintf(
					"INSERT INTO repos (prefix, repo_url, branch, registered_by) VALUES ('%s', '%s', 'main', 'doctor-fix')",
					sqlEscape(inst.Prefix), sqlEscape(repoURL),
				)
				if _, err := client.DoltSQL(insertSQL); err != nil {
					fmt.Printf("    Failed to migrate %s: %s\n", inst.Prefix, err)
				} else {
					fmt.Printf("    Migrated %s (%s) to dolt repos table\n", inst.Prefix, repoURL)
					migrated++
				}
			}
			if migrated > 0 {
				if err := client.DoltCommit(fmt.Sprintf("doctor-fix: migrate %d local registrations", migrated)); err != nil {
					fmt.Printf("    Warning: dolt commit failed: %s\n", err)
				}
			}
		},
	}
}

// credentialSpec maps a credential key to its possible env var overrides.
type credentialSpec struct {
	Key     string
	EnvVars []string
}

// credentialSpecs defines all required credentials and their env var overrides.
var credentialSpecs = []credentialSpec{
	{"anthropic-key", []string{"ANTHROPIC_API_KEY", "SPIRE_ANTHROPIC_KEY"}},
	{"github-token", []string{"GITHUB_TOKEN", "SPIRE_GITHUB_TOKEN"}},
	{"dolthub-user", []string{"DOLT_REMOTE_USER", "SPIRE_DOLTHUB_USER"}},
	{"dolthub-password", []string{"DOLT_REMOTE_PASSWORD", "SPIRE_DOLTHUB_PASSWORD"}},
}

// checkCredentials verifies the credential file and/or env var overrides.
func checkCredentials() checkResult {
	name := "credentials"

	dir, err := configDir()
	if err != nil {
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: "cannot determine config dir",
		}
	}
	credPath := filepath.Join(dir, "credentials")

	// Check file permissions if the file exists
	if info, statErr := os.Stat(credPath); statErr == nil {
		perm := info.Mode().Perm()
		if perm != 0600 {
			return checkResult{
				Name:   name,
				Status: statusOutdated,
				Detail: fmt.Sprintf("file permissions %04o (should be 0600)", perm),
				FixFunc: func() {
					if err := os.Chmod(credPath, 0600); err != nil {
						fmt.Printf("    Failed to fix permissions: %s\n", err)
					} else {
						fmt.Println("    credentials file permissions set to 0600")
					}
				},
			}
		}
	}

	// Parse the credentials file if it exists
	fileKeys := parseCredentialFile(credPath)

	var missing []string
	var present []string
	for _, spec := range credentialSpecs {
		found := false
		// Check file
		if _, ok := fileKeys[spec.Key]; ok {
			found = true
		}
		// Check env var overrides
		if !found {
			for _, env := range spec.EnvVars {
				if os.Getenv(env) != "" {
					found = true
					break
				}
			}
		}
		if found {
			present = append(present, spec.Key)
		} else {
			missing = append(missing, spec.Key)
		}
	}

	if len(missing) == 0 {
		return checkResult{
			Name:   name,
			Status: statusOK,
			Detail: fmt.Sprintf("%d of %d keys set", len(present), len(credentialSpecs)),
		}
	}

	if len(present) == 0 {
		// Nothing set at all
		detail := "missing: " + strings.Join(missing, ", ")
		if _, err := os.Stat(credPath); os.IsNotExist(err) {
			detail = "file does not exist; " + detail
		}
		return checkResult{
			Name:   name,
			Status: statusMissing,
			Detail: detail,
		}
	}

	return checkResult{
		Name:   name,
		Status: statusOutdated,
		Detail: fmt.Sprintf("missing: %s", strings.Join(missing, ", ")),
	}
}

// parseCredentialFile reads a flat key=value file and returns the keys that have non-empty values.
func parseCredentialFile(path string) map[string]bool {
	keys := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return keys
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if key != "" && val != "" {
				keys[key] = true
			}
		}
	}
	return keys
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
