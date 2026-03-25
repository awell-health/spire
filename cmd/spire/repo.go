package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func cmdRepo(args []string) error {
	if len(args) == 0 {
		printRepoUsage()
		return nil
	}
	switch args[0] {
	case "add":
		// Delegate to repo-add logic, passing remaining args
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
	case "--help", "-h", "help":
		printRepoUsage()
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand: %q\nRun 'spire repo --help' for usage", args[0])
	}
}

func printRepoUsage() {
	fmt.Println(`Usage: spire repo <command> [args]

Manage repository registrations under a tower.

Commands:
  add [path]          Register a repo (--prefix, --repo-url, --branch)
  list                List registered repos (--json)
  remove <prefix>     Remove a repo registration

Run 'spire repo add --help' for details on registration.`)
}

// repoList queries the dolt repos table (source of truth) for registered repos.
// Falls back to local config.json if dolt is not reachable.
func repoList(jsonOut bool) error {
	// Try shared state first (dolt repos table)
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		return fmt.Errorf("multiple towers found — run 'spire tower use <name>' to set the active tower")
	}

	columns := []string{"prefix", "repo_url", "branch", "language", "registered_by"}

	if database != "" {
		sql := fmt.Sprintf("SELECT prefix, repo_url, branch, language, registered_by FROM `%s`.repos ORDER BY prefix", database)
		out, err := rawDoltQuery(sql)
		if err == nil {
			// Dolt reachable — this is authoritative, even if empty
			rows := parseDoltRows(out, columns)
			if len(rows) == 0 {
				if jsonOut {
					fmt.Println("[]")
				} else {
					fmt.Println("No repos registered. Run `spire repo add` in a repo to get started.")
				}
				return nil
			}
			if jsonOut {
				data, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Println(string(data))
			} else {
				fmt.Printf("%-10s %-50s %-10s %-12s %s\n", "PREFIX", "REPO", "BRANCH", "LANGUAGE", "REGISTERED BY")
				for _, r := range rows {
					fmt.Printf("%-10s %-50s %-10s %-12s %s\n", r["prefix"], r["repo_url"], r["branch"], r["language"], r["registered_by"])
				}
			}
			return nil
		}
		// Dolt not reachable — fall through to local config with warning
		fmt.Println("  (dolt not reachable — showing local cache)")
	}

	// Fallback: local config (not authoritative, only when no tower exists)
	if len(cfg.Instances) == 0 {
		if jsonOut {
			fmt.Println("[]")
		} else {
			fmt.Println("No repos registered. Run `spire repo add` in a repo to get started.")
		}
		return nil
	}

	if jsonOut {
		// Emit same shape as shared-state path for stable JSON API
		var rows []map[string]string
		for _, inst := range cfg.Instances {
			rows = append(rows, map[string]string{
				"prefix":        inst.Prefix,
				"repo_url":      "",
				"branch":        "",
				"language":      "",
				"registered_by": "",
			})
		}
		data, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("%-10s %-10s %s\n", "PREFIX", "DATABASE", "PATH")
	for _, inst := range cfg.Instances {
		fmt.Printf("%-10s %-10s %s\n", inst.Prefix, inst.Database, inst.Path)
	}
	return nil
}

// resolveRepoArg resolves a user-supplied argument to a canonical prefix.
// Tries exact prefix match first, then falls back to matching by directory path
// or basename, so users can type the directory name instead of the prefix.
// Returns the original arg unchanged if no local config match is found
// (letting dolt be the final arbiter).
func resolveRepoArg(cfg *SpireConfig, arg string) (string, error) {
	if _, ok := cfg.Instances[arg]; ok {
		return arg, nil
	}
	var matched []string
	for prefix, inst := range cfg.Instances {
		if inst.Path == arg || filepath.Base(inst.Path) == arg {
			matched = append(matched, prefix)
		}
	}
	switch len(matched) {
	case 0:
		return arg, nil // not in local config; dolt SELECT will catch non-existence
	case 1:
		fmt.Printf("  Resolved %q → prefix %q\n", arg, matched[0])
		return matched[0], nil
	default:
		return "", fmt.Errorf("ambiguous: %q matches prefixes %s — use the prefix directly", arg, strings.Join(matched, ", "))
	}
}

