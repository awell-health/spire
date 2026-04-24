package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var setRepoURL string
var setBranch string

var repoCmd = &cobra.Command{
	Use:   "repo",
	Short: "Manage repository registrations",
	RunE: func(cmd *cobra.Command, args []string) error {
		// No subcommand: print usage
		cmd.Help()
		return nil
	},
}

var repoAddCmd = &cobra.Command{
	Use:   "add [path]",
	Short: "Register a repo under a tower",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("prefix"); v != "" {
			fullArgs = append(fullArgs, "--prefix", v)
		}
		if v, _ := cmd.Flags().GetString("repo-url"); v != "" {
			fullArgs = append(fullArgs, "--repo-url", v)
		}
		if v, _ := cmd.Flags().GetString("branch"); v != "" {
			fullArgs = append(fullArgs, "--branch", v)
		}
		if yes, _ := cmd.Flags().GetBool("yes"); yes {
			fullArgs = append(fullArgs, "--yes")
		}
		fullArgs = append(fullArgs, args...)
		return cmdRegisterRepo(fullArgs)
	},
}

var repoListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered repos",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		return repoList(jsonOut)
	},
}

var repoRemoveCmd = &cobra.Command{
	Use:   "remove <prefix>",
	Short: "Remove a repo registration",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return repoRemove(args[0])
	},
}

var repoSetCmd = &cobra.Command{
	Use:   "set <prefix>",
	Short: "Update shared repo fields (repo-url, branch) for an existing prefix",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return repoSet(args[0], setRepoURL, setBranch)
	},
}

var repoBindCmd = &cobra.Command{
	Use:   "bind <prefix> [path]",
	Short: "Bind a local checkout to a shared repo prefix, or skip/unmanage it on this machine",
	Long: `Adopt a shared repo registration by binding a local checkout path to it.
This does not modify the shared tower state — it records this machine's
relationship to an already-registered prefix.

Examples:
  spire repo bind api ./api           bind ./api as the local checkout for prefix api
  spire repo bind api --skip          mark api as intentionally skipped on this machine
  spire repo bind api --unmanaged     mark api as unmanaged on this machine`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdRepoBind(args, cmd)
	},
}

