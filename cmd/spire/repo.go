package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func cmdRepo(args []string) error {
	if len(args) == 0 {
		return repoList(false)
	}
	switch args[0] {
	case "add":
		// Delegate to the register-repo logic, passing remaining args
		return cmdRegisterRepo(args[1:])
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
		return fmt.Errorf("unknown repo subcommand: %q\nusage: spire repo [add|list|remove]", args[0])
	}
}

// repoList queries the dolt repos table (source of truth) for registered repos.
// Falls back to local config.json if dolt is not reachable.
func repoList(jsonOut bool) error {
	// Try shared state first (dolt repos table)
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Determine database name from active tower
	database := ""
	if cfg.ActiveTower != "" {
		if tower, err := loadTowerConfig(cfg.ActiveTower); err == nil {
			database = tower.Database
		}
	}
	if database == "" {
		// Fallback: find database from any instance
		for _, inst := range cfg.Instances {
			if inst.Database != "" {
				database = inst.Database
				break
			}
		}
	}

	if database != "" {
		sql := fmt.Sprintf("SELECT prefix, repo_url, branch, language, registered_by FROM `%s`.repos ORDER BY prefix", database)
		out, err := rawDoltQuery(sql)
		if err == nil && strings.TrimSpace(out) != "" {
			if jsonOut {
				// Parse the pipe-delimited output into JSON
				rows := parseDoltRows(out, []string{"prefix", "repo_url", "branch", "language", "registered_by"})
				data, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Println(string(data))
			} else {
				fmt.Println(out)
			}
			return nil
		}
		// Fall through to local config if dolt query failed
	}

	// Fallback: local config
	if len(cfg.Instances) == 0 {
		fmt.Println("No repos registered. Run `spire repo add` in a repo to get started.")
		return nil
	}

	if jsonOut {
		data, _ := json.MarshalIndent(cfg.Instances, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("%-10s %-10s %s\n", "PREFIX", "DATABASE", "PATH")
	for _, inst := range cfg.Instances {
		fmt.Printf("%-10s %-10s %s\n", inst.Prefix, inst.Database, inst.Path)
	}
	return nil
}

// repoRemove removes a repo from both the dolt repos table and local config.
func repoRemove(prefix string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Remove from dolt repos table if possible
	database := ""
	if cfg.ActiveTower != "" {
		if tower, err := loadTowerConfig(cfg.ActiveTower); err == nil {
			database = tower.Database
		}
	}
	if database != "" {
		sql := fmt.Sprintf("DELETE FROM `%s`.repos WHERE prefix = '%s'", database, prefix)
		if _, err := rawDoltQuery(sql); err != nil {
			fmt.Printf("  Warning: could not remove from repos table: %s\n", err)
		} else {
			fmt.Printf("  Removed %q from repos table\n", prefix)
		}
	}

	// Remove from local config
	if _, ok := cfg.Instances[prefix]; !ok {
		if database == "" {
			return fmt.Errorf("prefix %q not found", prefix)
		}
		// Already removed from dolt, just not in local config
		return nil
	}

	delete(cfg.Instances, prefix)
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("  Removed %q from local config\n", prefix)
	return nil
}

// parseDoltRows parses pipe-delimited dolt SQL output into a slice of maps.
func parseDoltRows(out string, columns []string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil
	}

	var rows []map[string]string
	// Skip header line (first line)
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		row := make(map[string]string)
		colIdx := 0
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if colIdx < len(columns) {
				row[columns[colIdx]] = p
				colIdx++
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}
