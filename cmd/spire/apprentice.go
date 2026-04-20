package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/git"
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

// signalPayload is the JSON structure written to the task bead's metadata
// under apprentice_signal_<role>. Consumers (wizard, reviewers) parse this
// to locate the bundle and confirm the submission kind.
type signalPayload struct {
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	BundleKey   string   `json:"bundle_key,omitempty"`
	Commits     []string `json:"commits,omitempty"`
	SubmittedAt string   `json:"submitted_at"`
}

// cmdApprenticeSubmit is the full control flow for `spire apprentice submit`.
// It is structured so each step can fail with an actionable message —
// the apprentice's pod evaporates after submit, so diagnostics printed here
// are the only trace the archmage will have.
func cmdApprenticeSubmit(beadFlag, sinceFlag string, noChanges bool) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Step 2: resolve bead ID.
	beadID := beadFlag
	if beadID == "" {
		beadID = os.Getenv("SPIRE_BEAD_ID")
	}
	if beadID == "" {
		return fmt.Errorf("no bead ID resolved: pass --bead or run from a wizard-spawned context (SPIRE_BEAD_ID)")
	}

	// Step 3: resolve role.
	idx := os.Getenv("SPIRE_APPRENTICE_IDX")
	if idx == "" {
		idx = "0"
	}
	idxInt, err := strconv.Atoi(idx)
	if err != nil {
		return fmt.Errorf("invalid SPIRE_APPRENTICE_IDX %q: %w", idx, err)
	}
	role := fmt.Sprintf("apprentice-%s-%s", beadID, idx)

	// Step 4: resolve base branch.
	base := "main"
	if bead, err := apprenticeGetBeadFunc(beadID); err == nil {
		for _, l := range bead.Labels {
			if strings.HasPrefix(l, "base-branch:") {
				base = strings.TrimPrefix(l, "base-branch:")
				break
			}
		}
	}
	if sinceFlag != "" {
		base = sinceFlag
	}

	// Step 5: clean-worktree check. The apprentice's emptyDir vanishes
	// after submit, so the error has to enumerate every dirty path or
	// the archmage has no way to tell what was left behind.
	statusOut, err := apprenticeRunGit("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if dirty := strings.TrimRight(string(statusOut), "\n"); dirty != "" {
		return fmt.Errorf("refusing to submit: worktree has uncommitted changes:\n%s", dirty)
	}

	// Step 6: commit-message verification. Every commit in base..HEAD must
	// carry the task bead ID in its conventional prefix.
	logOut, err := apprenticeRunGit("log", "--format=%H%x09%s", base+"..HEAD")
	if err != nil {
		return fmt.Errorf("git log %s..HEAD: %w", base, err)
	}
	var commitShas []string
	var offenders []string
	for _, line := range strings.Split(strings.TrimRight(string(logOut), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		sha, subject := parts[0], parts[1]
		got := git.BeadIDFromSubject(subject)
		if got != beadID {
			offenders = append(offenders, fmt.Sprintf("%s %s", sha, subject))
			continue
		}
		commitShas = append(commitShas, sha)
	}
	if len(offenders) > 0 {
		return fmt.Errorf("refusing to submit: %d commit(s) do not reference %s:\n%s",
			len(offenders), beadID, strings.Join(offenders, "\n"))
	}

	// Log output is in reverse-chronological order by default; flip it so
	// the commits slice reads in commit order.
	for i, j := 0, len(commitShas)-1; i < j; i, j = i+1, j-1 {
		commitShas[i], commitShas[j] = commitShas[j], commitShas[i]
	}

	ctx := context.Background()

	// Step 7: --no-changes short-circuit.
	if noChanges {
		return writeNoChangesSignal(beadID, role)
	}

	// Step 8: empty-range guard. If there are genuinely no commits to
	// bundle, the apprentice must say so explicitly — a silent no-op is
	// almost always a mistake.
	if len(commitShas) == 0 {
		return fmt.Errorf("no commits in %s..HEAD: pass --no-changes if this is intentional", base)
	}

	// Step 9: bundle + put.
	bstore, err := apprenticeNewBundleStoreFunc()
	if err != nil {
		return fmt.Errorf("open bundle store: %w", err)
	}

	tmp, err := os.CreateTemp("", "spire-bundle-*.bundle")
	if err != nil {
		return fmt.Errorf("create tmp bundle: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := apprenticeRunGit("bundle", "create", tmpPath, fmt.Sprintf("%s..HEAD", base)); err != nil {
		return fmt.Errorf("git bundle create: %w", err)
	}

	handle, err := putBundle(ctx, bstore, beadID, idxInt, tmpPath)
	if err != nil {
		return fmt.Errorf("upload bundle: %w", err)
	}

	// Step 10: write signal + comment. Put has already returned — if the
	// signal write fails the bundle is leaked (janitor collects). That
	// ordering is load-bearing: see task description.
	payload := signalPayload{
		Kind:        "bundle",
		Role:        role,
		BundleKey:   handle.Key,
		Commits:     commitShas,
		SubmittedAt: apprenticeNowFunc().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal signal payload: %w", err)
	}
	metaKey := "apprentice_signal_" + role
	if err := apprenticeSetBeadMetadataFunc(beadID, metaKey, string(raw)); err != nil {
		return fmt.Errorf("write signal metadata: %w", err)
	}

	first, last := commitShas[0], commitShas[len(commitShas)-1]
	summary := fmt.Sprintf("apprentice %s submitted bundle covering %d commit(s) (%s..%s)",
		role, len(commitShas), shortSHA(first), shortSHA(last))
	if err := apprenticeAddCommentFunc(beadID, summary); err != nil {
		return fmt.Errorf("write submission comment: %w", err)
	}

	fmt.Printf("submitted bundle %s (%d commits)\n", handle.Key, len(commitShas))
	return nil
}

// putBundle wraps the BundleStore Put with the "overwrite latest handle"
// idempotency choice: if Put reports a duplicate, delete the prior handle
// and retry exactly once. Any non-duplicate error propagates.
func putBundle(ctx context.Context, bstore bundlestore.BundleStore, beadID string, idx int, path string) (bundlestore.BundleHandle, error) {
	attemptID := os.Getenv("SPIRE_ATTEMPT_ID")
	if attemptID == "" {
		// BundleStore.Put validates attempt ID against its idPattern, so
		// fall back to a deterministic sentinel when the wizard didn't
		// inject one (local dev, tests, standalone invocations).
		attemptID = beadID + "-local"
	}
	req := bundlestore.PutRequest{
		BeadID:        beadID,
		AttemptID:     attemptID,
		ApprenticeIdx: idx,
	}
	handle, err := putOnce(ctx, bstore, req, path)
	if err == nil {
		return handle, nil
	}
	if !errors.Is(err, bundlestore.ErrDuplicate) {
		return bundlestore.BundleHandle{}, err
	}

	// Idempotent re-submit: clear the prior handle and retry once.
	priorKey := beadID + "/" + attemptID + "-" + strconv.Itoa(idx) + ".bundle"
	_ = bstore.Delete(ctx, bundlestore.BundleHandle{BeadID: beadID, Key: priorKey})
	return putOnce(ctx, bstore, req, path)
}

func putOnce(ctx context.Context, bstore bundlestore.BundleStore, req bundlestore.PutRequest, path string) (bundlestore.BundleHandle, error) {
	f, err := os.Open(path)
	if err != nil {
		return bundlestore.BundleHandle{}, err
	}
	defer f.Close()
	return bstore.Put(ctx, req, f)
}

// writeNoChangesSignal writes the no-op signal + comment for the --no-changes
// branch. No bundle is uploaded.
func writeNoChangesSignal(beadID, role string) error {
	payload := signalPayload{
		Kind:        "no-op",
		Role:        role,
		SubmittedAt: apprenticeNowFunc().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal no-changes payload: %w", err)
	}
	metaKey := "apprentice_signal_" + role
	if err := apprenticeSetBeadMetadataFunc(beadID, metaKey, string(raw)); err != nil {
		return fmt.Errorf("write signal metadata: %w", err)
	}
	summary := fmt.Sprintf("apprentice %s submitted no-changes signal", role)
	if err := apprenticeAddCommentFunc(beadID, summary); err != nil {
		return fmt.Errorf("write submission comment: %w", err)
	}
	fmt.Printf("submitted no-changes signal for %s\n", beadID)
	return nil
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
