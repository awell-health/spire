package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func cmdSync(args []string) error {
	merge := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--merge":
			merge = true
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire sync --merge

Three-way merge pull for diverged histories.

Run this when 'spire pull' fails because local and remote histories have
diverged. Unlike 'spire pull --force' (which overwrites local history),
'spire sync --merge' attempts a three-way merge, preserving commits from
both sides.

If the merge produces conflicts, dolt's output is printed verbatim so you
can identify and resolve them manually.

Options:
  --merge        Required. Perform the three-way merge pull.

Auth:
  Credentials are read from spire's credential store (spire config set dolthub-user,
  dolthub-password) or from DOLT_REMOTE_USER / DOLT_REMOTE_PASSWORD env vars.

Examples:
  spire sync --merge      # three-way merge pull from existing remote
`)
			return nil
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire sync --merge", args[i])
		}
	}

	if !merge {
		fmt.Println("Usage: spire sync --merge")
		fmt.Println()
		fmt.Println("  --merge    Three-way merge pull for diverged histories")
		fmt.Println()
		fmt.Println("Run 'spire sync --help' for more information.")
		return nil
	}

	return runSync()
}

func runSync() error {
	if err := requireDolt(); err != nil {
		return err
	}

	// ── Resolve database data directory ───────────────────────────────────────
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	out, _ := bd("dolt", "remote", "list")
	if !strings.Contains(out, "origin") {
		return fmt.Errorf("no remote configured\n  set one with 'spire pull <url>' first, or run: bd dolt remote add origin <url>")
	}
	// Sync SQL remote to CLI config in case it was set via bd but not CLI.
	if url := parseOriginURL(out); url != "" {
		setDoltCLIRemote(dataDir, "origin", url)
	}

	// ── Inject DoltHub credentials ────────────────────────────────────────────
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
		defer os.Unsetenv("DOLT_REMOTE_USER")
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
		defer os.Unsetenv("DOLT_REMOTE_PASSWORD")
	}

	// ── Record pre-merge commit for ownership enforcement ────────────────────
	dbName := readBeadsDBName()
	preCommit := ""
	if dbName != "" {
		preCommit = getCurrentCommitHash(dbName)
	}

	// ── Three-way merge: fetch then merge ─────────────────────────────────────
	// dolt pull fails on diverged histories (fast-forward only). Instead we run
	// dolt fetch (updates remotes/origin/main) then dolt merge (three-way merge),
	// which can reconcile commits from both sides without overwriting local history.
	fmt.Println("  Fetching from origin...")
	mergeOut, err := doltCLIFetchMerge(dataDir)
	mergeHadConflicts := err != nil

	// If merge produced conflicts, try automatic field-level resolution.
	if mergeHadConflicts && dbName != "" {
		resolved, resolveErr := resolveIssueConflicts(dbName)
		if resolveErr == nil && resolved > 0 {
			fmt.Printf("  Auto-resolved %d conflict(s) with field-level ownership rules.\n", resolved)
			err = nil // conflicts were resolved
		}
	}
	if err != nil {
		fmt.Println("  Merge failed — dolt output:")
		fmt.Println()
		fmt.Println(err.Error())
		fmt.Println()
		fmt.Println("  Resolve any conflicts manually, then commit with:")
		fmt.Println("    bd vc commit -m 'resolve merge conflicts'")
		return fmt.Errorf("sync --merge failed")
	}

	if mergeOut != "" {
		fmt.Println(mergeOut)
	}

	// ── Scan for status regressions (skip conflict resolution — already done above) ──
	if dbName != "" && preCommit != "" {
		regressions, scanErr := scanClusterRegressions(dbName, preCommit)
		if scanErr != nil {
			fmt.Printf("  Warning: regression scan: %s\n", scanErr)
		} else if len(regressions) > 0 {
			if repairErr := repairClusterRegressions(dbName, regressions); repairErr != nil {
				fmt.Printf("  Warning: repair regressions: %s\n", repairErr)
			} else {
				fmt.Printf("  Repaired %d status regression(s).\n", len(regressions))
			}
		}
	}

	fmt.Println("  Merge complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}

// doltCLIFetchMerge performs a three-way merge by running dolt fetch followed
// by dolt merge. Unlike dolt pull (fast-forward only), this can reconcile
// diverged histories by creating a merge commit. Returns the merge output.
func doltCLIFetchMerge(dataDir string) (string, error) {
	bin := doltBin()
	if bin == "" {
		return "", fmt.Errorf("dolt not found — run spire up to install")
	}

	env := os.Environ()

	// Step 1: fetch remote commits into remotes/origin/main.
	fetchCmd := exec.Command(bin, "fetch", "origin", "main")
	fetchCmd.Dir = dataDir
	fetchCmd.Env = env
	fetchOut, err := fetchCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(fetchOut)), fmt.Errorf("dolt fetch: %w\n%s", err, strings.TrimSpace(string(fetchOut)))
	}

	// Step 2: three-way merge into current branch.
	mergeCmd := exec.Command(bin, "merge", "remotes/origin/main")
	mergeCmd.Dir = dataDir
	mergeCmd.Env = env
	mergeOut, err := mergeCmd.CombinedOutput()
	output := strings.TrimSpace(string(mergeOut))
	if err != nil {
		return output, fmt.Errorf("dolt merge: %w\n%s", err, output)
	}
	return output, nil
}
