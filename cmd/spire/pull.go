package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull [url]",
	Short: "Pull from DoltHub (fast-forward; --force to overwrite)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if force, _ := cmd.Flags().GetBool("force"); force {
			fullArgs = append(fullArgs, "--force")
		}
		fullArgs = append(fullArgs, args...)
		return cmdPull(fullArgs)
	},
}

func init() {
	pullCmd.Flags().Bool("force", false, "Force overwrite local changes")
}

func cmdPull(args []string) error {
	remoteURL := ""
	force := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--force":
			force = true
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire pull [--force] [<dolthub-url>]

Pull the beads database from a DoltHub remote (fast-forward).
Counterpart to 'spire push'.

If histories have diverged and the fast-forward pull fails:
  - Run 'spire sync --merge' to attempt a three-way merge (preserves both sides).
  - Run 'spire pull --force' to overwrite local history with the remote (destructive).

Options:
  --force        Force pull, overwriting local history with the remote.

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
  spire sync --merge                      # three-way merge for diverged histories
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
		dolt.SetCLIRemote(dataDir, "origin", remoteURL)
	} else {
		out, _ := bd("dolt", "remote", "list")
		if !strings.Contains(out, "origin") {
			return fmt.Errorf("no remote configured\n  pass a DoltHub URL or run: bd dolt remote add origin <url>")
		}
		// Sync SQL remote to CLI config in case it was set via bd but not CLI.
		if url := parseOriginURL(out); url != "" {
			dolt.SetCLIRemote(dataDir, "origin", url)
		}
	}

	// ── Inject DoltHub credentials ────────────────────────────────────────────
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
	}

	// ── Record pre-pull commit for ownership enforcement ─────────────────────
	dbName := readBeadsDBName()
	preCommit := ""
	if dbName != "" {
		preCommit = dolt.GetCurrentCommitHash(dbName)
	}

	// ── Pull via dolt CLI ─────────────────────────────────────────────────────
	fmt.Println("  Pulling from origin...")
	if err := dolt.CLIPull(dataDir, force); err != nil {
		if !force && (strings.Contains(err.Error(), "non-fast-forward") ||
			strings.Contains(err.Error(), "diverged") ||
			strings.Contains(err.Error(), "conflicts") ||
			strings.Contains(err.Error(), "cannot merge")) {
			fmt.Println("  Pull failed — histories have diverged.")
			fmt.Println()
			fmt.Println("  To attempt a three-way merge (preserves both sides), run:")
			fmt.Println("    spire sync --merge")
			fmt.Println()
			fmt.Println("  To overwrite local history with the remote (destructive), run:")
			fmt.Println("    spire pull --force")
			return fmt.Errorf("pull failed (diverged histories)")
		}
		return fmt.Errorf("dolt pull: %w", err)
	}

	// ── Enforce field-level ownership ─────────────────────────────────────────
	if dbName != "" && preCommit != "" {
		if ownerErr := dolt.ApplyMergeOwnership(dbName, preCommit); ownerErr != nil {
			fmt.Printf("  Warning: ownership enforcement: %s\n", ownerErr)
		}
	}

	fmt.Println("  Pull complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}
