package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func cmdPush(args []string) error {
	remoteURL := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire push [<dolthub-url>]

Push the local beads database to a DoltHub remote.
Counterpart to 'spire sync' (which pulls).

If the DoltHub database does not exist and DOLT_REMOTE_PASSWORD is set,
spire push creates it first.

Arguments:
  <dolthub-url>  Optional. Sets (or replaces) the 'origin' remote before pushing.
                 Short form 'org/repo' is accepted.
                 e.g. awell/my-db  or  https://doltremoteapi.dolthub.com/awell/my-db

Auth:
  Set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD env vars for DoltHub.
  Credentials must be present in the calling process environment (not just the
  dolt server) because the push uses the dolt CLI directly.

Examples:
  spire push                              # push to existing remote
  spire push awell/my-db                 # set remote and push
  spire push https://doltremoteapi.dolthub.com/awell/my-db
`)
			return nil
		default:
			remoteURL = args[i]
		}
	}

	return runPush(remoteURL)
}

func runPush(remoteURL string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	// ── Resolve database name ──────────────────────────────────────────────────
	// We need the actual dolt data directory to run push client-side.
	dbName := readBeadsDBName()
	if dbName == "" {
		return fmt.Errorf("could not determine database name — run from a directory with .beads/")
	}
	dataDir := filepath.Join(doltDataDir(), dbName)
	if _, err := os.Stat(dataDir); err != nil {
		return fmt.Errorf("database directory not found: %s", dataDir)
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	if remoteURL != "" {
		remoteURL = normalizeDolthubURL(remoteURL)

		// Best-effort: create the DoltHub database if it doesn't exist yet.
		// Non-fatal — push will report the real error if this silently fails.
		if err := ensureDoltHubDB(remoteURL); err != nil {
			fmt.Printf("  Note: could not pre-create remote db: %s\n", err)
		}

		// Set remote in both SQL (for bd) and CLI (for direct push).
		out, _ := bd("dolt", "remote", "list")
		existingURL := parseOriginURL(out)
		if existingURL == "" {
			fmt.Printf("  Adding remote origin → %s\n", remoteURL)
			bd("dolt", "remote", "add", "origin", remoteURL) //nolint — SQL remote
		} else if existingURL != remoteURL {
			fmt.Printf("  Updating remote origin: %s → %s\n", existingURL, remoteURL)
			bd("dolt", "remote", "add", "origin-new", remoteURL) //nolint
			bd("dolt", "remote", "remove", "origin")             //nolint
			bd("dolt", "remote", "add", "origin", remoteURL)     //nolint
			bd("dolt", "remote", "remove", "origin-new")         //nolint
		} else {
			fmt.Printf("  Remote origin: %s\n", remoteURL)
		}

		// Also write the CLI remote directly into the data dir.
		// bd dolt remote add writes to SQL tables; dolt push (CLI) reads
		// from .dolt/config.json in the data directory — they're separate.
		setDoltCLIRemote(dataDir, "origin", remoteURL)
	} else {
		out, _ := bd("dolt", "remote", "list")
		if !strings.Contains(out, "origin") {
			return fmt.Errorf("no remote configured\n  pass a DoltHub URL or run: bd dolt remote add origin <url>")
		}
		// Sync SQL remote to CLI config in case it was set via bd but not CLI.
		if url := parseOriginURL(out); url != "" {
			setDoltCLIRemote(dataDir, "origin", url)
		}
	}

	// ── Commit any uncommitted working-set changes ────────────────────────────
	vcStatus, _ := bd("vc", "status")
	if strings.Contains(vcStatus, "uncommitted") {
		fmt.Println("  Committing working-set changes before push...")
		if _, err := bd("vc", "commit", "-m", "pre-push: commit working set (spire push)"); err != nil {
			return fmt.Errorf("commit working set: %w", err)
		}
	}

	// ── Push via dolt CLI (not bd) ────────────────────────────────────────────
	// bd routes dolt push through the SQL server (CALL dolt_push()), which
	// doesn't inherit the caller's credential environment. The CLI binary
	// reads DOLT_REMOTE_USER/DOLT_REMOTE_PASSWORD directly. This is the
	// standard bootstrap path for local-first operation.
	fmt.Println("  Pushing to origin...")
	if err := doltCLIPush(dataDir, false); err != nil {
		if strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(err.Error(), "no common ancestor") {
			fmt.Println("  Divergent history — retrying with --force...")
			if err2 := doltCLIPush(dataDir, true); err2 != nil {
				return fmt.Errorf("dolt push (force): %w", err2)
			}
		} else {
			return fmt.Errorf("dolt push: %w", err)
		}
	}

	fmt.Println("  Push complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}

// doltCLIPush runs `dolt push origin main` directly from the database data
// directory, inheriting the caller's environment so DOLT_REMOTE_USER /
// DOLT_REMOTE_PASSWORD are available.
func doltCLIPush(dataDir string, force bool) error {
	bin := doltBin()
	if bin == "" {
		return fmt.Errorf("dolt not found — run spire up to install")
	}

	args := []string{"push", "origin", "main"}
	if force {
		args = []string{"push", "--force", "origin", "main"}
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = dataDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setDoltCLIRemote adds or updates a remote in the dolt CLI config (.dolt/config.json)
// inside the database data directory. This is separate from the SQL-level remote
// managed by bd, which lives in the dolt database tables.
func setDoltCLIRemote(dataDir, name, url string) {
	bin := doltBin()
	if bin == "" {
		return
	}

	// Remove existing, ignore error
	removeCmd := exec.Command(bin, "remote", "remove", name)
	removeCmd.Dir = dataDir
	removeCmd.Env = os.Environ()
	removeCmd.Run() //nolint

	addCmd := exec.Command(bin, "remote", "add", name, url)
	addCmd.Dir = dataDir
	addCmd.Env = os.Environ()
	addCmd.Run() //nolint
}

// ensureDoltHubDB creates the DoltHub database if it doesn't exist.
// Requires DOLT_REMOTE_PASSWORD env var for auth.
// Non-fatal: if the database already exists or creation fails, push will
// surface the real error.
func ensureDoltHubDB(remoteURL string) error {
	suffix := strings.TrimPrefix(remoteURL, "https://doltremoteapi.dolthub.com/")
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("cannot parse org/repo from URL: %s", remoteURL)
	}
	owner, repo := parts[0], parts[1]

	token := os.Getenv("DOLT_REMOTE_PASSWORD")
	if token == "" {
		return nil // no token — let push surface auth error
	}

	// Check if database already exists
	checkURL := fmt.Sprintf("https://www.dolthub.com/api/v1alpha1/%s/%s", owner, repo)
	req, _ := http.NewRequest("GET", checkURL, nil)
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil // network issue — let push try anyway
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil // already exists
	}

	// Create it
	fmt.Printf("  Creating remote database %s/%s on DoltHub...\n", owner, repo)
	body := fmt.Sprintf(`{"ownerName":%q,"repoName":%q,"description":"Created by spire push","visibility":"private"}`,
		owner, repo)
	req, _ = http.NewRequest("POST", "https://www.dolthub.com/api/v1alpha1/database", strings.NewReader(body))
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("create db request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		var errResp struct {
			Message string `json:"message"`
		}
		json.Unmarshal(respBody, &errResp)
		if errResp.Message != "" {
			return fmt.Errorf("create db: %s", errResp.Message)
		}
		return fmt.Errorf("create db: HTTP %d", resp.StatusCode)
	}

	fmt.Printf("  Created %s/%s\n", owner, repo)
	return nil
}
