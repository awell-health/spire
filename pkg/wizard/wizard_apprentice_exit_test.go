package wizard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/apprentice"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/config"
	spgit "github.com/awell-health/spire/pkg/git"
)

// setupApprenticeExitRepo creates a repo with an initial commit on main,
// checks out a feature branch, and adds one bead-prefixed commit so the
// push transport branch has something to deliver. Reuses the repo dir as
// the worktree dir (pkg/git.Push just runs `git -C wc.Dir push`).
func setupApprenticeExitRepo(t *testing.T, beadID string) *spgit.WorktreeContext {
	t.Helper()
	dir := t.TempDir()
	branch := "feat/" + beadID
	gitRun(t, dir, "init", "--initial-branch=main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test User")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "chore: initial")
	gitRun(t, dir, "checkout", "-b", branch)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-m", "feat("+beadID+"): add a")
	return &spgit.WorktreeContext{
		Dir:        dir,
		Branch:     branch,
		BaseBranch: "main",
		RepoPath:   dir,
	}
}

// swapSubmitFunc replaces submitApprenticeBundleFunc for the test and
// restores the original on cleanup. Returns a pointer to the captured
// Options so tests can assert on what the seam received.
func swapSubmitFunc(t *testing.T, result error) *apprentice.Options {
	t.Helper()
	captured := &apprentice.Options{}
	orig := submitApprenticeBundleFunc
	submitApprenticeBundleFunc = func(_ context.Context, opts apprentice.Options) error {
		*captured = opts
		return result
	}
	t.Cleanup(func() { submitApprenticeBundleFunc = orig })
	return captured
}

// TestDeliverApprenticeWork_BundleTransport routes delivery through the
// bundle branch: Deps wires a real LocalStore, the seam captures the
// Options the wizard constructed, and we assert on the wiring.
func TestDeliverApprenticeWork_BundleTransport(t *testing.T) {
	beadID := "spi-bundle"
	wc := setupApprenticeExitRepo(t, beadID)

	storeDir := t.TempDir()
	ls, err := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: storeDir})
	if err != nil {
		t.Fatalf("bundlestore: %v", err)
	}

	captured := swapSubmitFunc(t, nil)

	tower := &TowerConfig{
		Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle},
	}
	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return tower, nil },
		NewBundleStore:    func() (bundlestore.BundleStore, error) { return ls, nil },
	}
	if err := deliverApprenticeWork(wc, beadID, 2, "att-xyz", deps, noopLog); err != nil {
		t.Fatalf("deliverApprenticeWork: %v", err)
	}

	if captured.BeadID != beadID {
		t.Errorf("Options.BeadID = %q, want %q", captured.BeadID, beadID)
	}
	if captured.AttemptID != "att-xyz" {
		t.Errorf("Options.AttemptID = %q, want %q", captured.AttemptID, "att-xyz")
	}
	if captured.ApprenticeIdx != 2 {
		t.Errorf("Options.ApprenticeIdx = %d, want 2", captured.ApprenticeIdx)
	}
	if captured.BaseBranch != "main" {
		t.Errorf("Options.BaseBranch = %q, want main (from wc.BaseBranch)", captured.BaseBranch)
	}
	if captured.WorktreeDir != wc.Dir {
		t.Errorf("Options.WorktreeDir = %q, want %q", captured.WorktreeDir, wc.Dir)
	}
	if captured.Store != bstore(ls) {
		t.Errorf("Options.Store is not the LocalStore Deps returned")
	}
}

// bstore returns the BundleStore interface wrapping the concrete store.
// Used only so the test assertion compares interface values (same pointer).
func bstore(ls bundlestore.BundleStore) bundlestore.BundleStore { return ls }

// TestDeliverApprenticeWork_BundleSurfacesSubmitErr surfaces errors from
// apprentice.Submit rather than swallowing them.
func TestDeliverApprenticeWork_BundleSurfacesSubmitErr(t *testing.T) {
	beadID := "spi-err"
	wc := setupApprenticeExitRepo(t, beadID)
	ls, _ := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: t.TempDir()})
	want := errors.New("submit exploded")
	swapSubmitFunc(t, want)

	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle}}, nil
		},
		NewBundleStore: func() (bundlestore.BundleStore, error) { return ls, nil },
	}
	err := deliverApprenticeWork(wc, beadID, 0, "", deps, noopLog)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapping %v", err, want)
	}
}

// TestDeliverApprenticeWork_BundleStoreOpenErr propagates BundleStore
// construction failures.
func TestDeliverApprenticeWork_BundleStoreOpenErr(t *testing.T) {
	wc := setupApprenticeExitRepo(t, "spi-open")
	boom := errors.New("cannot open store")
	// Submit seam still needs to be replaced to prevent accidental real calls.
	swapSubmitFunc(t, nil)

	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle}}, nil
		},
		NewBundleStore: func() (bundlestore.BundleStore, error) { return nil, boom },
	}
	err := deliverApprenticeWork(wc, "spi-open", 0, "", deps, noopLog)
	if err == nil || !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("err = %v, want wrapping %q", err, boom.Error())
	}
}

