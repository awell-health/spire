// Package apprentice houses the apprentice delivery core: the bundle-produce
// + signal-write flow invoked by both `spire apprentice submit` and the
// wizard's apprentice-mode exit.
//
// The wizard cannot shell out to `spire apprentice submit` — in cluster mode
// the wizard runs in a different pod from the apprentice, and even locally,
// re-bootstrapping the CLI just to produce a bundle is wasteful. Both call
// sites resolve their dependencies (tower, BundleStore, store helpers) and
// hand them to Submit as Options.
package apprentice

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
)

// SignalPayload is the JSON structure written to the task bead's metadata
// under apprentice_signal_<role>. Mirrors the consumer-side shape in
// pkg/bundlestore.Signal — keep the tags in lockstep.
//
// HandoffMode records which delivery mode the executor selected for this
// apprentice spawn (runtime.HandoffMode — "bundle", "transitional",
// "borrowed", "none"). The apprentice does NOT choose the mode; it emits
// whatever the executor decided so downstream consumers (metrics, the
// spi-xplwy chunk 5b cutover) can reason about delivery without
// re-inferring it. Omitted from the signal when the caller didn't wire it
// (preserves back-compat with older callers that submitted via the CLI
// wrapper before the runtime-contract fields existed).
type SignalPayload struct {
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	BundleKey   string   `json:"bundle_key,omitempty"`
	Commits     []string `json:"commits,omitempty"`
	SubmittedAt string   `json:"submitted_at"`
	HandoffMode string   `json:"handoff_mode,omitempty"`
}

// Options describes a single apprentice submission. Every external dependency
// is injected so Submit has no hidden coupling to process env or globals —
// callers (the CLI wrapper, the wizard exit) wire whatever they need.
type Options struct {
	// BeadID is the task bead the apprentice is delivering work for.
	BeadID string
	// AttemptID is the attempt bead ID — passed to BundleStore.Put so the
	// store can disambiguate retries. When empty, Submit falls back to
	// "<BeadID>-local" for standalone invocations.
	AttemptID string
	// ApprenticeIdx is the fan-out slot. 0 for single-apprentice tasks.
	ApprenticeIdx int
	// BaseBranch is the ref commits are bundled against (base..HEAD). When
	// empty, defaults to "main" — callers that care should set it explicitly
	// from the bead's base-branch: label.
	BaseBranch string
	// WorktreeDir is the directory the apprentice's feat branch is checked
	// out in. When empty, git commands run in the process CWD (legacy CLI
	// behavior). When set, git commands are run with -C WorktreeDir so the
	// caller doesn't need to chdir.
	WorktreeDir string
	// NoChanges short-circuits: writes a no-op signal and skips bundle
	// creation. Mirrors the CLI's --no-changes flag.
	NoChanges bool

	// HandoffMode is the delivery mode the executor selected for this
	// spawn (runtime.HandoffMode value as a string). The apprentice emits
	// whatever the executor chose — it does not select the mode. When
	// empty the signal omits the field, preserving back-compat with older
	// callers. See docs/design/spi-xplwy-runtime-contract.md §1.3.
	HandoffMode string

	// Store is the BundleStore the bundle is uploaded to. Required unless
	// NoChanges is true.
	Store bundlestore.BundleStore

	// GetBead reads bead state (needed to resolve BaseBranch from the
	// base-branch: label when BaseBranch is unset). Optional; when nil,
	// defaults to store.GetBead. Exposed for tests.
	GetBead func(id string) (store.Bead, error)
	// SetMetadata writes the signal value. Optional; defaults to
	// store.SetBeadMetadata.
	SetMetadata func(id, key, value string) error
	// AddComment writes the submission comment. Optional; defaults to
	// store.AddComment.
	AddComment func(id, text string) error
	// Now supplies the timestamp for SubmittedAt. Optional; defaults to
	// time.Now().UTC().
	Now func() time.Time
	// RunGit, when non-nil, is used for all git invocations. The caller is
	// responsible for arranging CWD. When nil, Submit uses exec.Command
	// directly (with -C WorktreeDir when set).
	RunGit func(args ...string) ([]byte, error)
}

