package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// cmdDebugRecoveryDispatchImpl is the `spire debug recovery dispatch`
// implementation: parse flags, resolve beads dir, guard the tower,
// load the recovery bead, and run cleric-default synchronously in
// foreground mode. Events stream to stdout as single-line status
// records; the final OUTCOME line summarizes decision/repair_mode/
// class. Exit codes: 0 resume, 2 escalate, 1 infra error.
func cmdDebugRecoveryDispatchImpl(args []string) error {
	fs := flag.NewFlagSet("debug recovery dispatch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	beadID := fs.String("bead", "", "recovery bead ID to dispatch (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*beadID) == "" {
		return fmt.Errorf("--bead is required")
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDebugTower(); err != nil {
		return err
	}

	// Build executor deps scoped to the recovery bead's repo so the
	// cleric reads the same spire.yaml the production steward path
	// would. Backend resolution follows the same cwd/repoPath
	// fallback as every other dispatch site.
	spawner := resolveBackendForBead(*beadID)
	deps := buildExecutorDepsForBead(*beadID, spawner)

	runner := func(ctx context.Context, bead *store.Bead, events chan<- recovery.PhaseEvent) (recovery.RecoveryOutcome, error) {
		return executor.RunClericForeground(ctx, bead, deps, events)
	}

	ctx := context.Background()
	outcome, err := recovery.DispatchForeground(ctx, *beadID, os.Stdout, runner)
	if err != nil {
		return fmt.Errorf("dispatch: %w", err)
	}

	fmt.Fprintf(os.Stdout, "\nOUTCOME decision=%s repair_mode=%s class=%s action=%s\n",
		outcome.Decision, outcome.RepairMode, outcome.FailureClass, outcome.RepairAction)

	if outcome.Decision == recovery.DecisionEscalate {
		// Signal escalation to shell callers without treating it as a
		// tool failure. The outcome is durably persisted; the
		// recovery bead is closed with Decision=DecisionEscalate.
		os.Exit(2)
	}
	return nil
}