func init() {
	repoAddCmd.Flags().String("prefix", "", "Repo prefix (default: first 3 chars of directory name)")
	repoAddCmd.Flags().String("repo-url", "", "Git remote URL (default: git remote get-url origin)")
	repoAddCmd.Flags().String("branch", "", "Default branch (default: current branch or \"main\")")
	repoAddCmd.Flags().BoolP("yes", "y", false, "Accept all detected defaults without prompting")

	repoListCmd.Flags().Bool("json", false, "Output as JSON")

	repoSetCmd.Flags().StringVar(&setRepoURL, "repo-url", "", "New remote URL for the shared repo registration")
	repoSetCmd.Flags().StringVar(&setBranch, "branch", "", "New shared default base branch")

	repoBindCmd.Flags().Bool("skip", false, "Mark this repo as skipped on this machine")
	repoBindCmd.Flags().Bool("unmanaged", false, "Mark this repo as unmanaged on this machine")

	repoCmd.AddCommand(repoAddCmd, repoListCmd, repoRemoveCmd, repoSetCmd, repoBindCmd)
}

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
	case "bind":
		return repoBindCmd.RunE(repoBindCmd, args[1:])
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: spire repo remove <prefix>")
		}
		return repoRemove(args[1])
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: spire repo set <prefix> [--repo-url <url>] [--branch <branch>]")
		}
		fs := flag.NewFlagSet("repo set", flag.ContinueOnError)
		var u, b string
		fs.StringVar(&u, "repo-url", "", "")
		fs.StringVar(&b, "branch", "", "")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return repoSet(args[1], u, b)
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
  bind <prefix> [path] Bind a local checkout to a shared repo prefix
  list                List registered repos (--json)
  remove <prefix>     Remove a repo registration
  set <prefix>        Update shared repo fields (--repo-url, --branch)

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

			// Load local bindings from tower config.
			var bindings map[string]*config.LocalRepoBinding
			if tc, err := towerConfigForDatabase(database); err == nil && tc.LocalBindings != nil {
				bindings = tc.LocalBindings
			}

			if jsonOut {
				// Merge binding state into each row for JSON output.
				for _, r := range rows {
					if b := bindings[r["prefix"]]; b != nil {
						r["local_state"] = b.State
						r["local_path"] = b.LocalPath
					}
				}
				data, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Println(string(data))
			} else {
				useColor := colorOutputEnabled()
				fmt.Printf("%-10s %-50s %-10s %-12s %s\n", "PREFIX", "REPO", "BRANCH", "LANGUAGE", "LOCAL")
				for _, r := range rows {
					local := renderLocalBindingCell(bindings[r["prefix"]], useColor)
					fmt.Printf("%-10s %-50s %-10s %-12s %s\n", r["prefix"], r["repo_url"], r["branch"], r["language"], local)
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

// repoSet updates shared repo fields (repo_url, branch) for an existing prefix
// in the tower's repos table. This mutates tower-wide shared state only — it does
// not touch machine-local config or binding state.
func repoSet(prefix, newURL, newBranch string) error {
	if newURL == "" && newBranch == "" {
		return fmt.Errorf("nothing to update: specify --repo-url and/or --branch")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	database, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		return fmt.Errorf("multiple towers found; set SPIRE_TOWER or run: spire tower use <name>")
	}
	if database == "" {
		return fmt.Errorf("no tower found; run spire tower create or spire tower attach")
	}

	// Verify the prefix is registered
	checkSQL := fmt.Sprintf(
		"SELECT prefix FROM `%s`.repos WHERE prefix = '%s'",
		database, sqlEscape(prefix),
	)
	out, err := rawDoltQuery(checkSQL)
	if err != nil {
		return fmt.Errorf("could not verify %q in repos table: %w", prefix, err)
	}
	rows := parseDoltRows(out, []string{"prefix"})
	if len(rows) == 0 {
		return fmt.Errorf("repo %q not registered in tower", prefix)
	}

	// Build SET clause from provided fields only
	var parts []string
	if newURL != "" {
		parts = append(parts, fmt.Sprintf("repo_url = '%s'", sqlEscape(newURL)))
	}
	if newBranch != "" {
		parts = append(parts, fmt.Sprintf("branch = '%s'", sqlEscape(newBranch)))
	}

	updateSQL := fmt.Sprintf(
		"UPDATE `%s`.repos SET %s WHERE prefix = '%s'",
		database, strings.Join(parts, ", "), sqlEscape(prefix),
	)
	if _, err := rawDoltQuery(updateSQL); err != nil {
		return fmt.Errorf("update repo %q: %w", prefix, err)
	}

	fmt.Printf("repo %q updated in tower\n", prefix)
	return nil
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
// Delegates to pkg/dolt.ParseDoltRows.
func parseDoltRows(out string, columns []string) []map[string]string {
	return dolt.ParseDoltRows(out, columns)
}

// colorOutputEnabled reports whether ANSI color escapes should be
// emitted. Honors NO_COLOR (see no-color.org) and falls back to an
// stdout-TTY probe so pipes, CI logs, and redirections stay plain.
func colorOutputEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// renderLocalBindingCell formats the LOCAL column of `spire repo list`.
// Unbound / skipped / unmanaged states get a visible `!` prefix (and
// red+bold coloring when a TTY is attached) so they don't visually
// blend with bound entries — see spi-rpuzs6. When NO_COLOR is set or
// stdout is not a TTY, only the plain-text `!` prefix remains.
func renderLocalBindingCell(b *config.LocalRepoBinding, useColor bool) string {
	if b == nil {
		if useColor {
			return red + bold + "! unbound" + reset
		}
		return "! unbound"
	}
	switch b.State {
	case "bound":
		if b.LocalPath != "" {
			return fmt.Sprintf("bound (%s)", b.LocalPath)
		}
		return "bound"
	case "unbound":
		if useColor {
			return red + bold + "! unbound" + reset
		}
		return "! unbound"
	case "skipped":
		if useColor {
			return yellow + "! skipped" + reset
		}
		return "! skipped"
	case "unmanaged":
		if useColor {
			return dim + "unmanaged" + reset
		}
		return "unmanaged"
	default:
		return b.State
	}
}
