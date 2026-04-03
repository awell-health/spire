package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/spf13/cobra"
)

// performRepoBind validates localPath, bootstraps .beads/, and updates
// tower.LocalBindings and cfg.Instances in place. The caller is responsible
// for persisting both configs after the call succeeds.
func performRepoBind(tower *TowerConfig, cfg *SpireConfig, prefix, localPath, repoURL, branch string) error {
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return fmt.Errorf("path %s does not exist", absPath)
	}
	if _, err := os.Stat(filepath.Join(absPath, ".git")); err != nil {
		return fmt.Errorf("%s does not appear to be a git repository", absPath)
	}

	// Bootstrap .beads/ in the local path (idempotent — safe to re-run).
	beadsDir := filepath.Join(absPath, ".beads")
	if err := bootstrapRepoBeadsDir(beadsDir, tower, prefix); err != nil {
		return fmt.Errorf("bootstrap .beads: %w", err)
	}
	ensureGitignoreEntry(absPath, ".beads/")

	// Update tower local binding.
	if tower.LocalBindings == nil {
		tower.LocalBindings = make(map[string]*config.LocalRepoBinding)
	}
	tower.LocalBindings[prefix] = &config.LocalRepoBinding{
		Prefix:       prefix,
		LocalPath:    absPath,
		State:        "bound",
		RepoURL:      repoURL,
		SharedBranch: branch,
		BoundAt:      time.Now(),
	}

	// Register Instance for backward compat (code paths that range over Instances).
	cfg.Instances[prefix] = &Instance{
		Path:     absPath,
		Prefix:   prefix,
		Database: tower.Database,
		Tower:    tower.Name,
	}

	return nil
}

// cmdRepoBind implements `spire repo bind`.
// It does NOT write to the shared dolt repos table. It only writes local config state.
func cmdRepoBind(args []string, cmd *cobra.Command) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire repo bind <prefix> [path] [--skip|--unmanaged]")
	}
	prefix := args[0]

	skipFlag, _ := cmd.Flags().GetBool("skip")
	unmanagedFlag, _ := cmd.Flags().GetBool("unmanaged")

	if skipFlag && unmanagedFlag {
		return fmt.Errorf("--skip and --unmanaged are mutually exclusive")
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Verify prefix exists in shared state (must be in dolt repos table).
	// This prevents binding a prefix that was never registered.
	database, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		return fmt.Errorf("multiple towers — run 'spire tower use <name>' first")
	}
	if database == "" {
		return fmt.Errorf("no active tower found — run 'spire tower attach' first")
	}

	sql := fmt.Sprintf(
		"SELECT repo_url, branch FROM `%s`.repos WHERE prefix = '%s'",
		database, sqlEscape(prefix),
	)
	out, err := rawDoltQuery(sql)
	if err != nil {
		return fmt.Errorf("dolt not reachable — run 'spire up' first")
	}
	rows := parseDoltRows(out, []string{"repo_url", "branch"})
	if len(rows) == 0 {
		return fmt.Errorf("prefix %q is not registered in this tower — run 'spire repo add' from the repo directory to register it first", prefix)
	}
	sharedRepoURL := rows[0]["repo_url"]
	sharedBranch := rows[0]["branch"]

	// Resolve tower config so we can update LocalBindings.
	tower, err := config.ResolveTowerConfig()
	if err != nil {
		return fmt.Errorf("resolve tower: %w", err)
	}
	if tower.LocalBindings == nil {
		tower.LocalBindings = make(map[string]*config.LocalRepoBinding)
	}

	switch {
	case skipFlag:
		tower.LocalBindings[prefix] = &config.LocalRepoBinding{
			Prefix: prefix,
			State:  "skipped",
		}
		if err := config.SaveTowerConfig(tower); err != nil {
			return err
		}
		fmt.Printf("Marked %s as skipped on this machine.\n", prefix)
		fmt.Printf("  Shared repo: %s (branch: %s)\n", sharedRepoURL, sharedBranch)
		fmt.Printf("  Run 'spire repo bind %s [path]' later to adopt it.\n", prefix)
		return nil

	case unmanagedFlag:
		tower.LocalBindings[prefix] = &config.LocalRepoBinding{
			Prefix: prefix,
			State:  "unmanaged",
		}
		if err := config.SaveTowerConfig(tower); err != nil {
			return err
		}
		fmt.Printf("Marked %s as unmanaged on this machine.\n", prefix)
		return nil
	}

	// Bind a local path.
	localPath := "."
	if len(args) >= 2 {
		localPath = args[1]
	}

	if err := performRepoBind(tower, cfg, prefix, localPath, sharedRepoURL, sharedBranch); err != nil {
		return err
	}
	if err := config.SaveTowerConfig(tower); err != nil {
		return err
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	absPath := tower.LocalBindings[prefix].LocalPath
	fmt.Printf("Bound prefix %s → %s\n", prefix, absPath)
	fmt.Printf("  Shared repo: %s (branch: %s)\n", sharedRepoURL, sharedBranch)
	fmt.Printf("  Run 'spire up' if the daemon is not running, then 'bd list' to verify.\n")
	return nil
}