// TestDeliverApprenticeWork_BundleMissingStoreFactory errors when the tower
// requests bundle transport but Deps.NewBundleStore is nil.
func TestDeliverApprenticeWork_BundleMissingStoreFactory(t *testing.T) {
	wc := setupApprenticeExitRepo(t, "spi-missing")
	// Replace Submit so we fail loud if we accidentally fall through.
	swapSubmitFunc(t, errors.New("must not be called"))

	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return &TowerConfig{Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportBundle}}, nil
		},
		// NewBundleStore deliberately nil.
	}
	err := deliverApprenticeWork(wc, "spi-missing", 0, "", deps, noopLog)
	if err == nil {
		t.Fatal("expected error when NewBundleStore is nil")
	}
	if !strings.Contains(err.Error(), "BundleStore") {
		t.Errorf("error should mention BundleStore: %s", err.Error())
	}
}

// TestDeliverApprenticeWork_PushTransport exercises the push branch with a
// real bare repo as the remote. Verifies: the feat branch arrives on the
// remote, no BundleStore is touched, and the Submit seam is never called.
func TestDeliverApprenticeWork_PushTransport(t *testing.T) {
	beadID := "spi-push"
	wc := setupApprenticeExitRepo(t, beadID)

	bare := t.TempDir()
	gitRun(t, bare, "init", "--bare", "--initial-branch=main")
	gitRun(t, wc.Dir, "remote", "add", "origin", bare)

	// Any call into Submit would be a bug — push branch must not touch it.
	submitCalled := false
	orig := submitApprenticeBundleFunc
	submitApprenticeBundleFunc = func(_ context.Context, _ apprentice.Options) error {
		submitCalled = true
		return nil
	}
	t.Cleanup(func() { submitApprenticeBundleFunc = orig })

	tower := &TowerConfig{
		Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportPush},
	}
	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return tower, nil },
		// NewBundleStore intentionally unset — push path must not dereference it.
	}
	if err := deliverApprenticeWork(wc, beadID, 0, "att-xyz", deps, noopLog); err != nil {
		t.Fatalf("deliverApprenticeWork: %v", err)
	}
	if submitCalled {
		t.Fatal("push transport must not invoke apprentice.Submit")
	}

	branches := gitRun(t, bare, "branch", "--list", wc.Branch)
	if !strings.Contains(branches, wc.Branch) {
		t.Fatalf("feat branch %q did not reach bare remote; saw: %q", wc.Branch, branches)
	}
}

// TestDeliverApprenticeWork_PushSurfacesErr returns the git push error when
// no remote is configured.
func TestDeliverApprenticeWork_PushSurfacesErr(t *testing.T) {
	beadID := "spi-noremote"
	wc := setupApprenticeExitRepo(t, beadID)
	// No remote set on the repo — Push must fail.

	tower := &TowerConfig{
		Apprentice: config.ApprenticeConfig{Transport: config.ApprenticeTransportPush},
	}
	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return tower, nil },
	}
	err := deliverApprenticeWork(wc, beadID, 0, "", deps, noopLog)
	if err == nil {
		t.Fatal("expected error when no remote is configured")
	}
}

// TestDeliverApprenticeWork_UnknownTransport errors out rather than silently
// no-opping or defaulting to one of the branches.
func TestDeliverApprenticeWork_UnknownTransport(t *testing.T) {
	wc := setupApprenticeExitRepo(t, "spi-weird")
	tower := &TowerConfig{
		Apprentice: config.ApprenticeConfig{Transport: "warp-drive"},
	}
	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return tower, nil },
	}
	err := deliverApprenticeWork(wc, "spi-weird", 0, "", deps, noopLog)
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
	if !strings.Contains(err.Error(), "warp-drive") {
		t.Errorf("error should name the bad transport: %s", err.Error())
	}
}

// TestDeliverApprenticeWork_DefaultTransport verifies empty Transport resolves
// to bundle via EffectiveTransport — the key default-flip behavior.
func TestDeliverApprenticeWork_DefaultTransport(t *testing.T) {
	wc := setupApprenticeExitRepo(t, "spi-default")
	ls, _ := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: t.TempDir()})
	captured := swapSubmitFunc(t, nil)

	tower := &TowerConfig{} // empty transport -> bundle
	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return tower, nil },
		NewBundleStore:    func() (bundlestore.BundleStore, error) { return ls, nil },
	}
	if err := deliverApprenticeWork(wc, "spi-default", 0, "", deps, noopLog); err != nil {
		t.Fatalf("deliverApprenticeWork: %v", err)
	}
	if captured.BeadID != "spi-default" {
		t.Fatalf("empty transport should reach bundle branch; captured = %+v", captured)
	}
}

// TestDeliverApprenticeWork_NilTowerConfig falls through to the package
// default (bundle) when ActiveTowerConfig returns an error: Deps still needs
// NewBundleStore wired. This mirrors the production flow where the wizard
// proceeds with the default rather than failing the apprentice on a
// missing/corrupt tower config.
func TestDeliverApprenticeWork_NilTowerConfig(t *testing.T) {
	wc := setupApprenticeExitRepo(t, "spi-notower")
	ls, _ := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: t.TempDir()})
	captured := swapSubmitFunc(t, nil)

	deps := &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, errors.New("no tower") },
		NewBundleStore:    func() (bundlestore.BundleStore, error) { return ls, nil },
	}
	if err := deliverApprenticeWork(wc, "spi-notower", 0, "", deps, noopLog); err != nil {
		t.Fatalf("deliverApprenticeWork: %v", err)
	}
	if captured.BeadID != "spi-notower" {
		t.Fatalf("nil tower should default to bundle; captured = %+v", captured)
	}
}
