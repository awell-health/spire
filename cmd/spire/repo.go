package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdRepo(args []string) error {
	if len(args) == 0 {
		return repoList(false)
	}
	switch args[0] {
	case "list":
		jsonOut := false
		for _, a := range args[1:] {
			if a == "--json" {
				jsonOut = true
			}
		}
		return repoList(jsonOut)
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: spire repo remove <prefix>")
		}
		return repoRemove(args[1])
	default:
		return fmt.Errorf("unknown repo subcommand: %q\nusage: spire repo [list|remove]", args[0])
	}
}

func cmdWorktree(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("usage: spire worktree <subcommand>")
		fmt.Println()
		fmt.Println("  remove    Unregister this directory from its instance,")
		fmt.Println("            leaving other worktrees intact.")
		fmt.Println("            If this is the last path, use: spire repo remove <prefix>")
		return nil
	}
	switch args[0] {
	case "remove":
		return worktreeRemove()
	default:
		return fmt.Errorf("unknown worktree subcommand: %q\nusage: spire worktree [remove]", args[0])
	}
}

func repoList(jsonOut bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if len(cfg.Instances) == 0 {
		fmt.Println("No repos init'd. Run `spire init` in a repo to get started.")
		return nil
	}

	if jsonOut {
		data, _ := json.MarshalIndent(cfg.Instances, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	// Print table
	fmt.Printf("%-10s %-12s %-10s %s\n", "PREFIX", "ROLE", "DATABASE", "PATH")
	for _, inst := range cfg.Instances {
		fmt.Printf("%-10s %-12s %-10s %s\n", inst.Prefix, inst.Role, inst.Database, inst.Path)
	}
	return nil
}

func repoRemove(prefix string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	inst, ok := cfg.Instances[prefix]
	if !ok {
		return fmt.Errorf("prefix %q not found in config", prefix)
	}

	// Clean up every registered path (primary + worktrees)
	for _, p := range allPaths(inst) {
		cleanSatelliteDir(p)
	}

	// If satellite, remove from hub's satellite list and regenerate routes
	if inst.Role == "satellite" && inst.Hub != "" {
		if hubInst, ok := cfg.Instances[inst.Hub]; ok {
			var updated []string
			for _, s := range hubInst.Satellites {
				if s != prefix {
					updated = append(updated, s)
				}
			}
			hubInst.Satellites = updated
			if err := regenerateRoutes(cfg, inst.Hub); err != nil {
				fmt.Printf("  Warning: route regeneration failed: %s\n", err)
			} else {
				fmt.Println("  Routes updated")
			}
		}
	}

	// If hub, warn about orphaned satellites
	if inst.Role == "hub" && len(inst.Satellites) > 0 {
		fmt.Printf("  Warning: hub %q has satellites: %s\n", prefix, strings.Join(inst.Satellites, ", "))
		fmt.Println("  Those satellites will need to be re-pointed or removed.")
	}

	delete(cfg.Instances, prefix)

	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("  Removed %q from config\n", prefix)
	return nil
}

// worktreeRemove removes the CWD from its instance's path list without touching other worktrees.
func worktreeRemove() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		return fmt.Errorf("this directory is not registered with spire")
	}

	// Count total paths
	total := 1 + len(inst.Paths)
	if total == 1 {
		return fmt.Errorf("%q has only one registered path — use: spire repo remove %s", inst.Prefix, inst.Prefix)
	}

	// Remove from the right place
	if inst.Path == cwd {
		// CWD is the primary path — promote first worktree to primary
		inst.Path = inst.Paths[0]
		inst.Paths = inst.Paths[1:]
		fmt.Printf("  Promoted %s to primary path\n", inst.Path)
	} else {
		removeFromPaths(inst, cwd)
	}

	// Clean up this directory
	cleanSatelliteDir(cwd)

	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("  Removed %s from %q (%d path(s) remaining)\n", cwd, inst.Prefix, 1+len(inst.Paths))
	return nil
}

// worktreeAdd registers cwd as an additional path for an existing instance.
// Called from spire init when the prefix is already registered elsewhere.
func worktreeAdd(cfg *SpireConfig, inst *Instance, cwd string) error {
	fmt.Printf("  Prefix %q is already registered at %s\n", inst.Prefix, inst.Path)
	fmt.Printf("  Adding %s as an additional path (worktree).\n", cwd)

	// Write .beads/redirect pointing to hub's .beads
	if inst.Role == "satellite" && inst.Hub != "" {
		hubInst, ok := cfg.Instances[inst.Hub]
		if !ok {
			return fmt.Errorf("hub %q not found in config", inst.Hub)
		}
		hubBeads := filepath.Join(hubInst.Path, ".beads")
		os.MkdirAll(filepath.Join(cwd, ".beads"), 0755)
		if err := os.WriteFile(filepath.Join(cwd, ".beads", "redirect"), []byte(hubBeads+"\n"), 0644); err != nil {
			return fmt.Errorf("write redirect: %w", err)
		}
		fmt.Printf("  Redirect → %s\n", hubBeads)
	}

	// Write .envrc
	envrcPath := filepath.Join(cwd, ".envrc")
	envrcContent := fmt.Sprintf("export SPIRE_IDENTITY=\"%s\"\n", inst.Prefix)
	if data, err := os.ReadFile(envrcPath); err == nil {
		if !strings.Contains(string(data), "SPIRE_IDENTITY") {
			os.WriteFile(envrcPath, append(data, []byte("\n"+envrcContent)...), 0644)
		}
	} else {
		os.WriteFile(envrcPath, []byte(envrcContent), 0644)
	}
	fmt.Printf("  .envrc written (SPIRE_IDENTITY=%s)\n", inst.Prefix)

	// Register additional path
	if !containsStr(inst.Paths, cwd) {
		inst.Paths = append(inst.Paths, cwd)
	}

	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("  Worktree registered (%d path(s) for %q)\n", 1+len(inst.Paths), inst.Prefix)
	return nil
}

// cleanSatelliteDir removes spire-managed files from a satellite/worktree directory.
func cleanSatelliteDir(dir string) {
	os.Remove(filepath.Join(dir, ".beads", "redirect"))
	os.Remove(filepath.Join(dir, ".beads", "routes.jsonl"))
	removeEnvrcEntry(filepath.Join(dir, ".envrc"), "SPIRE_IDENTITY")
}

// removeEnvrcEntry removes lines containing key from an .envrc file.
func removeEnvrcEntry(path, key string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, key) {
			kept = append(kept, line)
		}
	}
	result := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if result != "" {
		result += "\n"
	}
	os.WriteFile(path, []byte(result), 0644)
}
