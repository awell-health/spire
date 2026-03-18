package main

import (
	"encoding/json"
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
	// ── Bootstrap: ensure local database exists ────────────────────────────────
	if _, err := bd("status"); err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "not found") || strings.Contains(errStr, "does not exist") {
			prefix, bootstrapErr := bootstrapDatabase(mode == "hard")
			if bootstrapErr != nil {
				return fmt.Errorf("database not found and bootstrap failed: %w\n  run manually: bd init --prefix <prefix>", bootstrapErr)
			}
			fmt.Printf("  Database bootstrapped (prefix: %s-).\n", prefix)
		}
		// Other errors (server down, etc.) will surface later in context
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	if remoteURL != "" {
		remoteURL = normalizeDolthubURL(remoteURL)
		out, _ := bd("dolt", "remote", "list")
		existingURL := parseOriginURL(out)
		if existingURL == "" {
			fmt.Printf("  Adding remote origin → %s\n", remoteURL)
			if _, err := bd("dolt", "remote", "add", "origin", remoteURL); err != nil {
				return fmt.Errorf("add remote: %w", err)
			}
		} else if existingURL != remoteURL {
			fmt.Printf("  Updating remote origin: %s → %s\n", existingURL, remoteURL)
			// Add under a temp name first; only remove old if add succeeds.
			if _, err := bd("dolt", "remote", "add", "origin-new", remoteURL); err != nil {
				return fmt.Errorf("add remote: %w", err)
			}
			bd("dolt", "remote", "remove", "origin")         //nolint
			bd("dolt", "remote", "add", "origin", remoteURL) //nolint
			bd("dolt", "remote", "remove", "origin-new")     //nolint
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

	// ── Guard: verify remote has beads schema before resetting ────────────────
	// If the remote has no issues table the reset would wipe a valid local schema.
	hasSchema, _ := bd("sql", "SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='issues' LIMIT 1")
	remoteHasSchema := strings.TrimSpace(hasSchema) != "" && strings.Contains(hasSchema, "1")
	if !remoteHasSchema {
		// Check if remote branch has the table
		remoteCheck, _ := bd("sql", "CALL dolt_checkout('refs/remotes/origin/main'); SELECT COUNT(*) FROM information_schema.tables WHERE table_name='issues'")
		remoteHasSchema = strings.Contains(remoteCheck, "1")
		// Restore local branch regardless
		bd("sql", "CALL dolt_checkout('main')") //nolint
	}
	if !remoteHasSchema {
		return fmt.Errorf("remote has no beads schema (missing 'issues' table)\n" +
			"  The remote may be a raw dolt repo, not a beads database.\n" +
			"  Initialize locally first with 'bd init --prefix <prefix>', then push to the remote.")
	}

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

// normalizeDolthubURL expands a short "org/repo" form to the full DoltHub API URL.
// Full URLs (http/https) are returned unchanged.
func normalizeDolthubURL(url string) string {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	// Looks like "org/repo" — expand to full DoltHub URL
	return "https://doltremoteapi.dolthub.com/" + url
}

// bootstrapDatabase creates the local Dolt database when it doesn't exist yet.
// Instead of running bd init (which may prompt for confirmation when backups exist),
// it creates the database directly on the server via dolt SQL. The subsequent
// fetch+reset will populate the schema and data from the remote.
func bootstrapDatabase(_ bool) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}

	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		return "", fmt.Errorf("directory %s is not registered with spire — run spire init first", cwd)
	}

	// Read the actual database name bd expects from .beads/metadata.json.
	// bd may name it differently from the prefix (e.g. "beads_mlti" vs "mlti").
	dbName := readBeadsDBName()
	if dbName == "" {
		dbName = inst.Database
	}

	fmt.Printf("  Database not found — creating %q on dolt server...\n", dbName)
	if err := ensureDatabase(dbName); err != nil {
		return "", fmt.Errorf("create database %q: %w", dbName, err)
	}
	return inst.Prefix, nil
}

// readBeadsDBName reads the dolt_database field from .beads/metadata.json,
// which is the actual database name bd uses to connect to the dolt server.
func readBeadsDBName() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Walk up to find .beads/
	for dir := cwd; ; dir = filepath.Dir(dir) {
		meta := filepath.Join(dir, ".beads", "metadata.json")
		if data, err := os.ReadFile(meta); err == nil {
			var m struct {
				DoltDatabase string `json:"dolt_database"`
			}
			if err := json.Unmarshal(data, &m); err == nil && m.DoltDatabase != "" {
				return m.DoltDatabase
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
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
