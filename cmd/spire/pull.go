package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func cmdPull(args []string) error {
	remoteURL := ""
	force := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--force":
			force = true
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire pull [--force] [<dolthub-url>]

Pull the beads database from a DoltHub remote.
Counterpart to 'spire push'.

By default, runs a normal pull (three-way merge). If histories have diverged
and the pull fails, it tells you and suggests --force.

Options:
  --force        Force pull, overwriting local history.

Arguments:
  <dolthub-url>  Optional. Sets (or replaces) the 'origin' remote before pulling.
                 Short form 'org/repo' is accepted.
                 e.g. awell/my-db  or  https://doltremoteapi.dolthub.com/awell/my-db

Auth:
  Credentials are read from spire's credential store (spire config set dolthub-user,
  dolthub-password) or from DOLT_REMOTE_USER / DOLT_REMOTE_PASSWORD env vars.

Examples:
  spire pull                              # pull from existing remote
  spire pull awell/my-db                 # set remote and pull
  spire pull --force                      # force pull (overwrite local)
`)
			return nil
		default:
			remoteURL = args[i]
		}
	}

	return runPull(remoteURL, force)
}

func runPull(remoteURL string, force bool) error {
	if err := requireDolt(); err != nil {
		return err
	}

	// ── Resolve database data directory ───────────────────────────────────────
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	if remoteURL != "" {
		remoteURL = normalizeDolthubURL(remoteURL)

		// Set remote in both SQL (for bd) and CLI (for direct pull).
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

	// ── Inject DoltHub credentials ────────────────────────────────────────────
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
	}

	// ── Pull via dolt CLI ─────────────────────────────────────────────────────
	fmt.Println("  Pulling from origin...")
	if err := doltCLIPull(dataDir, force); err != nil {
		if !force && (strings.Contains(err.Error(), "non-fast-forward") ||
			strings.Contains(err.Error(), "diverged") ||
			strings.Contains(err.Error(), "conflicts") ||
			strings.Contains(err.Error(), "cannot merge")) {
			fmt.Println("  Pull failed — histories have diverged.")
			fmt.Println()
			fmt.Println("  To resolve, re-run with --force:")
			fmt.Println("    spire pull --force")
			return fmt.Errorf("pull failed (diverged histories)")
		}
		return fmt.Errorf("dolt pull: %w", err)
	}

	fmt.Println("  Pull complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}

// doltCLIPull runs `dolt pull origin main` directly from the database data
// directory, inheriting the caller's environment so DOLT_REMOTE_USER /
// DOLT_REMOTE_PASSWORD are available.
func doltCLIPull(dataDir string, force bool) error {
	bin := doltBin()
	if bin == "" {
		return fmt.Errorf("dolt not found — run spire up to install")
	}

	args := []string{"pull", "origin", "main"}
	if force {
		args = []string{"pull", "--force", "origin", "main"}
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
