package executor

// apprentice_bundle.go — consumer side of the apprentice submit/fetch flow.
//
// The apprentice (cmd/spire/apprentice.go) writes a git bundle to the
// BundleStore and stamps a JSON signal on the task bead under the
// apprentice_signal_<role> metadata key. The wizard side reads that signal,
// streams the bundle to a temp file, and fetches it into staging as a local
// branch. Merge integration stays in StagingWorktree.MergeBranch — this
// helper only materializes the branch.
//
// All four dispatch sites (direct, wave, sequential, injected) call
// applyApprenticeBundle after a successful spawn and before MergeBranch when
// e.deps.BundleStore is wired. A no-op signal tells the caller "nothing to
// merge" and the merge is skipped entirely. The bundle handle is returned to
// the caller so it can call deleteApprenticeBundle only after a successful
// merge — deleting earlier would leave the wizard with no way to retry on
// merge failure.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/bundlestore"
	spgit "github.com/awell-health/spire/pkg/git"
)

// bundleOutcome tells the caller what happened after
// applyApprenticeBundle ran. Shaped to keep the merge-vs-skip decision
// explicit at each dispatch site rather than buried in a boolean.
type bundleOutcome struct {
	// Applied is true when the apprentice's bundle was fetched and a
	// local branch ref was force-updated to its HEAD. The caller should
	// then call stagingWt.MergeBranch(Branch, resolver) and, on success,
	// deleteApprenticeBundle(Handle).
	Applied bool
	// NoOp is true when the apprentice explicitly signalled "no changes".
	// The caller must skip merge for this bead. Handle is zero in this case.
	NoOp bool
	// Branch is the local branch ref the bundle was applied to. Empty
	// when Applied is false.
	Branch string
	// Handle is the BundleStore handle that was applied. Returned so the
	// caller can call deleteApprenticeBundle ONLY after a successful merge.
	// Deleting before merge would leave the wizard with no way to retry on
	// conflict. Zero when Applied is false.
	Handle bundlestore.BundleHandle
}

// applyApprenticeBundle reads the apprentice's signal for (beadID, idx),
// streams the bundle out of the BundleStore, and applies it as a local
// branch in the staging worktree. On success the caller merges Branch into
// staging via the existing MergeBranch helper, then calls
// deleteApprenticeBundle(Handle) to clean up.
//
// When no signal is present the function returns a zero outcome with no
// error — callers must treat that as "apprentice used push transport" and
// fall back to fetching feat/<beadID> from the remote before merging. The
// bundle and push transports share this consumer path; only the signal's
// presence distinguishes them.
func (e *Executor) applyApprenticeBundle(beadID string, idx int, stagingWt *spgit.StagingWorktree) (bundleOutcome, error) {
	if e.deps.BundleStore == nil {
		return bundleOutcome{}, errors.New("no BundleStore configured")
	}
	if stagingWt == nil {
		return bundleOutcome{}, errors.New("no staging worktree available")
	}

	bead, err := e.deps.GetBead(beadID)
	if err != nil {
		return bundleOutcome{}, fmt.Errorf("get bead %s: %w", beadID, err)
	}

	role := bundlestore.ApprenticeRole(beadID, idx)
	sig, ok, err := bundlestore.SignalForRole(bead.Metadata, role)
	if err != nil {
		return bundleOutcome{}, fmt.Errorf("parse apprentice signal %s: %w", role, err)
	}
	if !ok {
		// No signal — apprentice used push transport (or ran before bundle
		// support existed). Return a zero outcome so the caller falls through
		// to the fetch+merge legacy path.
		e.log("no apprentice signal for %s on %s — falling back to feat-branch merge", role, beadID)
		return bundleOutcome{}, nil
	}

	if sig.Kind == bundlestore.SignalKindNoOp {
		e.log("apprentice %s signalled no-op — skipping merge", role)
		return bundleOutcome{NoOp: true}, nil
	}
	if sig.Kind != bundlestore.SignalKindBundle {
		return bundleOutcome{}, fmt.Errorf("unexpected signal kind %q for %s", sig.Kind, role)
	}
	if sig.BundleKey == "" {
		return bundleOutcome{}, fmt.Errorf("bundle signal for %s has empty bundle key", role)
	}

	handle := bundlestore.HandleForSignal(beadID, sig)
	tmpPath, err := e.streamBundleToTmp(handle, stagingWt.Dir)
	if err != nil {
		return bundleOutcome{}, err
	}
	defer os.Remove(tmpPath)

	branch := e.resolveBranch(beadID)
	if err := stagingWt.ApplyBundle(tmpPath, branch); err != nil {
		return bundleOutcome{}, fmt.Errorf("apply bundle for %s: %w", beadID, err)
	}

	e.log("applied apprentice bundle for %s (%d commits) -> %s", beadID, len(sig.Commits), branch)
	return bundleOutcome{Applied: true, Branch: branch, Handle: handle}, nil
}

// deleteApprenticeBundle removes a bundle from the BundleStore. Callers
// invoke it AFTER a successful merge (or an explicit discard) — never
// before, because a merge failure that has already lost its bundle cannot
// be retried. Errors are logged and swallowed; the bundle janitor is the
// correctness net.
func (e *Executor) deleteApprenticeBundle(beadID string, h bundlestore.BundleHandle) {
	if e.deps.BundleStore == nil || h.Key == "" {
		return
	}
	if err := e.deps.BundleStore.Delete(context.Background(), h); err != nil {
		e.log("warning: delete bundle %s for %s: %s — janitor will collect", h.Key, beadID, err)
	}
}

// streamBundleToTmp copies the bundle stream out of the BundleStore into a
// temp file under stagingDir/.git/tmp-bundles/. Placing it inside the
// worktree's .git dir keeps the file on the same filesystem as the repo
// (no cross-device errors during fetch) and makes the path trivially
// discoverable during incident diagnosis.
func (e *Executor) streamBundleToTmp(h bundlestore.BundleHandle, stagingDir string) (string, error) {
	rc, err := e.deps.BundleStore.Get(context.Background(), h)
	if err != nil {
		return "", fmt.Errorf("get bundle %s: %w", h.Key, err)
	}
	defer rc.Close()

	tmpDir := filepath.Join(stagingDir, ".git", "tmp-bundles")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir tmp-bundles: %w", err)
	}

	f, err := os.CreateTemp(tmpDir, "apprentice-*.bundle")
	if err != nil {
		return "", fmt.Errorf("create tmp bundle: %w", err)
	}
	path := f.Name()
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("stream bundle to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close tmp bundle %s: %w", path, err)
	}
	return path, nil
}