// withDefaults fills unset dependency callbacks with their library defaults
// and returns a copy. Callers pass an Options with only the values they care
// about; Submit then resolves the full set locally.
func (o Options) withDefaults() Options {
	if o.GetBead == nil {
		o.GetBead = store.GetBead
	}
	if o.SetMetadata == nil {
		o.SetMetadata = store.SetBeadMetadata
	}
	if o.AddComment == nil {
		o.AddComment = store.AddComment
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}

// runGit dispatches to opts.RunGit when present, otherwise shells out via
// exec.Command, scoping the working directory to opts.WorktreeDir.
func (o Options) runGit(args ...string) ([]byte, error) {
	if o.RunGit != nil {
		return o.RunGit(args...)
	}
	cmd := exec.Command("git", args...)
	if o.WorktreeDir != "" {
		cmd.Dir = o.WorktreeDir
	}
	return cmd.Output()
}

// Submit runs the delivery pipeline: verify the worktree is clean, enumerate
// base..HEAD commits, bundle them, upload to the BundleStore, and write the
// apprentice_signal_<role> metadata + a summary comment. When opts.NoChanges
// is true, only the no-op signal is written and no bundle is produced.
//
// Errors are wrapped with actionable context. This function is the single
// authority on signal-write semantics — do not duplicate its logic in other
// call sites.
func Submit(ctx context.Context, opts Options) error {
	opts = opts.withDefaults()

	if opts.BeadID == "" {
		return fmt.Errorf("apprentice submit: bead id required")
	}

	role := bundlestore.ApprenticeRole(opts.BeadID, opts.ApprenticeIdx)

	// Resolve base branch. Explicit setting wins; otherwise read the
	// base-branch: label off the bead.
	base := opts.BaseBranch
	if base == "" {
		if bead, err := opts.GetBead(opts.BeadID); err == nil {
			for _, l := range bead.Labels {
				if strings.HasPrefix(l, "base-branch:") {
					base = strings.TrimPrefix(l, "base-branch:")
					break
				}
			}
		}
	}
	if base == "" {
		base = "main"
	}

	// Clean-worktree check. Dirty apprentice worktrees indicate incomplete
	// work — refuse to submit rather than silently bundling a stale HEAD.
	statusOut, err := opts.runGit("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if dirty := strings.TrimRight(string(statusOut), "\n"); dirty != "" {
		return fmt.Errorf("refusing to submit: worktree has uncommitted changes:\n%s", dirty)
	}

	// Commit-message verification. Every commit in base..HEAD must carry
	// the task bead ID in its conventional prefix.
	logOut, err := opts.runGit("log", "--format=%H%x09%s", base+"..HEAD")
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
		if got != opts.BeadID {
			offenders = append(offenders, fmt.Sprintf("%s %s", sha, subject))
			continue
		}
		commitShas = append(commitShas, sha)
	}
	if len(offenders) > 0 {
		return fmt.Errorf("refusing to submit: %d commit(s) do not reference %s:\n%s",
			len(offenders), opts.BeadID, strings.Join(offenders, "\n"))
	}

	// Reverse chronological order -> commit order.
	for i, j := 0, len(commitShas)-1; i < j; i, j = i+1, j-1 {
		commitShas[i], commitShas[j] = commitShas[j], commitShas[i]
	}

	if opts.NoChanges {
		return writeNoChangesSignal(opts, role)
	}

	if len(commitShas) == 0 {
		return fmt.Errorf("no commits in %s..HEAD: pass NoChanges=true if this is intentional", base)
	}

	if opts.Store == nil {
		return fmt.Errorf("apprentice submit: bundle store required")
	}

	tmp, err := os.CreateTemp("", "spire-bundle-*.bundle")
	if err != nil {
		return fmt.Errorf("create tmp bundle: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := opts.runGit("bundle", "create", tmpPath, fmt.Sprintf("%s..HEAD", base)); err != nil {
		return fmt.Errorf("git bundle create: %w", err)
	}

	handle, err := putBundle(ctx, opts.Store, opts, tmpPath)
	if err != nil {
		return fmt.Errorf("upload bundle: %w", err)
	}

	payload := SignalPayload{
		Kind:        bundlestore.SignalKindBundle,
		Role:        role,
		BundleKey:   handle.Key,
		Commits:     commitShas,
		SubmittedAt: opts.Now().Format(time.RFC3339),
		HandoffMode: opts.HandoffMode,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal signal payload: %w", err)
	}
	metaKey := bundlestore.SignalMetadataKey(role)
	if err := opts.SetMetadata(opts.BeadID, metaKey, string(raw)); err != nil {
		return fmt.Errorf("write signal metadata: %w", err)
	}

	first, last := commitShas[0], commitShas[len(commitShas)-1]
	summary := fmt.Sprintf("apprentice %s submitted bundle covering %d commit(s) (%s..%s)",
		role, len(commitShas), shortSHA(first), shortSHA(last))
	if err := opts.AddComment(opts.BeadID, summary); err != nil {
		return fmt.Errorf("write submission comment: %w", err)
	}

	return nil
}

// writeNoChangesSignal writes the no-op signal + comment for the NoChanges
// branch. No bundle is uploaded.
func writeNoChangesSignal(opts Options, role string) error {
	payload := SignalPayload{
		Kind:        bundlestore.SignalKindNoOp,
		Role:        role,
		SubmittedAt: opts.Now().Format(time.RFC3339),
		HandoffMode: opts.HandoffMode,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal no-changes payload: %w", err)
	}
	metaKey := bundlestore.SignalMetadataKey(role)
	if err := opts.SetMetadata(opts.BeadID, metaKey, string(raw)); err != nil {
		return fmt.Errorf("write signal metadata: %w", err)
	}
	summary := fmt.Sprintf("apprentice %s submitted no-changes signal", role)
	if err := opts.AddComment(opts.BeadID, summary); err != nil {
		return fmt.Errorf("write submission comment: %w", err)
	}
	return nil
}

// putBundle uploads the bundle with "overwrite latest handle" idempotency: if
// Put reports ErrDuplicate, delete the prior handle and retry exactly once.
// Any non-duplicate error propagates.
func putBundle(ctx context.Context, bstore bundlestore.BundleStore, opts Options, path string) (bundlestore.BundleHandle, error) {
	attemptID := opts.AttemptID
	if attemptID == "" {
		attemptID = opts.BeadID + "-local"
	}
	req := bundlestore.PutRequest{
		BeadID:        opts.BeadID,
		AttemptID:     attemptID,
		ApprenticeIdx: opts.ApprenticeIdx,
	}
	handle, err := putOnce(ctx, bstore, req, path)
	if err == nil {
		return handle, nil
	}
	if !errors.Is(err, bundlestore.ErrDuplicate) {
		return bundlestore.BundleHandle{}, err
	}

	// Idempotent re-submit: clear the prior handle and retry once.
	priorKey := opts.BeadID + "/" + attemptID + "-" + strconv.Itoa(opts.ApprenticeIdx) + ".bundle"
	_ = bstore.Delete(ctx, bundlestore.BundleHandle{BeadID: opts.BeadID, Key: priorKey})
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

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
