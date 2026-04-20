package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/config"
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
  Credentials are read from spire's credential store or from DOLT_REMOTE_USER /
  DOLT_REMOTE_PASSWORD env vars. The source depends on the active tower's
  remote_kind:
    dolthub    → spire config set dolthub-user / dolthub-password
    remotesapi → stored per-tower by 'spire tower attach' (--user / --password-stdin)

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

// pullDeps is the cmd/spire/pull-local seam over pkg/dolt's pull pipeline.
// It exists so runPullCore can be unit-tested without a live dolt server.
type pullDeps interface {
	GetCurrentCommitHash(dbName string) string
	CLIPull(ctx context.Context, dataDir string, force bool) error
	ApplyMergeOwnership(dbName, preCommit string) error
	HasUnresolvedConflicts(dbName string) (int, error)
}

type realPullDeps struct{}

func (realPullDeps) GetCurrentCommitHash(dbName string) string {
	return dolt.GetCurrentCommitHash(dbName)
}

func (realPullDeps) CLIPull(ctx context.Context, dataDir string, force bool) error {
	return dolt.CLIPull(ctx, dataDir, force)
}

func (realPullDeps) ApplyMergeOwnership(dbName, preCommit string) error {
	return dolt.ApplyMergeOwnership(dbName, preCommit)
}

func (realPullDeps) HasUnresolvedConflicts(dbName string) (int, error) {
	return dolt.HasUnresolvedConflicts(dbName)
}

var defaultPullDeps pullDeps = realPullDeps{}

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

	// ── Inject remote credentials (DoltHub or cluster remotesapi) ────────────
	// Resolve the active tower so remotesapi-attached towers pull with their
	// per-tower MySQL-style creds instead of the shared DoltHub JWK creds.
	tower, _ := activeTowerConfig()
	user, pass := config.RemoteCredentials(tower)
	if user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
	}
	if pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
	}

	dbName := readBeadsDBName()
	if err := runPullCore(defaultPullDeps, dataDir, dbName, force); err != nil {
		return err
	}

	fmt.Println("  Pull complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}

// runPullCore drives the dolt pull → ownership-enforce → conflict-check
// pipeline through the pullDeps seam. Pre-pipeline setup (remote config,
// credentials, data-dir resolution) and the post-pipeline UX echo stay in
// runPull.
func runPullCore(deps pullDeps, dataDir, dbName string, force bool) error {
	// ── Record pre-pull commit for ownership enforcement ─────────────────────
	preCommit := ""
	if dbName != "" {
		preCommit = deps.GetCurrentCommitHash(dbName)
	}

	// ── Pull via dolt CLI ─────────────────────────────────────────────────────
	fmt.Println("  Pulling from origin...")
	pullErr := deps.CLIPull(context.Background(), dataDir, force)

	// ── Enforce field-level ownership ─────────────────────────────────────────
	// Must run even when pull reports conflicts, since CLIPull merges data
	// into the working set before returning the conflict error.
	var ownerErr error
	if dbName != "" && preCommit != "" {
		ownerErr = deps.ApplyMergeOwnership(dbName, preCommit)
	}
	remainingConflicts := 0
	if dbName != "" {
		remaining, conflictErr := deps.HasUnresolvedConflicts(dbName)
		if conflictErr != nil {
			if ownerErr != nil {
				return fmt.Errorf("ownership enforcement failed and conflict state unknown: %w", ownerErr)
			}
			return fmt.Errorf("check unresolved conflicts: %w", conflictErr)
		}
		remainingConflicts = remaining
		if remainingConflicts > 0 {
			if ownerErr != nil {
				return fmt.Errorf("merge conflicts remain (%d unresolved): %w", remainingConflicts, ownerErr)
			}
			return fmt.Errorf("merge conflicts remain (%d unresolved)", remainingConflicts)
		}
	}
	if ownerErr != nil {
		fmt.Printf("  Warning: ownership enforcement: %s\n", ownerErr)
	}

	// ── Classify pull errors after ownership enforcement ──────────────────────
	if pullErr != nil {
		hard, merge := classifyPullError(pullErr.Error())
		if hard && !force {
			fmt.Println("  Pull failed — histories have diverged.")
			fmt.Println()
			fmt.Println("  To attempt a three-way merge (preserves both sides), run:")
			fmt.Println("    spire sync --merge")
			fmt.Println()
			fmt.Println("  To overwrite local history with the remote (destructive), run:")
			fmt.Println("    spire pull --force")
			return fmt.Errorf("pull failed (diverged histories)")
		}
		if merge {
			fmt.Println("  Merge conflicts resolved via field-level ownership.")
		} else {
			// hard+force (force pull still failed) or unknown error — propagate.
			return fmt.Errorf("dolt pull: %w", pullErr)
		}
	}

	return nil
}

// classifyPullError categorises a dolt pull error message.
// Returns (hard, merge) where hard means a diverged-history rejection
// (no data merged) and merge means a merge conflict (data merged into
// working set, resolvable by ownership).
func classifyPullError(errMsg string) (hard, merge bool) {
	if strings.Contains(errMsg, "non-fast-forward") ||
		strings.Contains(errMsg, "diverged") {
		return true, false
	}
	if strings.Contains(errMsg, "conflict") || strings.Contains(errMsg, "CONFLICT") ||
		strings.Contains(errMsg, "cannot merge") {
		return false, true
	}
	return false, false
}
