package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// reconcileLogsCmd is the operator-facing entry point that brings the
// on-disk wizard log directory into the agent_log_artifacts manifest.
// It exists because pkg/wizard and pkg/agent currently write `.log` /
// `.jsonl` transcripts directly to disk without registering them
// through logartifact.Put — the gateway's bead-logs API consequently
// reports an empty list for every bead until something teaches the
// manifest about those files. See bead spi-tsodj3.
//
// Idempotent: re-running the command does not duplicate manifest rows.
// The unique key on (bead_id, attempt_id, run_id, agent_name, role,
// phase, provider, stream, sequence) is the guard.
var reconcileLogsCmd = &cobra.Command{
	Use:   "reconcile-logs [bead-id]",
	Short: "Register existing wizard/apprentice transcripts in the bead-logs manifest",
	Long: `reconcile-logs walks the local wizard log directory and inserts an
agent_log_artifacts manifest row for every transcript file that does not
have one yet. After it completes, the gateway's
GET /api/v1/beads/<id>/logs endpoint surfaces the on-disk logs that were
previously invisible because the write path did not register them.

Pass an optional bead ID to scope the walk; with no argument every bead
under the wizards directory is reconciled. Re-running the command is
safe — manifest rows are upserted on the unique identity key.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var beadID string
		if len(args) == 1 {
			beadID = args[0]
		}
		return runReconcileLogs(cmd.Context(), beadID, cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(reconcileLogsCmd)
}

// runReconcileLogs is the testable core of the reconcile-logs verb. It
// resolves the active tower + dolt store, constructs a LocalStore over
// the wizards directory, and invokes Reconcile. The (added, existing)
// summary printed at the end is what an operator sees on the terminal.
func runReconcileLogs(ctx context.Context, beadID string, out interface{ Write(p []byte) (int, error) }) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tower, err := activeTowerConfig()
	if err != nil {
		return fmt.Errorf("reconcile-logs: resolve active tower: %w", err)
	}
	if tower == nil || tower.Name == "" {
		return fmt.Errorf("reconcile-logs: no active tower configured")
	}

	if _, err := ensureStore(); err != nil {
		return fmt.Errorf("reconcile-logs: ensure store: %w", err)
	}
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		return fmt.Errorf("reconcile-logs: active dolt DB unavailable")
	}

	root := filepath.Join(dolt.GlobalDir(), "wizards")
	localStore, err := logartifact.NewLocal(root, db)
	if err != nil {
		return fmt.Errorf("reconcile-logs: build local store: %w", err)
	}

	manifests, err := localStore.Reconcile(ctx, tower.Name, beadID)
	if err != nil {
		return fmt.Errorf("reconcile-logs: %w", err)
	}

	added := 0
	existing := 0
	beadsTouched := map[string]struct{}{}
	for _, m := range manifests {
		beadsTouched[m.Identity.BeadID] = struct{}{}
		// Manifests created by this run have CreatedAt == UpdatedAt;
		// pre-existing rows have an older CreatedAt. Compare with a
		// small tolerance so clock skew between Dolt and the local
		// machine doesn't misreport a fresh row as existing.
		if m.UpdatedAt.Sub(m.CreatedAt).Abs().Seconds() < 1 {
			added++
		} else {
			existing++
		}
	}

	scope := "all beads"
	if beadID != "" {
		scope = beadID
	}
	fmt.Fprintf(out, "reconcile-logs: scope=%s tower=%s root=%s\n", scope, tower.Name, root)
	fmt.Fprintf(out, "  beads touched: %d\n", len(beadsTouched))
	fmt.Fprintf(out, "  manifest rows added: %d\n", added)
	fmt.Fprintf(out, "  already-registered rows skipped: %d\n", existing)
	return nil
}
