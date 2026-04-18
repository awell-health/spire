package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push [url]",
	Short: "Push local database to DoltHub",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdPush(args)
	},
}

func cmdPush(args []string) error {
	remoteURL := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire push [<dolthub-url>]

Push the local beads database to a DoltHub remote.
Counterpart to 'spire pull'.

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

	// ── Resolve tower (to know the remote kind) ──────────────────────────────
	tower, _ := activeTowerConfig()
	remoteKind := config.RemoteKindDoltHub
	if tower != nil {
		remoteKind = tower.EffectiveRemoteKind()
	}

	// ── Inject remote credentials ────────────────────────────────────────────
	// dolt CLI reads DOLT_REMOTE_USER / DOLT_REMOTE_PASSWORD directly. Both
	// DoltHub and remotesapi use these env var names; only the source differs.
	if user, pass := config.RemoteCredentials(tower); user != "" || pass != "" {
		if user != "" {
			os.Setenv("DOLT_REMOTE_USER", user)
		}
		if pass != "" {
			os.Setenv("DOLT_REMOTE_PASSWORD", pass)
		}
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	if remoteURL != "" {
		// Classify the passed URL. If it disagrees with the stored tower kind
		// we trust the URL — user is likely re-pointing the remote.
		if kind, err := dolt.ClassifyRemoteURL(remoteURL); err == nil {
			remoteKind = kind
		}
		remoteURL = dolt.NormalizeRemoteURL(remoteURL, remoteKind)

		// Best-effort: create the DoltHub database if it doesn't exist yet.
		// Skip for remotesapi — that endpoint manages its own databases.
		if remoteKind == config.RemoteKindDoltHub {
			if err := dolt.EnsureDoltHubDB(remoteURL); err != nil {
				fmt.Printf("  Note: could not pre-create remote db: %s\n", err)
			}
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
	if err := dolt.CLIPush(context.Background(), dataDir, false); err != nil {
		if strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(err.Error(), "no common ancestor") {
			fmt.Println("  Divergent history — retrying with --force...")
			if err2 := dolt.CLIPush(context.Background(), dataDir, true); err2 != nil {
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
