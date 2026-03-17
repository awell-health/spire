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

	// If satellite, remove from hub's satellite list
	if inst.Role == "satellite" && inst.Hub != "" {
		if hubInst, ok := cfg.Instances[inst.Hub]; ok {
			var updated []string
			for _, s := range hubInst.Satellites {
				if s != prefix {
					updated = append(updated, s)
				}
			}
			hubInst.Satellites = updated
		}

		// Remove redirect if it exists
		redirectPath := filepath.Join(inst.Path, ".beads", "redirect")
		os.Remove(redirectPath)
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