// resolveRemoveDatabase determines which database a repo remove should target.
// Priority: instance's tower config (authoritative) > instance's cached database
// > global tower resolution. Returns the database name or an error if ambiguous
// or unresolvable.
func resolveRemoveDatabase(cfg *SpireConfig, prefix string) (string, error) {
	// Resolve from the instance's own tower config (authoritative)
	// rather than the cached inst.Database (which can drift).
	if inst, ok := cfg.Instances[prefix]; ok {
		if inst.Tower != "" {
			if tower, err := loadTowerConfig(inst.Tower); err == nil && tower.Database != "" {
				return tower.Database, nil
			}
		}
		if inst.Database != "" {
			return inst.Database, nil
		}
	}

	// Fall back to global tower resolution
	db, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		return "", fmt.Errorf("multiple towers found — run 'spire tower use <name>' to set the active tower")
	}
	if db == "" {
		return "", fmt.Errorf("cannot resolve tower for prefix %q — run 'spire tower use <name>' to set the active tower", prefix)
	}
	return db, nil
}

// repoRemove removes a repo from both the dolt repos table and local config.
// Resolves the tower from the instance's own cache first (it knows which tower
// it was registered under), falling back to global tower resolution only if the
// instance doesn't record its tower.
func repoRemove(arg string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve arg to canonical prefix (accepts path or basename as fallback).
	prefix, err := resolveRepoArg(cfg, arg)
	if err != nil {
		return err
	}

	database, err := resolveRemoveDatabase(cfg, prefix)
	if err != nil {
		return err
	}

	// Verify the prefix exists in the repos table before attempting to delete.
	checkSQL := fmt.Sprintf("SELECT prefix FROM `%s`.repos WHERE prefix = '%s'", database, sqlEscape(prefix))
	out, err := rawDoltQuery(checkSQL)
	if err != nil {
		return fmt.Errorf("could not verify %q in repos table: %w", prefix, err)
	}
	rows := parseDoltRows(out, []string{"prefix"})
	if len(rows) == 0 {
		// Not in dolt — clean up local config if present and warn.
		if _, ok := cfg.Instances[prefix]; ok {
			delete(cfg.Instances, prefix)
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("  Removed %q from local config (was not in repos table)\n", prefix)
			return nil
		}
		return fmt.Errorf("no repo registered with prefix %q\nRun 'spire repo list' to see registered repos", prefix)
	}

	// Remove from authoritative repos table first.
	deleteSQL := fmt.Sprintf("DELETE FROM `%s`.repos WHERE prefix = '%s'", database, sqlEscape(prefix))
	if _, err := rawDoltQuery(deleteSQL); err != nil {
		return fmt.Errorf("could not remove %q from repos table: %w", prefix, err)
	}
	fmt.Printf("  Removed %q from repos table\n", prefix)

	// Then remove from local config.
	if _, ok := cfg.Instances[prefix]; ok {
		delete(cfg.Instances, prefix)
		if err := saveConfig(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Printf("  Removed %q from local config\n", prefix)
	}
	return nil
}

// parseDoltRows parses MySQL-style tabular dolt SQL output into a slice of maps.
// Dolt output format:
//
//	+--------+----------+
//	| prefix | repo_url |
//	+--------+----------+
//	| spi    | https... |
//	+--------+----------+
//
// Separator lines (+---+) and the header row (first | ... | line) are skipped.
func parseDoltRows(out string, columns []string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(out), "\n")

	var rows []map[string]string
	headerSkipped := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		// First pipe-delimited line is the header — skip it
		if !headerSkipped {
			headerSkipped = true
			continue
		}
		// Parse data row
		parts := strings.Split(line, "|")
		var cells []string
		for _, p := range parts {
			cells = append(cells, strings.TrimSpace(p))
		}
		// Strip leading/trailing empty boundary cells from "| a | b |"
		if len(cells) > 0 && cells[0] == "" {
			cells = cells[1:]
		}
		if len(cells) > 0 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}

		row := make(map[string]string)
		for i, col := range columns {
			if i < len(cells) {
				row[col] = cells[i]
			} else {
				row[col] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows
}
