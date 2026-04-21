package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/awell-health/spire/pkg/apprentice"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

var apprenticeCmd = &cobra.Command{
	Use:   "apprentice",
	Short: "Apprentice-side commands (submit, ...)",
	Long: `Apprentice-side commands.

An apprentice is the agent dispatched by a wizard to implement a single task
bead in an isolated worktree. "spire apprentice submit" is the delivery
command an apprentice runs after its final commit — it bundles the branch,
hands the bundle to the configured BundleStore, and writes a signal on the
task bead so the wizard can pick the work up.`,
}

var apprenticeSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Deliver apprentice work to the wizard via git bundle",
	Long: `Deliver the apprentice's work to the wizard.

Bundles every commit between the base branch and HEAD into a git bundle,
uploads the bundle to the tower's BundleStore, and writes a signal on the
task bead so the wizard knows the bundle is ready to consume. The command
never pushes to a git remote — the bundle IS the delivery.

Requires SPIRE_BEAD_ID (or --bead) to identify the task bead. The apprentice
identity (attempt id + fan-out index) is read from SPIRE_ATTEMPT_ID and
SPIRE_APPRENTICE_IDX, which the wizard injects at spawn time.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadFlag, _ := cmd.Flags().GetString("bead")
		sinceFlag, _ := cmd.Flags().GetString("since")
		noChanges, _ := cmd.Flags().GetBool("no-changes")
		return cmdApprenticeSubmit(beadFlag, sinceFlag, noChanges)
	},
}

// apprenticeRunCmd is the internal spawn-only entry point used by the
// executor/spawner to launch an apprentice subprocess. It is hidden from
// the user-facing catalog — operators invoke apprentices through
// `spire summon`, not directly. DisableFlagParsing is on because
// CmdWizardRun owns its own flag grammar (--name, --review-fix,
// --worktree-dir, --start-ref, --custom-prompt-file).
var apprenticeRunCmd = &cobra.Command{
	Use:                "run <bead-id>",
	Short:              "Internal: run apprentice implementation subprocess",
	Hidden:             true,
	DisableFlagParsing: true,
	Args:               cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdWizardRun(args)
	},
}

func init() {
	apprenticeSubmitCmd.Flags().String("bead", "", "Task bead ID (overrides SPIRE_BEAD_ID)")
	apprenticeSubmitCmd.Flags().String("since", "", "Base ref for the bundle (overrides the bead's base-branch: label)")
	apprenticeSubmitCmd.Flags().Bool("no-changes", false, "Signal the bead with a no-op payload; skips bundle creation")
	apprenticeCmd.AddCommand(apprenticeSubmitCmd)
	apprenticeCmd.AddCommand(apprenticeRunCmd)
}

// --- Test-replaceable seams ---

// apprenticeGetBeadFunc lets tests stub the store lookup.
var apprenticeGetBeadFunc = storeGetBead

// apprenticeSetBeadMetadataFunc lets tests stub metadata writes.
var apprenticeSetBeadMetadataFunc = store.SetBeadMetadata

// apprenticeAddCommentFunc lets tests stub comment writes.
var apprenticeAddCommentFunc = storeAddComment

// apprenticeNewBundleStoreFunc lets tests substitute a BundleStore backed by
// a temp dir without touching the user's real data dir.
var apprenticeNewBundleStoreFunc = defaultNewBundleStore

// apprenticeNowFunc lets tests freeze or advance the clock.
var apprenticeNowFunc = func() time.Time { return time.Now().UTC() }

// apprenticeRunGit lets tests replace git invocations with fakes. The real
// implementation just shells out through os/exec.
var apprenticeRunGit = func(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	return cmd.Output()
}

// defaultNewBundleStore reads the active tower config (if any) and builds a
// LocalStore rooted at the configured path — or at XDG_DATA_HOME when no
// tower is resolvable. Any failure to resolve the tower falls through to
// the zero-value config, which WithDefaults() turns into the platform default.
func defaultNewBundleStore() (bundlestore.BundleStore, error) {
	cfg := bundlestore.Config{}
	if tc, err := activeTowerConfig(); err == nil && tc != nil {
		cfg = bundlestore.Config{
			Backend:   tc.BundleStore.Backend,
			LocalRoot: tc.BundleStore.LocalRoot,
			MaxBytes:  tc.BundleStore.MaxBytes,
		}
		if d, perr := time.ParseDuration(tc.BundleStore.JanitorInterval); perr == nil {
			cfg.JanitorInterval = d
		}
	}
	return bundlestore.NewLocalStore(cfg.WithDefaults())
}

// signalPayload is retained as a type alias for tests that unmarshal the
// signal metadata and assert on its shape. The authoritative definition
// lives in pkg/apprentice — keep the tags there in lockstep.
type signalPayload = apprentice.SignalPayload

// cmdApprenticeSubmit is the thin CLI wrapper around pkg/apprentice.Submit.
// It resolves process-level inputs (env vars, bundle store, beads dir) and
// forwards to the pkg function. The wizard's apprentice-mode exit calls
// pkg/apprentice.Submit directly with its own wiring — never this command.
func cmdApprenticeSubmit(beadFlag, sinceFlag string, noChanges bool) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	beadID := beadFlag
	if beadID == "" {
		beadID = os.Getenv("SPIRE_BEAD_ID")
	}
	if beadID == "" {
		return fmt.Errorf("no bead ID resolved: pass --bead or run from a wizard-spawned context (SPIRE_BEAD_ID)")
	}

	idx := os.Getenv("SPIRE_APPRENTICE_IDX")
	if idx == "" {
		idx = "0"
	}
	idxInt, err := strconv.Atoi(idx)
	if err != nil {
		return fmt.Errorf("invalid SPIRE_APPRENTICE_IDX %q: %w", idx, err)
	}

	bstore, err := apprenticeNewBundleStoreFunc()
	if err != nil {
		return fmt.Errorf("open bundle store: %w", err)
	}

	opts := apprentice.Options{
		BeadID:        beadID,
		AttemptID:     os.Getenv("SPIRE_ATTEMPT_ID"),
		ApprenticeIdx: idxInt,
		BaseBranch:    sinceFlag,
		NoChanges:     noChanges,
		Store:         bstore,
		// Test seams: let the existing cmd/spire-level stubs keep working
		// without the pkg-level function reaching into the real dolt store
		// or the real git binary during tests.
		GetBead:     apprenticeGetBeadFunc,
		SetMetadata: apprenticeSetBeadMetadataFunc,
		AddComment:  apprenticeAddCommentFunc,
		Now:         apprenticeNowFunc,
		RunGit:      apprenticeRunGit,
	}

	if err := apprentice.Submit(context.Background(), opts); err != nil {
		return err
	}

	if noChanges {
		fmt.Printf("submitted no-changes signal for %s\n", beadID)
	} else {
		fmt.Printf("submitted bundle for %s (idx %d)\n", beadID, idxInt)
	}
	return nil
}
