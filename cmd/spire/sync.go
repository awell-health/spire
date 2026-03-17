package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cmdSync(args []string) error {
	mode := "merge"
	remoteURL := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--hard":
			mode = "hard"
		case args[i] == "--merge":
			mode = "merge"
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire sync [--hard|--merge] [<dolthub-url>]

Sync the local beads database with a DoltHub remote.
Handles divergent histories that 'bd dolt pull' cannot (e.g. fresh init vs existing remote).

Modes:
  --merge  (default) Export local issues, force pull from remote, reimport locals.
           Safe when both sides have data you want to keep.
  --hard   Force pull from remote, overwriting all local data.
           Use when local is empty or expendable.

Arguments:
  <dolthub-url>  Optional. Sets (or replaces) the 'origin' remote before syncing.
                 e.g. https://doltremoteapi.dolthub.com/org/repo

Auth:
  Set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD env vars for DoltHub.

Examples:
  spire sync                                                  # merge with existing remote
  spire sync https://doltremoteapi.dolthub.com/org/db         # merge, set remote first
  spire sync --hard https://doltremoteapi.dolthub.com/org/db  # hard reset to remote
  spire sync --hard                                           # hard reset, remote already set
`)
			return nil
		default:
			remoteURL = args[i]
		}
	}

	return runSync(mode, remoteURL)
}

// runSync is the core sync logic, also called by cmdInit when --dolthub is provided.
func runSync(mode, remoteURL string) error {
	// ── Remote setup ──────────────────────────────────────────────────────────
	if remoteURL != "" {
		out, _ := bd("dolt", "remote", "list")
		existingURL := parseOriginURL(out)
		if existingURL == "" {
			fmt.Printf("  Adding remote origin → %s\n", remoteURL)
			if _, err := bd("dolt", "remote", "add", "origin", remoteURL); err != nil {
				return fmt.Errorf("add remote: %w", err)
			}
		} else if existingURL != remoteURL {
			fmt.Printf("  Updating remote origin: %s → %s\n", existingURL, remoteURL)
			bd("dolt", "remote", "remove", "origin") //nolint
			if _, err := bd("dolt", "remote", "add", "origin", remoteURL); err != nil {
				return fmt.Errorf("add remote: %w", err)
			}
		} else {
			fmt.Printf("  Remote origin: %s\n", remoteURL)
		}
	} else {
		out, _ := bd("dolt", "remote", "list")
		if !strings.Contains(out, "origin") {
			return fmt.Errorf("no remote configured\n  pass a DoltHub URL or run: bd dolt remote add origin <url>")
		}
	}

	// ── Mode: merge — stash local issues ──────────────────────────────────────
	stashFile := ""
	if mode == "merge" {
		countStr, _ := bd("count")
		count := strings.TrimSpace(countStr)
		if count != "" && count != "0" {
			ts := time.Now().Format("20060102_150405")
			stashFile = filepath.Join(os.TempDir(), fmt.Sprintf("spire-sync-stash-%s.jsonl", ts))
			fmt.Printf("  Stashing %s local issue(s) → %s\n", count, stashFile)
			if _, err := bd("export", "-o", stashFile); err != nil {
				return fmt.Errorf("export stash: %w", err)
			}
			fmt.Println("  Stash saved.")
		} else {
			fmt.Println("  No local issues to stash.")
		}
	}

	// ── Commit any uncommitted working-set changes ─────────────────────────────
	vcStatus, _ := bd("vc", "status")
	if strings.Contains(vcStatus, "uncommitted") {
		fmt.Println("  Committing working-set changes before sync...")
		if _, err := bd("vc", "commit", "-m", "pre-sync: commit working set (spire sync)"); err != nil {
			return fmt.Errorf("commit working set: %w", err)
		}
		fmt.Println("  Working set committed.")
	}

	// ── Force fetch ────────────────────────────────────────────────────────────
	fmt.Println("  Fetching from origin...")
	_, fetchErr := bd("sql", "CALL dolt_fetch('origin', 'main')")
	if fetchErr != nil {
		// Retry without branch spec (some remotes don't need it)
		if _, err2 := bd("sql", "CALL dolt_fetch('origin')"); err2 != nil {
			return fmt.Errorf("dolt fetch failed: %w\n  (also tried without branch: %s)", fetchErr, err2)
		}
	}
	fmt.Println("  Fetch complete.")

	// ── Hard reset to remote ───────────────────────────────────────────────────
	fmt.Println("  Resetting to origin/main...")
	if _, err := bd("sql", "CALL dolt_reset('--hard', 'refs/remotes/origin/main')"); err != nil {
		return fmt.Errorf("dolt reset: %w", err)
	}
	fmt.Println("  Reset complete.")

	// ── Mode: merge — reimport stashed issues ──────────────────────────────────
	if mode == "merge" && stashFile != "" {
		if _, statErr := os.Stat(stashFile); statErr == nil {
			fmt.Println("  Reimporting stashed issues...")
			if _, err := bd("import", stashFile); err != nil {
				return fmt.Errorf("import stash: %w\n  stash preserved at: %s", err, stashFile)
			}
			fmt.Printf("  Import complete. Stash preserved at: %s\n", stashFile)
			fmt.Println()
			fmt.Println("  Tip: if imported IDs conflict with remote IDs, run:")
			fmt.Println("    bd rename-prefix <new-prefix->")
		}
	}

	fmt.Println()
	bd("status") //nolint
	fmt.Println("  Sync complete.")
	return nil
}

// parseOriginURL extracts the URL for the 'origin' remote from 'bd dolt remote list' output.
func parseOriginURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "origin") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	return ""
}
